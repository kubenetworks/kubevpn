package action

import (
	"context"
	"os"
	"sync"

	log "github.com/sirupsen/logrus"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// SessionLifecycle manages the context hierarchy and cleanup functions for a
// daemon action session (Connect, Sync, etc.). It replaces the ad-hoc pattern
// of creating context.WithCancel + scattered cancel/cleanup calls.
type SessionLifecycle struct {
	// Ctx controls the overall session. Cancelling it tears down SSH tunnels,
	// port-forwards, and any goroutines bound to this session.
	Ctx    context.Context
	Cancel context.CancelFunc

	mu       sync.Mutex
	cleanups []func() error
	done     bool
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

// AddCleanup registers a function to be called during RunCleanups.
// Cleanups run in LIFO order (last registered = first executed).
func (s *SessionLifecycle) AddCleanup(f func() error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanups = append(s.cleanups, f)
}

// AddTempFile is a convenience for registering temp file removal.
func (s *SessionLifecycle) AddTempFile(path *string) {
	s.AddCleanup(func() error {
		if *path != "" {
			return os.Remove(*path)
		}
		return nil
	})
}

// RunCleanups executes all registered cleanup functions in reverse order,
// cancels the context, and detaches the logger. Safe to call multiple times.
func (s *SessionLifecycle) RunCleanups() {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return
	}
	s.done = true
	cleanups := make([]func() error, len(s.cleanups))
	copy(cleanups, s.cleanups)
	s.mu.Unlock()

	for i := len(cleanups) - 1; i >= 0; i-- {
		if err := cleanups[i](); err != nil {
			plog.G(s.Ctx).Warnf("cleanup error: %v", err)
		}
	}
	s.Cancel()
	plog.WithoutLogger(s.Ctx)
}
