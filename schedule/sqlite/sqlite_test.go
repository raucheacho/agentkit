package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/raucheacho/agentkit/schedule"
	"github.com/raucheacho/agentkit/schedule/scheduletest"
	_ "modernc.org/sqlite"
)

func TestContract(t *testing.T) {
	scheduletest.Run(t, func(t *testing.T) schedule.Store {
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "s.db")+"?_pragma=busy_timeout(5000)")
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
