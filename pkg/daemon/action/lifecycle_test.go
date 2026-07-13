package action

import (
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

func TestSessionLifecycle_Teardown_Idempotent(t *testing.T) {
	session := NewSessionLifecycle(log.New())

	// Teardown is only cancel + logger-detach now; both are idempotent, so
	// repeated calls must not panic and must leave the context cancelled.
	session.Teardown()
	session.Teardown()
	session.Teardown()

	if session.Ctx.Err() == nil {
		t.Fatal("Ctx should be cancelled after Teardown")
	}
}
