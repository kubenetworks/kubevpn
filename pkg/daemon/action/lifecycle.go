package action

import (
	"context"

	log "github.com/sirupsen/logrus"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// SessionLifecycle owns the context for a daemon action session (Connect, Sync,
// etc.) whose lifetime outlives the originating gRPC request, together with its
// teardown. It replaces the ad-hoc pattern of creating context.WithCancel +
// scattered cancel/logger-detach calls.
type SessionLifecycle struct {
	// Ctx controls the overall session. Cancelling it tears down SSH tunnels,
	// port-forwards, and any goroutines bound to this session.
	Ctx    context.Context
	Cancel context.CancelFunc
}

// NewSessionLifecycle creates a session with a cancellable context derived from
// context.Background() (not from the gRPC request — sessions outlive RPCs).
// The logger is attached to the context for structured logging.
func NewSessionLifecycle(logger *log.Logger) *SessionLifecycle {
	ctx, cancel := context.WithCancel(context.Background())
	ctx = plog.WithLogger(ctx, logger)
	return &SessionLifecycle{
		Ctx:    ctx,
		Cancel: cancel,
	}
}

// Teardown cancels the session context (closing SSH tunnels, port-forwards, and
// session-bound goroutines) and detaches the stream logger so post-teardown logs
// fall back to the default logger. Idempotent: context cancellation and logger
// detach are both safe to repeat.
func (s *SessionLifecycle) Teardown() {
	s.Cancel()
	plog.WithoutLogger(s.Ctx)
}
