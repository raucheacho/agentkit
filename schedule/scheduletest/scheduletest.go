// Package scheduletest vérifie qu'une implémentation de schedule.Store respecte le contrat.
package scheduletest

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raucheacho/agentkit/schedule"
	"github.com/raucheacho/agentkit/tool"
)

// Run exécute le contrat sur des Store neufs produits par open.
func Run(t *testing.T, open func(t *testing.T) schedule.Store) {
	ctx := context.Background()
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	conv := tool.Conversation{Session: "s1", Channel: "tg", ChatID: "1", Sender: "ana"}
	job := func(next time.Time, session string) *schedule.Job {
		c := conv
		c.Session = session
		return &schedule.Job{Name: "n", Spec: "every 1h", Task: "t", Conversation: c, Enabled: true, NextRun: next, Silent: true}
	}

	t.Run("création, lecture, liste, suppression", func(t *testing.T) {
		s := open(t)
		a, b := job(now, "s1"), job(now, "s2")
		if err := s.Create(ctx, a); err != nil {
			t.Fatal(err)
		}
		_ = s.Create(ctx, b)
		if a.ID == "" || a.CreatedAt.IsZero() {
			t.Fatalf("préparation : %+v", a)
		}
		got, err := s.Get(ctx, a.ID)
		if err != nil || got == nil || got.Conversation != conv || !got.NextRun.Equal(now) || !got.Silent || !got.Enabled || got.Spec != "every 1h" {
			t.Fatalf("relu : %+v %v", got, err)
		}
		if l, _ := s.List(ctx, schedule.Filter{Session: "s1"}); len(l) != 1 || l[0].ID != a.ID {
			t.Fatalf("par session : %+v", l)
		}
		if l, _ := s.List(ctx, schedule.Filter{}); len(l) != 2 || l[0].ID != a.ID {
			t.Fatalf("toutes, plus ancienne d'abord : %+v", l)
		}
		if ok, _ := s.Delete(ctx, a.ID); !ok {
			t.Fatal("suppression")
		}
		if g, _ := s.Get(ctx, a.ID); g != nil {
			t.Fatal("encore là")
		}
		if err := s.SetEnabled(ctx, "inconnue", true, now); err != schedule.ErrNotFound {
			t.Fatalf("SetEnabled inconnue : %v", err)
		}
	})

	t.Run("échéances", func(t *testing.T) {
		s := open(t)
		due, later, off := job(now.Add(-time.Minute), "s1"), job(now.Add(time.Minute), "s1"), job(now.Add(-time.Hour), "s1")
		_ = s.Create(ctx, due)
		_ = s.Create(ctx, later)
		_ = s.Create(ctx, off)
		_ = s.SetEnabled(ctx, off.ID, false, now.Add(-time.Hour))
		l, err := s.Due(ctx, now, 10)
		if err != nil || len(l) != 1 || l[0].ID != due.ID {
			t.Fatalf("dues : %+v %v", l, err)
		}
	})

	t.Run("une échéance n'est prise qu'une fois", func(t *testing.T) {
		s := open(t)
		j := job(now, "s1")
		_ = s.Create(ctx, j)
		var wins atomic.Int32
		var wg sync.WaitGroup
		for range 10 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ok, err := s.Advance(ctx, j.ID, now, now.Add(time.Hour), true)
				if err != nil {
					t.Error(err)
				}
				if ok {
					wins.Add(1)
				}
			}()
		}
		wg.Wait()
		if wins.Load() != 1 {
			t.Fatalf("%d prises, attendu 1", wins.Load())
		}
		got, _ := s.Get(ctx, j.ID)
		if !got.NextRun.Equal(now.Add(time.Hour)) || !got.LastRun.Equal(now) || !got.Enabled {
			t.Fatalf("après : %+v", got)
		}
		// Sautée : LastRun inchangé.
		_, _ = s.Advance(ctx, j.ID, now.Add(time.Hour), now.Add(2*time.Hour), false)
		got, _ = s.Get(ctx, j.ID)
		if !got.LastRun.Equal(now) {
			t.Fatalf("échéance sautée comptée : %+v", got)
		}
		// Dernière échéance : la tâche se désactive.
		_, _ = s.Advance(ctx, j.ID, now.Add(2*time.Hour), time.Time{}, true)
		got, _ = s.Get(ctx, j.ID)
		if got.Enabled || !got.NextRun.IsZero() {
			t.Fatalf("fin : %+v", got)
		}
	})
}
