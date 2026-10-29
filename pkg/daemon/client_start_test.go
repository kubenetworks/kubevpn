//go:build !windows

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

const startupTestEnv = "KUBEVPN_DAEMON_START_TEST"

type startupResult struct {
	Error    string
	Deadline bool
	Canceled bool
}

func TestMain(m *testing.M) {
	if os.Getenv(startupTestEnv) != "" {
		if err := runStartupHelper(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// The child serves real gRPC without SvrOption.Start's networking, persistence,
// watchdog and global logger side effects.
type startupServer struct {
	rpc.UnimplementedDaemonServer
}

func (startupServer) Upgrade(context.Context, *rpc.UpgradeRequest) (*rpc.UpgradeResponse, error) {
	return &rpc.UpgradeResponse{}, nil
}

func runStartupHelper() error {
	time.AfterFunc(45*time.Second, func() { os.Exit(2) })
	isSudo := os.Getenv("KUBEVPN_DAEMON_START_SUDO") == "1"
	mode := os.Getenv(startupTestEnv)
	if len(os.Args) > 1 && os.Args[1] == "daemon" {
		home := os.Getenv("HOME")
		if err := os.WriteFile(filepath.Join(home, "started"), nil, 0600); err != nil {
			return err
		}
		defer func() { _ = os.WriteFile(filepath.Join(home, "exited"), nil, 0600) }()
		fmt.Fprintln(os.Stdout, "daemon startup stdout")
		fmt.Fprintln(os.Stderr, "daemon startup stderr")
		if mode == "crash" {
			return fmt.Errorf("fatal synthetic daemon startup")
		}
		if mode == "no-pid" || mode == "cancel" {
			for {
				if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), "stop")); err == nil {
					return nil
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		if mode == "pid-error" {
			// Replace the parent directory after runDaemon has removed the stale PID.
			dir := filepath.Dir(config.GetPidPath(isSudo))
			if err := os.Rename(dir, dir+".saved"); err != nil {
				return err
			}
			return os.WriteFile(dir, []byte("not a directory"), 0600)
		}
		lis, err := net.Listen("unix", config.GetSockPath(isSudo))
		if err != nil {
			return err
		}
		defer lis.Close()
		s := grpc.NewServer()
		defer s.Stop()
		hs := health.NewServer()
		hs.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
		grpc_health_v1.RegisterHealthServer(s, hs)
		rpc.RegisterDaemonServer(s, startupServer{})
		go func() { _ = s.Serve(lis) }()
		if err := writePIDToFile(isSudo); err != nil {
			return err
		}
		for {
			if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), "stop")); err == nil {
				return nil
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if mode == "missing" {
		exe = filepath.Join(os.Getenv("HOME"), "missing-binary")
	}
	logPath := strings.TrimSuffix(config.GetDaemonLogPath(isSudo), ".log") + ".startup.log"
	if mode == "symlink" {
		target := filepath.Join(os.Getenv("HOME"), "untouched")
		if err := os.WriteFile(target, []byte("do not change"), 0400); err != nil {
			return err
		}
		if err := os.Symlink(target, logPath); err != nil {
			return err
		}
	}
	if mode == "readonly" {
		if err := os.WriteFile(logPath, []byte("old startup"), 0400); err != nil {
			return err
		}
	}
	if mode == "log-error" {
		if err := os.Mkdir(logPath, 0700); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	if mode == "success" || mode == "symlink" || mode == "readonly" {
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	if mode == "cancel" {
		go func() {
			// Cancel once the child has written stderr into the startup log.
			for ctx.Err() == nil {
				data, _ := os.ReadFile(logPath)
				if strings.Contains(string(data), "daemon startup stderr") {
					cancel()
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
	}
	err = runDaemon(ctx, exe, isSudo)
	result := startupResult{Deadline: errors.Is(err, context.DeadlineExceeded), Canceled: errors.Is(err, context.Canceled)}
	if err != nil {
		result.Error = err.Error()
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func TestDaemonStartupLifecycle(t *testing.T) {
	for _, mode := range []string{"success", "symlink", "readonly", "missing", "crash", "no-pid", "cancel", "pid-error", "log-error"} {
		t.Run(mode, func(t *testing.T) { testDaemonStartup(t, mode, false) })
	}
	if os.Getuid() == 0 {
		t.Run("already-root", func(t *testing.T) { testDaemonStartup(t, "success", true) })
	}
}

func testDaemonStartup(t *testing.T, mode string, isSudo bool) {
	t.Helper()
	home, err := os.MkdirTemp("", "kv-start-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	// Stop only the synthetic child owned by this test, after all assertions.
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(home, "stop"), nil, 0600)
		if _, err := os.Stat(filepath.Join(home, "started")); os.IsNotExist(err) {
			return
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(home, "exited")); err == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("synthetic daemon did not acknowledge shutdown")
	})
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), startupTestEnv+"="+mode, "HOME="+home, "KUBECONFIG="+filepath.Join(home, "no-kubeconfig"))
	role := "user"
	if isSudo {
		role = "root"
		cmd.Env = append(cmd.Env, "KUBEVPN_DAEMON_START_SUDO=1")
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	cmd.Stdout, cmd.Stderr = writer, writer
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("foreground failed: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("foreground did not exit")
	}
	if err := reader.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("output did not reach EOF while daemon lives: %v", err)
	}
	var result startupResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("foreground output contaminated: %q, %v", data, err)
	}
	logPath := filepath.Join(home, ".kubevpn", "log", role+"_daemon.startup.log")
	if mode == "success" || mode == "symlink" || mode == "readonly" {
		if result.Error != "" {
			t.Fatalf("startup failed: %s", result.Error)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		conn, err := grpc.NewClient("unix:"+filepath.Join(home, ".kubevpn", "daemon", role+"_daemon.sock"), grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		response, err := grpc_health_v1.NewHealthClient(conn).Check(ctx, &grpc_health_v1.HealthCheckRequest{})
		if err != nil || response.GetStatus() != grpc_health_v1.HealthCheckResponse_SERVING {
			t.Fatalf("daemon not serving after EOF: %v, %v", response, err)
		}
	} else {
		if result.Error == "" || !strings.Contains(result.Error, logPath) {
			t.Fatalf("startup error lacks diagnostic path: %+v", result)
		}
		if (mode == "crash" || mode == "no-pid") && !result.Deadline {
			t.Fatalf("deadline cause lost: %+v", result)
		}
		if mode == "cancel" && !result.Canceled {
			t.Fatalf("cancellation cause lost: %+v", result)
		}
	}
	if mode == "log-error" {
		entries, err := os.ReadDir(filepath.Dir(logPath))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != role+"_daemon.startup.log" {
			t.Fatalf("log setup leaked temporary files: %v", entries)
		}
		return
	}
	info, err := os.Lstat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatalf("startup log is not a private regular file: %v", info.Mode())
	}
	data, err = os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if mode != "missing" && (!strings.Contains(string(data), "daemon startup stdout") || !strings.Contains(string(data), "daemon startup stderr")) {
		t.Fatalf("missing startup output: %q", data)
	}
	if mode == "crash" && !strings.Contains(string(data), "fatal synthetic daemon startup") {
		t.Fatalf("lost fatal diagnostic: %q", data)
	}
	if mode == "symlink" {
		data, err = os.ReadFile(filepath.Join(home, "untouched"))
		if err != nil || string(data) != "do not change" {
			t.Fatalf("symlink target changed: %q, %v", data, err)
		}
	}
}
