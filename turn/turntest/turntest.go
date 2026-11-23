// Package turntest vérifie qu'une implémentation de turn.Queue respecte le
// contrat. Toute nouvelle implémentation (Postgres, Redis…) doit passer Run.
package turntest

import (
	"context"
	"testing"
	"time"

	"github.com/raucheacho/agentkit/turn"
)

// Run exécute le contrat sur des files neuves produites par open.
func Run(t *testing.T, open func(t *testing.T) turn.Queue) {
	ctx := context.Background()
	now := time.Now().UTC()

	t.Run("une session à la fois, sessions en parallèle", func(t *testing.T) {
		q := open(t)
		for _, x := range []*turn.Turn{
			{Session: "a", Content: "a1"}, {Session: "a", Content: "a2"}, {Session: "b", Content: "b1"},
		} {
			if err := q.Enqueue(ctx, x); err != nil {
				t.Fatal(err)
			}
		}
		got, err := q.Claim(ctx, now, 10)
		if err != nil || len(got) != 2 || got[0].Content != "a1" || got[1].Content != "b1" || got[0].Status != turn.Running {
			t.Fatalf("premier claim : %v %+v", err, got)
		}
		if again, _ := q.Claim(ctx, now, 10); len(again) != 0 {
			t.Fatalf("une session en cours ne doit rien céder : %+v", again)
		}
		if err := q.Finish(ctx, got[0].ID, turn.Done, "ok", ""); err != nil {
			t.Fatal(err)
		}
		if err := q.Finish(ctx, got[0].ID, turn.Done, "ok", ""); err == nil {
			t.Fatal("un tour terminé ne peut pas l'être deux fois")
		}
		next, _ := q.Claim(ctx, now, 10)
		if len(next) != 1 || next[0].Content != "a2" {
			t.Fatalf("suite : %+v", next)
		}
		done, _ := q.Get(ctx, got[0].ID)
		if done.Status != turn.Done || done.Reply != "ok" || done.FinishedAt.IsZero() {
			t.Fatalf("tour terminé : %+v", done)
		}
	})

	t.Run("max et report", func(t *testing.T) {
		q := open(t)
		_ = q.Enqueue(ctx, &turn.Turn{Session: "a"})
		_ = q.Enqueue(ctx, &turn.Turn{Session: "b"})
		_ = q.Enqueue(ctx, &turn.Turn{Session: "c", NotBefore: now.Add(time.Hour)})
		if got, _ := q.Claim(ctx, now, 1); len(got) != 1 {
			t.Fatalf("max : %d", len(got))
		}
		if got, _ := q.Claim(ctx, now, 10); len(got) != 1 || got[0].Session != "b" {
			t.Fatalf("report non respecté : %+v", got)
		}
		if got, _ := q.Claim(ctx, now.Add(2*time.Hour), 10); len(got) != 1 || got[0].Session != "c" {
			t.Fatalf("tour reporté non démarré : %+v", got)
		}
	})

	t.Run("steering", func(t *testing.T) {
		q := open(t)
		_ = q.Enqueue(ctx, &turn.Turn{Session: "s", Channel: "tg", Content: "1"})
		run, _ := q.Claim(ctx, now, 1)
		_ = q.Enqueue(ctx, &turn.Turn{Session: "s", Channel: "tg", Content: "2", Meta: map[string]string{"k": "v"}})
		_ = q.Enqueue(ctx, &turn.Turn{Session: "s", Channel: "web", Content: "autre canal"})
		_ = q.Enqueue(ctx, &turn.Turn{Session: "s", Channel: "tg", Source: turn.SourceSchedule, Content: "planifié"})
		_ = q.Enqueue(ctx, &turn.Turn{Session: "s", Channel: "tg", Content: "3"})
		taken, err := q.TakeQueued(ctx, "s", "tg", run[0].ID)
		if err != nil || len(taken) != 2 || taken[0].Content != "2" || taken[1].Content != "3" || taken[0].Meta["k"] != "v" {
			t.Fatalf("taken : %v %+v", err, taken)
		}
		m, _ := q.Get(ctx, taken[0].ID)
		if m.Status != turn.Merged || m.MergedInto != run[0].ID {
			t.Fatalf("absorbé : %+v", m)
		}
		_ = q.Finish(ctx, run[0].ID, turn.Done, "", "")
		rest, _ := q.Claim(ctx, now, 10)
		if len(rest) != 1 || rest[0].Content != "autre canal" {
			t.Fatalf("reste : %+v", rest)
		}
	})

	t.Run("annulation, liste et reprise après arrêt", func(t *testing.T) {
		q := open(t)
		a := &turn.Turn{Channel: "tg", ChatID: "1"}
		b := &turn.Turn{Session: "x"}
		_ = q.Enqueue(ctx, a)
		_ = q.Enqueue(ctx, b)
		if a.Session != "tg:1" || a.ID == "" {
			t.Fatalf("session déduite : %+v", a)
		}
		if ok, _ := q.Cancel(ctx, b.ID); !ok {
			t.Fatal("annulation refusée")
		}
		run, _ := q.Claim(ctx, now, 10)
		if len(run) != 1 || run[0].ID != a.ID {
			t.Fatalf("claim : %+v", run)
		}
		if ok, _ := q.Cancel(ctx, a.ID); ok {
			t.Fatal("un tour en cours ne s'annule pas par Cancel")
		}
		if n, _ := q.Recover(ctx); n != 1 {
			t.Fatalf("recover : %d", n)
		}
		got, _ := q.Get(ctx, a.ID)
		if got.Status != turn.Failed || got.Error != turn.ErrInterrupted {
			t.Fatalf("après recover : %+v", got)
		}
		list, _ := q.List(ctx, turn.Filter{Status: []turn.Status{turn.Cancelled, turn.Failed}})
		if len(list) != 2 || list[0].ID != b.ID {
			t.Fatalf("liste : %+v", list)
		}
		if g, _ := q.Get(ctx, "inconnu"); g != nil {
			t.Fatal("tour inconnu")
		}
	})
}
