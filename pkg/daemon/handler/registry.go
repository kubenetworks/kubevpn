package handler

import (
	"context"
	"sync"

	"golang.org/x/crypto/ssh"
)

// sessionRegistry provides thread-safe access to SSH sessions and their readiness signals.
var sessionRegistry = &registry{
	sessions: sync.Map{},
	ready:    sync.Map{},
}

type registry struct {
	sessions sync.Map // map[string]*ssh.Session
	ready    sync.Map // map[string]context.Context
}

func (r *registry) storeSession(id string, session *ssh.Session) {
	r.sessions.Store(id, session)
}

func (r *registry) loadSession(id string) (*ssh.Session, bool) {
	v, ok := r.sessions.Load(id)
	if !ok {
		return nil, false
	}
	return v.(*ssh.Session), true
}

func (r *registry) storeReady(id string, ctx context.Context) {
	r.ready.Store(id, ctx)
}

func (r *registry) loadReady(id string) (context.Context, bool) {
	v, ok := r.ready.Load(id)
	if !ok {
		return nil, false
	}
	return v.(context.Context), true
}

func (r *registry) cleanup(id string) {
	r.sessions.Delete(id)
	r.ready.Delete(id)
}
