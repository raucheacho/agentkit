package session

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/raucheacho/agentkit/store"
	"github.com/raucheacho/agentkit/llm"
)

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	s, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	return s
}

// TestStoreConcurrentDifferentKeys exercises many goroutines hitting the
// SQLite store for different conversations at once — this is what a real
// WhatsApp deployment does under load, and previously crashed the in-memory
// map (`fatal error: concurrent map writes`).
func TestStoreConcurrentDifferentKeys(t *testing.T) {
	s := newTestStore(t)

	var wg sync.WaitGroup
	const n = 100
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "whatsapp:" + string(rune('a'+i%26))
			if err := s.Save(key, []llm.Message{{Role: "user", Content: "hi"}}); err != nil {
				t.Errorf("Save(%s): %v", key, err)
			}
			if _, err := s.Get(key); err != nil {
				t.Errorf("Get(%s): %v", key, err)
			}
		}(i)
	}
	wg.Wait()
}

// TestStoreSaveAndGetRoundTrip checks that history survives a save/get cycle
// and is truncated to maxHistory.
func TestStoreSaveAndGetRoundTrip(t *testing.T) {
	s := newTestStore(t)

	var messages []llm.Message
	for i := 0; i < maxHistory+10; i++ {
		messages = append(messages, llm.Message{Role: "user", Content: "msg"})
	}

	if err := s.Save("whatsapp:123", messages); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := s.Get("whatsapp:123")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got) != maxHistory {
		t.Fatalf("expected history truncated to %d, got %d", maxHistory, len(got))
	}

	if _, err := s.Get("whatsapp:unknown"); err != nil {
		t.Fatalf("Get on unknown key should not error, got: %v", err)
	}
}
