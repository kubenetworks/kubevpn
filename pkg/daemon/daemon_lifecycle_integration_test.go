//go:build !windows

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

func TestIntegration_DaemonInstanceLockAndCrashRecovery(t *testing.T) {
	home := newDaemonTestHome(t)
	sockPath := filepath.Join(home, ".kubevpn", "daemon", config.SockPath)
	pidPath := filepath.Join(home, ".kubevpn", "daemon", config.PidPath)
	start := func(id string) (*exec.Cmd, *os.File) {
		t.Helper()
		cmd := exec.Command(os.Args[0])
		cmd.Env = daemonHelperEnv(home, "instance")
		cmd.Env = append(cmd.Env, "KUBEVPN_DAEMON_INSTANCE_ID="+id)
		logFile, err := os.Create(filepath.Join(home, id+".log"))
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stdout, cmd.Stderr = logFile, logFile
		if err = cmd.Start(); err != nil {
			_ = logFile.Close()
			t.Fatalf("start %s daemon: %v", id, err)
		}
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			_ = logFile.Close()
		})
		return cmd, logFile
	}

	first, _ := start("first")
	waitForDaemonIdentity(t, sockPath, "first")
	pidBefore, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}

	probeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	second := exec.CommandContext(probeCtx, os.Args[0])
	second.Env = daemonHelperEnv(home, "instance")
	second.Env = append(second.Env, "KUBEVPN_DAEMON_INSTANCE_ID=second")
	output, secondErr := second.CombinedOutput()
	if secondErr == nil {
		t.Fatalf("second daemon unexpectedly acquired the instance: %s", output)
	}
	if !strings.Contains(string(output), errDaemonAlreadyRunning.Error()) {
		t.Fatalf("second daemon error = %q, want already-running error", output)
	}
	waitForDaemonIdentity(t, sockPath, "first")
	pidAfter, err := os.ReadFile(pidPath)
	if err != nil || !bytes.Equal(pidBefore, pidAfter) {
		t.Fatalf("second daemon changed PID file: before=%q after=%q err=%v", pidBefore, pidAfter, err)
	}

	// A crashed holder releases its OS lock; the next process reclaims the stale
	// socket and publishes a new identity without manual cleanup.
	if err = first.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = first.Process.Wait()
	if err = os.Remove(pidPath); err != nil {
		t.Fatal(err)
	}
	start("third")
	waitForDaemonIdentity(t, sockPath, "third")
}

func TestIntegration_ConcurrentStartupLaunchesOnce(t *testing.T) {
	home := newDaemonTestHome(t)
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(home, "stop"), nil, 0600)
	})
	const processCount = 4
	type foreground struct {
		cmd    *exec.Cmd
		output *bytes.Buffer
	}
	processes := make([]foreground, 0, processCount)
	for i := 0; i < processCount; i++ {
		cmd := exec.Command(os.Args[0])
		cmd.Env = daemonHelperEnv(home, "success")
		output := &bytes.Buffer{}
		cmd.Stdout, cmd.Stderr = output, output
		if err := cmd.Start(); err != nil {
			t.Fatalf("start foreground %d: %v", i, err)
		}
		processes = append(processes, foreground{cmd: cmd, output: output})
	}
	for i, process := range processes {
		done := make(chan error, 1)
		go func() { done <- process.cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("foreground %d failed: %v\n%s", i, err, process.output)
			}
		case <-time.After(10 * time.Second):
			_ = process.cmd.Process.Kill()
			t.Fatalf("foreground %d did not exit", i)
		}
		var result startupResult
		if err := json.Unmarshal(process.output.Bytes(), &result); err != nil {
			t.Fatalf("foreground %d output is not JSON: %q: %v", i, process.output, err)
		}
		if result.Error != "" {
			t.Fatalf("foreground %d startup error: %s", i, result.Error)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "stop"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, filepath.Join(home, "exited"))
	data, err := os.ReadFile(filepath.Join(home, "started"))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(strings.Fields(string(data))); got != 1 {
		t.Fatalf("actual daemon launches = %d, want 1; records=%q", got, data)
	}
}

func TestIntegration_DaemonProbeHasDeadline(t *testing.T) {
	home := newDaemonTestHome(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0])
	cmd.Env = daemonHelperEnv(home, "unresponsive")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("probe helper failed: %v\n%s", err, output)
	}
	var result startupResult
	if err = json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode probe result %q: %v", output, err)
	}
	if !result.Deadline {
		t.Fatalf("unresponsive health probe did not hit its deadline: %+v", result)
	}
}

func TestIntegration_DaemonFailureEvidencePreservesState(t *testing.T) {
	for _, mode := range []string{"reject-timeout", "live-probe-error", "startup-lock-timeout"} {
		t.Run(mode, func(t *testing.T) {
			home := newDaemonTestHome(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0])
			cmd.Env = daemonHelperEnv(home, mode)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("failure-evidence helper failed: %v\n%s", err, output)
			}
		})
	}
}

func TestDaemonStopBeforeStartDoesNotPanic(t *testing.T) {
	(&SvrOption{}).Stop()
}

func newDaemonTestHome(t *testing.T) string {
	t.Helper()
	// Unix socket paths are short on macOS/BSD; test-name-based TempDir paths
	// can exceed the platform sockaddr_un limit.
	home, err := os.MkdirTemp("", "kv-life-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	return home
}

func daemonHelperEnv(home, mode string) []string {
	return append(os.Environ(),
		startupTestEnv+"="+mode,
		"HOME="+home,
		"KUBECONFIG="+filepath.Join(home, "no-kubeconfig"),
		"ALL_PROXY=",
		"all_proxy=",
	)
}

func waitForDaemonIdentity(t *testing.T, sockPath, want string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := grpc.NewClient("unix:"+sockPath, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithNoProxy())
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			var resp *rpc.IdentifyResponse
			resp, err = rpc.NewDaemonClient(conn).Identify(ctx, &rpc.IdentifyRequest{})
			cancel()
			_ = conn.Close()
			if err == nil && resp.GetID() == want {
				return
			}
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("daemon at %s did not publish identity %q: %v", sockPath, want, lastErr)
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("file %s did not appear", path)
}
