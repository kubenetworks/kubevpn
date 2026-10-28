//go:build !windows

package elevate

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const processTestRole = "KUBEVPN_ELEVATE_TEST_ROLE"

// TestMain lets the test executable model foreground, daemon and sudo monitor
// processes without invoking real sudo or any kubevpn daemon entrypoint.
func TestMain(m *testing.M) {
	if role := os.Getenv(processTestRole); role != "" {
		if err := runProcessHelper(role); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runProcessHelper(role string) error {
	// A failed assertion must never leave a long-lived test daemon behind.
	time.AfterFunc(30*time.Second, func() { os.Exit(2) })
	dir := os.Getenv("KUBEVPN_ELEVATE_TEST_DIR")
	exe := os.Getenv("KUBEVPN_ELEVATE_TEST_EXE")
	if role == "launcher" && len(os.Args) > 1 && os.Args[1] == "daemon argument" {
		return runChildHelper(dir)
	}
	if role == "launcher" && filepath.Base(os.Args[0]) == "sudo" {
		if len(os.Args) < 4 || os.Args[1] != "--preserve-env=HOME" || os.Args[2] != "--background" {
			return fmt.Errorf("unexpected sudo arguments: %q", os.Args)
		}
		switch os.Getenv("KUBEVPN_ELEVATE_TEST_AUTH") {
		case "reject":
			return fmt.Errorf("sudo: test authentication rejected")
		case "tty":
			tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
			if err != nil {
				return fmt.Errorf("sudo: terminal required: %w", err)
			}
			defer tty.Close()
			if _, err := fmt.Fprint(tty, "test password: "); err != nil {
				return err
			}
			var password string
			if _, err := fmt.Fscanln(tty, &password); err != nil {
				return err
			}
			if password != "test-password" {
				return fmt.Errorf("sudo: test authentication rejected")
			}
		}
		cmd := exec.Command(exe, os.Args[3:]...)
		// Model sudo's background monitor, outside the terminal's foreground
		// group so it survives the foreground session leader's exit.
		cmd.SysProcAttr = &unix.SysProcAttr{Setpgid: true}
		cmd.Env = append(os.Environ(), processTestRole+"=monitor")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	}
	switch role {
	case "launcher":
		args := []string{"daemon argument", "--value=a b"}
		if os.Getenv("KUBEVPN_ELEVATE_TEST_MISSING") != "" {
			exe = filepath.Join(dir, "missing-executable")
		}
		output, err := os.Create(filepath.Join(dir, "startup.log"))
		if err != nil {
			return err
		}
		defer output.Close()
		if os.Getenv("KUBEVPN_ELEVATE_TEST_NULL") == "1" {
			output = nil
		}
		if os.Getenv("KUBEVPN_ELEVATE_TEST_SUDO") == "1" {
			return RunCmdWithElevated(exe, args, output)
		}
		return RunCmd(exe, args, output)
	case "monitor":
		if len(os.Args) < 2 {
			return fmt.Errorf("missing target")
		}
		cmd := exec.Command(os.Args[1], os.Args[2:]...)
		cmd.Env = append(os.Environ(), processTestRole+"=child")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Start(); err != nil {
			return err
		}
		defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
		return serveProcessHelper(dir, "monitor")
	case "child":
		return runChildHelper(dir)
	default:
		return fmt.Errorf("unexpected helper role %q", role)
	}
}

func runChildHelper(dir string) error {
	fmt.Fprintln(os.Stdout, "child stdout")
	fmt.Fprintln(os.Stderr, "child stderr")
	data, err := json.Marshal([]string{os.Getenv("HOME"), os.Getenv(EnvDisableSyncthingLog), strings.Join(os.Args[1:], "|")})
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(dir, "child.json"), data, 0600); err != nil {
		return err
	}
	go func() {
		var b [1]byte
		if n, err := os.Stdin.Read(b[:]); n == 0 && err == io.EOF {
			_ = os.WriteFile(filepath.Join(dir, "stdin-eof"), nil, 0600)
		}
	}()
	return serveProcessHelper(dir, "child")
}

func serveProcessHelper(dir, name string) error {
	lis, err := net.Listen("unix", filepath.Join(dir, name+".sock"))
	if err != nil {
		return err
	}
	defer lis.Close()
	for {
		conn, err := lis.Accept()
		if err != nil {
			return err
		}
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		var b [1]byte
		_, err = conn.Read(b[:])
		if err == nil {
			_, _ = conn.Write([]byte("alive"))
		}
		_ = conn.Close()
		if b[0] == 'q' {
			return nil
		}
	}
}

type processHarness struct {
	dir    string
	cmd    *exec.Cmd
	done   chan error
	output []*os.File
}

func newProcessHarness(t *testing.T, sudo, merged bool, auth string) *processHarness {
	t.Helper()
	dir, err := os.MkdirTemp("", "kv-elevate-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(exe, filepath.Join(dir, "sudo")); err != nil {
		t.Fatal(err)
	}
	h := &processHarness{dir: dir, cmd: exec.Command(exe), done: make(chan error, 1)}
	h.cmd.Env = append(os.Environ(), processTestRole+"=launcher", "KUBEVPN_ELEVATE_TEST_EXE="+exe,
		"KUBEVPN_ELEVATE_TEST_DIR="+dir, "KUBEVPN_ELEVATE_TEST_AUTH="+auth, "HOME="+dir, "PATH="+dir)
	if sudo {
		h.cmd.Env = append(h.cmd.Env, "KUBEVPN_ELEVATE_TEST_SUDO=1")
	}
	stdin, input, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	h.cmd.Stdin = stdin
	t.Cleanup(func() { _ = stdin.Close(); _ = input.Close() })
	for i := 0; i < 2; i++ {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		h.output = append(h.output, r)
		t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
		if i == 0 {
			h.cmd.Stdout = w
			if merged {
				h.cmd.Stderr = w
				break
			}
		} else {
			h.cmd.Stderr = w
		}
	}
	return h
}

func (h *processHarness) start(t *testing.T) {
	t.Helper()
	if err := h.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = h.cmd.Stdin.(*os.File).Close()
	_ = h.cmd.Stdout.(*os.File).Close()
	_ = h.cmd.Stderr.(*os.File).Close()
	go func() { h.done <- h.cmd.Wait() }()
	t.Cleanup(func() {
		for _, name := range []string{"child", "monitor"} {
			_, _ = probeProcess(h.dir, name, 'q')
		}
		_ = h.cmd.Process.Kill()
	})
}

func (h *processHarness) wait(t *testing.T, wantError bool) {
	t.Helper()
	select {
	case err := <-h.done:
		if (err != nil) != wantError {
			t.Fatalf("foreground exit: %v, want error %v", err, wantError)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("foreground launcher did not exit")
	}
}

func probeProcess(dir, name string, request byte) (string, error) {
	conn, err := net.DialTimeout("unix", filepath.Join(dir, name+".sock"), 100*time.Millisecond)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err = conn.Write([]byte{request}); err != nil {
		return "", err
	}
	b, err := io.ReadAll(conn)
	return string(b), err
}

func waitProcessCondition(t *testing.T, description string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func (h *processHarness) assertDetached(t *testing.T, sudo bool) {
	t.Helper()
	waitProcessCondition(t, "child readiness", func() bool {
		s, err := probeProcess(h.dir, "child", 'p')
		return err == nil && s == "alive"
	})
	for _, pipe := range h.output {
		if err := pipe.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(pipe)
		if err != nil {
			t.Fatalf("caller pipe did not reach EOF while background processes remain alive: %v (output %q)", err, data)
		}
		if len(data) != 0 {
			t.Fatalf("background output leaked to caller: %q", data)
		}
	}
	for _, name := range []string{"child", "monitor"} {
		if name == "monitor" && !sudo {
			continue
		}
		if s, err := probeProcess(h.dir, name, 'p'); err != nil || s != "alive" {
			t.Fatalf("%s not alive after EOF: %q, %v", name, s, err)
		}
	}
	waitProcessCondition(t, "detached stdin", func() bool {
		_, err := os.Stat(filepath.Join(h.dir, "stdin-eof"))
		return err == nil
	})
	data, err := os.ReadFile(filepath.Join(h.dir, "child.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	if err = json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	want := []string{h.dir, "1", "daemon argument|--value=a b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("child environment/arguments = %q, want %q", got, want)
	}
	data, err = os.ReadFile(filepath.Join(h.dir, "startup.log"))
	wantOutput := "child stdout\nchild stderr\n"
	for _, env := range h.cmd.Env {
		if env == "KUBEVPN_ELEVATE_TEST_NULL=1" {
			wantOutput = ""
		}
	}
	if err != nil || string(data) != wantOutput {
		t.Fatalf("detached output = %q, %v", data, err)
	}
}

func TestDetachedLaunchPipeEOF(t *testing.T) {
	for _, sudo := range []bool{false, true} {
		for _, merged := range []bool{false, true} {
			t.Run(fmt.Sprintf("sudo=%v/merged=%v", sudo, merged), func(t *testing.T) {
				h := newProcessHarness(t, sudo, merged, "")
				h.start(t)
				h.wait(t, false)
				h.assertDetached(t, sudo)
			})
		}
	}
	for _, sudo := range []bool{false, true} {
		t.Run(fmt.Sprintf("null/sudo=%v", sudo), func(t *testing.T) {
			h := newProcessHarness(t, sudo, true, "")
			h.cmd.Env = append(h.cmd.Env, "KUBEVPN_ELEVATE_TEST_NULL=1")
			h.start(t)
			h.wait(t, false)
			h.assertDetached(t, sudo)
		})
	}
}

func TestDetachedLaunchErrors(t *testing.T) {
	for _, tc := range []struct{ name, auth, missing string }{
		{"authentication", "reject", ""},
		{"no-terminal", "tty", ""},
		{"missing-executable", "", "1"},
		{"missing-sudo", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newProcessHarness(t, tc.missing == "", true, tc.auth)
			h.cmd.SysProcAttr = &unix.SysProcAttr{Setsid: true}
			h.cmd.Env = append(h.cmd.Env, "KUBEVPN_ELEVATE_TEST_MISSING="+tc.missing)
			if tc.name == "missing-sudo" {
				if err := os.Remove(filepath.Join(h.dir, "sudo")); err != nil {
					t.Fatal(err)
				}
			}
			h.start(t)
			h.wait(t, true)
			if _, err := os.Stat(filepath.Join(h.dir, "child.json")); !os.IsNotExist(err) {
				t.Fatalf("failed launch created child: %v", err)
			}
			if tc.auth != "" {
				data, err := os.ReadFile(filepath.Join(h.dir, "startup.log"))
				if err != nil || !strings.Contains(string(data), "sudo:") {
					t.Fatalf("sudo diagnostic missing: %q, %v", data, err)
				}
			}
		})
	}
}
