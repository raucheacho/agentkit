package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/raucheacho/agentkit/turn"
	"github.com/raucheacho/agentkit/turn/turntest"
	_ "modernc.org/sqlite"
)

func TestSQLiteQueue(t *testing.T) {
	turntest.Run(t, func(t *testing.T) turn.Queue {
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "t.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		s, err := New(context.Background(), db, "")
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}
