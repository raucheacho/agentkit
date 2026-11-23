package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/raucheacho/agentkit/channel"
	_ "modernc.org/sqlite"
)

// Une reprise en main survit à un redémarrage de la passerelle.
func TestHoldsSurviveRestart(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h, err := NewHolds(ctx, db, "")
	if err != nil {
		t.Fatal(err)
	}
	gw := channel.NewGateway(channel.Config{Holds: h})
	if err := gw.Take(ctx, "tg:1"); err != nil {
		t.Fatal(err)
	}
	_ = gw.Take(ctx, "tg:2")
	_ = gw.Take(ctx, "tg:2")
	if err := gw.Release(ctx, "tg:2"); err != nil {
		t.Fatal(err)
	}

	again := channel.NewGateway(channel.Config{Holds: h})
	if err := again.LoadHolds(ctx); err != nil {
		t.Fatal(err)
	}
	if !again.Held("tg:1") || again.Held("tg:2") {
		t.Fatalf("après redémarrage : %v", again.HeldSessions())
	}
}
