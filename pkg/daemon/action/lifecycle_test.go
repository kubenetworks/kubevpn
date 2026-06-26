package action

import (
	"os"
	"sync/atomic"
	"testing"

	log "github.com/sirupsen/logrus"
)

func TestNewSessionLifecycle(t *testing.T) {
	session := NewSessionLifecycle(log.New())
	if session.Ctx == nil {
		t.Fatal("Ctx is nil")
	}
	if session.Cancel == nil {
		t.Fatal("Cancel is nil")
	}
	if session.Ctx.Err() != nil {
		t.Fatal("Ctx already cancelled")
	}
}

func TestSessionLifecycle_Cancel(t *testing.T) {
	session := NewSessionLifecycle(log.New())
	session.Cancel()
	if session.Ctx.Err() == nil {
		t.Fatal("Ctx should be cancelled after Cancel()")
	}
}

func TestSessionLifecycle_AddCleanup_LIFO(t *testing.T) {
	session := NewSessionLifecycle(log.New())
	var order []int
	session.AddCleanup(func() error { order = append(order, 1); return nil })
	session.AddCleanup(func() error { order = append(order, 2); return nil })
	session.AddCleanup(func() error { order = append(order, 3); return nil })

	session.RunCleanups()

	if len(order) != 3 {
		t.Fatalf("expected 3 cleanups, got %d", len(order))
	}
	if order[0] != 3 || order[1] != 2 || order[2] != 1 {
		t.Fatalf("expected LIFO [3,2,1], got %v", order)
	}
}

func TestSessionLifecycle_RunCleanups_Idempotent(t *testing.T) {
	session := NewSessionLifecycle(log.New())
	var count atomic.Int32
	session.AddCleanup(func() error { count.Add(1); return nil })

	session.RunCleanups()
	session.RunCleanups()
	session.RunCleanups()

	if count.Load() != 1 {
		t.Fatalf("expected cleanup called once, got %d", count.Load())
	}
}

func TestSessionLifecycle_AddTempFile(t *testing.T) {
	f, err := os.CreateTemp("", "lifecycle-test-*")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()

	session := NewSessionLifecycle(log.New())
	session.AddTempFile(&path)
	session.RunCleanups()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("temp file should be deleted, but stat: %v", err)
	}
}

func TestSessionLifecycle_CancelledAfterRunCleanups(t *testing.T) {
	session := NewSessionLifecycle(log.New())
	session.RunCleanups()
	if session.Ctx.Err() == nil {
		t.Fatal("Ctx should be cancelled after RunCleanups")
	}
}
