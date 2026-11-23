package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/raucheacho/agentkit/observe"
	"github.com/raucheacho/agentkit/observe/observetest"
	_ "modernc.org/sqlite"
)

func TestContract(t *testing.T) {
	observetest.Run(t, func(t *testing.T) observe.Seen {
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "o.db"))
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
