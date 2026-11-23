package vectix

import (
	"path/filepath"
	"testing"

	"github.com/raucheacho/agentkit/memory/notes/notestest"
	vx "github.com/raucheacho/vectixdb/bindings/go"
)

func TestContract(t *testing.T) {
	notestest.Run(t, func(t *testing.T) notestest.Store {
		db, err := vx.Open(filepath.Join(t.TempDir(), "vx"), nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		s, err := New(db, Config{Dim: 2})
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}
