package observe

import (
	"context"
	"sync"
	"time"
)

// Seen conserve les clés des événements déjà traités.
type Seen interface {
	// Seen renvoie les clés déjà vues parmi keys.
	Seen(ctx context.Context, keys []string) (map[string]bool, error)
	// Mark enregistre des clés comme vues.
	Mark(ctx context.Context, keys []string, at time.Time) error
	// Forget oublie les clés vues avant before.
	Forget(ctx context.Context, before time.Time) (int, error)
}

// MemorySeen est un Seen en mémoire.
type MemorySeen struct {
	mu   sync.Mutex
	keys map[string]time.Time
}

// NewMemorySeen crée un Seen en mémoire.
func NewMemorySeen() *MemorySeen { return &MemorySeen{keys: map[string]time.Time{}} }

func (m *MemorySeen) Seen(_ context.Context, keys []string) (map[string]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]bool{}
	for _, k := range keys {
		if _, ok := m.keys[k]; ok {
			out[k] = true
		}
	}
	return out, nil
}

func (m *MemorySeen) Mark(_ context.Context, keys []string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range keys {
		m.keys[k] = at
	}
	return nil
}

func (m *MemorySeen) Forget(_ context.Context, before time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k, at := range m.keys {
		if at.Before(before) {
			delete(m.keys, k)
			n++
		}
	}
	return n, nil
}

var _ Seen = (*MemorySeen)(nil)
