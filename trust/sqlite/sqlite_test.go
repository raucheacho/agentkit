package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/raucheacho/agentkit/trust"
	"github.com/raucheacho/agentkit/trust/trusttest"
	_ "modernc.org/sqlite"
)

func TestSQLiteStore(t *testing.T) {
	trusttest.Run(t, func(t *testing.T) trust.Store {
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "t.db")+"?_pragma=busy_timeout(5000)")
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
