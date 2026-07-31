//go:build integration

package handler

import (
	"bufio"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// startExited starts cmd, waits until it prints "ready" on stdout (so any signal traps are
// installed before we signal it — otherwise a SIGINT racing shell startup would kill it via
// the default disposition), and returns a channel closed once it exits. Mirrors how
// waitRunStartup exposes process exit to stopRunCmd, without needing a real
// `kubevpn run`/cluster, so the stop sequence can be tested deterministically.
func startExited(t *testing.T, cmd *exec.Cmd) <-chan struct{} {
	t.Helper()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_ = cmd.Wait()
	}()

	ready := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), "ready") {
				close(ready)
				return
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("child never reported ready")
	}
	return exited
}

// TestTryStopRun_GracefulOnSIGINT: a child that exits on SIGINT is stopped cleanly and
// quickly, without reaching the SIGQUIT/SIGKILL escalation (killed=false).
func TestTryStopRun_GracefulOnSIGINT(t *testing.T) {
	defer withShortStopTimeouts()()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Default SIGINT disposition terminates sh; echo ready once it is looping.
	cmd := exec.CommandContext(ctx, "sh", "-c", "echo ready; sleep 60")
	exited := startExited(t, cmd)

	start := time.Now()
	killed, err := tryStopRun(cmd, cancel, exited)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("tryStopRun returned error: %v", err)
	}
	if killed {
		t.Fatalf("expected graceful stop on SIGINT, but process had to be force-killed")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("graceful stop took %s, expected near-instant", elapsed)
	}
	assertClosed(t, exited, "process should have exited")
}

// TestTryStopRun_ForceKillsWedged: a child that ignores SIGINT and SIGQUIT (a wedged
// teardown) is still force-terminated via the SIGKILL backstop within a bounded time —
// this is the guarantee that replaces the old unbounded `for cmd.ProcessState == nil {}`
// busy-wait (which turned a transient cluster hiccup into a 2h test hang).
func TestTryStopRun_ForceKillsWedged(t *testing.T) {
	defer withShortStopTimeouts()()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Ignore INT and QUIT so only SIGKILL (via cancel → CommandContext) can stop it.
	// echo ready AFTER the traps are installed so startExited waits out the race.
	cmd := exec.CommandContext(ctx, "sh", "-c", "trap '' INT QUIT; echo ready; while :; do sleep 1; done")
	exited := startExited(t, cmd)

	done := make(chan struct{})
	var killed bool
	var err error
	go func() {
		defer close(done)
		killed, err = tryStopRun(cmd, cancel, exited)
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("tryStopRun did not return within 15s — the stop sequence is not bounded")
	}

	if err != nil {
		t.Fatalf("tryStopRun returned error: %v", err)
	}
	if !killed {
		t.Fatalf("expected killed=true for a SIGINT/SIGQUIT-ignoring process")
	}
	assertClosed(t, exited, "process should have been killed and exited")
}

// withShortStopTimeouts shrinks the stop timeouts for the duration of a test and returns a
// restore func (call via defer).
func withShortStopTimeouts() func() {
	origTimeout, origGrace := runStopTimeout, runStopQuitGrace
	runStopTimeout = 300 * time.Millisecond
	runStopQuitGrace = 300 * time.Millisecond
	return func() {
		runStopTimeout, runStopQuitGrace = origTimeout, origGrace
	}
}

func assertClosed(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	default:
		t.Fatal(msg)
	}
}
