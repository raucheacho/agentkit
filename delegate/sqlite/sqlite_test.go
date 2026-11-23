package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/raucheacho/agentkit/delegate"
	"github.com/raucheacho/agentkit/delegate/delegatetest"
	_ "modernc.org/sqlite"
)

func TestSQLiteStore(t *testing.T) {
	delegatetest.Run(t, func(t *testing.T) delegate.Store {
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "d.db")+"?_pragma=busy_timeout(5000)")
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
