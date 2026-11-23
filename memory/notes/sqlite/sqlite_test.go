package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/raucheacho/agentkit/memory/notes/notestest"
	_ "modernc.org/sqlite"
)

func TestContract(t *testing.T) {
	notestest.Run(t, func(t *testing.T) notestest.Store {
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "n.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		s, err := New(context.Background(), db, "")
		if err != nil {
			t.Fatal(err)
		}
		if !s.hasFTS {
			t.Fatal("FTS5 attendu avec modernc.org/sqlite")
		}
		return s
	})
}
