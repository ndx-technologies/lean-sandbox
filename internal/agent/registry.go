package agent

import (
	"errors"
	"sync"

	"github.com/ndx-technologies/lean-sandbox/api"
)

// ErrSessionExists is returned when a second session is created on an agent.
// One pod = one sandbox = one session: isolation is the pod boundary, so a pod
// must never host two sessions that could touch each other's files.
var ErrSessionExists = errors.New("session already exists on this sandbox")

// Registry is a concurrency-safe map of sessions by ID. It enforces a maximum
// of one session per agent so that a pod is never shared between tasks.
type Registry struct {
	mu       sync.RWMutex
	sessions map[api.SessionID]*Session
}

func NewRegistry() *Registry { return &Registry{sessions: map[api.SessionID]*Session{}} }

func (r *Registry) Put(id api.SessionID, s *Session) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.sessions) > 0 {
		return ErrSessionExists
	}
	r.sessions[id] = s
	return nil
}

func (r *Registry) Get(id api.SessionID) (*Session, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.sessions[id]
	return s, ok
}

func (r *Registry) Delete(id api.SessionID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, id)
}
