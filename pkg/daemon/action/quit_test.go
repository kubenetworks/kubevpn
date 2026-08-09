package action

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc"
	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

// fakeQuitServer is a minimal rpc.Daemon_QuitServer that captures the messages
// the Quit handler streams back to the CLI. The embedded grpc.ServerStream
// satisfies the unused interface methods (and stays nil — they are never called).
type fakeQuitServer struct {
	grpc.ServerStream
	ctx  context.Context
	sent []string
}

func (f *fakeQuitServer) Send(r *rpc.QuitResponse) error {
	f.sent = append(f.sent, r.GetMessage())
	return nil
}

func (f *fakeQuitServer) Recv() (*rpc.QuitRequest, error) { return &rpc.QuitRequest{}, nil }

func (f *fakeQuitServer) Context() context.Context { return f.ctx }

// preserveDBFile backs up and restores config.GetDBPath() around a test, since
// Quit's deferred CleanupConfig removes it. Mirrors persistence_test.go.
func preserveDBFile(t *testing.T) {
	t.Helper()
	dbPath := config.GetDBPath()
	origData, origErr := os.ReadFile(dbPath)
	t.Cleanup(func() {
		if origErr == nil {
			_ = os.WriteFile(dbPath, origData, 0644)
		} else {
			_ = os.Remove(dbPath)
		}
	})
}

// TestQuit_StepEmittedOnlyByUserDaemon locks the dual-daemon dedup fix: the user
// daemon (orchestrator) renders the "Cleaned up N connections" step exactly once,
// while the sudo daemon cleans up its data-plane connections silently. Both
// daemons must still actually perform the cleanup.
func TestQuit_StepEmittedOnlyByUserDaemon(t *testing.T) {
	logFile := &lumberjack.Logger{Filename: filepath.Join(t.TempDir(), "daemon.log")}
	// lumberjack opens the file lazily on first write and keeps the handle open.
	// Close it before t.TempDir's RemoveAll cleanup runs, otherwise Windows
	// refuses to delete the still-open daemon.log and the test fails in cleanup.
	defer func() { _ = logFile.Close() }()

	run := func(t *testing.T, isSudo bool) (stream string, cleanupRan bool) {
		preserveDBFile(t)

		var ran atomic.Bool
		conn := &handler.ConnectOptions{}
		conn.AddRollbackFunc(func() error {
			ran.Store(true)
			return nil
		})

		svr := &Server{
			IsSudo:      isSudo,
			LogFile:     logFile,
			connections: []handler.Connection{conn},
		}
		resp := &fakeQuitServer{ctx: context.Background()}

		if err := svr.Quit(resp); err != nil {
			t.Fatalf("Quit returned error: %v", err)
		}
		return strings.Join(resp.sent, "\n"), ran.Load()
	}

	t.Run("user daemon emits the step", func(t *testing.T) {
		stream, cleanupRan := run(t, false)
		if !strings.Contains(stream, "Cleaned up 1 connections") {
			t.Fatalf("user daemon should emit the count step, got stream: %q", stream)
		}
		if !cleanupRan {
			t.Fatal("user daemon should still run connection cleanup")
		}
	})

	t.Run("sudo daemon stays silent but still cleans up", func(t *testing.T) {
		stream, cleanupRan := run(t, true)
		if strings.Contains(stream, "Cleaned up") || strings.Contains(stream, "Cleaning up connections") {
			t.Fatalf("sudo daemon must not emit the count step, got stream: %q", stream)
		}
		if !cleanupRan {
			t.Fatal("sudo daemon should still run connection cleanup silently")
		}
	})
}
