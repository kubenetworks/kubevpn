package handler

import "sync"

// rollbackList is a mutex-protected FIFO registry of teardown functions, shared by
// the session types (embedded in SessionBase and SyncOptions). Extracted so both
// use one thread-safe implementation instead of hand-rolled, sometimes-unlocked
// copies. Run the snapshot through executeRollbackFuncs.
type rollbackList struct {
	mu    sync.Mutex
	funcs []func() error
}

// AddRollbackFunc registers a cleanup function to be called when the connection is torn down.
func (r *rollbackList) AddRollbackFunc(f func() error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.funcs = append(r.funcs, f)
}

// getRollbackFuncs returns a snapshot of all registered rollback functions.
func (r *rollbackList) getRollbackFuncs() []func() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	fns := make([]func() error, len(r.funcs))
	copy(fns, r.funcs)
	return fns
}
