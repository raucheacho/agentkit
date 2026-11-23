// Package delegatetest vérifie qu'une implémentation de delegate.Store respecte le contrat.
package delegatetest

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raucheacho/agentkit/delegate"
	"github.com/raucheacho/agentkit/tool"
)

// Run exécute le contrat sur des Store neufs produits par open.
func Run(t *testing.T, open func(t *testing.T) delegate.Store) {
	ctx := context.Background()
	conv := tool.Conversation{Session: "s1", Channel: "tg", ChatID: "1", Source: "user", Turn: "t1"}

	t.Run("création et lecture", func(t *testing.T) {
		s := open(t)
		task := &delegate.Task{Goal: "chercher", Context: "ctx", Conversation: conv}
		if err := s.Create(ctx, task); err != nil {
			t.Fatal(err)
		}
		if task.ID == "" || task.Status != delegate.TaskRunning || task.CreatedAt.IsZero() {
			t.Fatalf("préparation : %+v", task)
		}
		got, err := s.Get(ctx, task.ID)
		if err != nil || got == nil {
			t.Fatal(err)
		}
		if got.Goal != "chercher" || got.Context != "ctx" || got.Conversation != conv || got.Status != delegate.TaskRunning {
			t.Fatalf("relu : %+v", got)
		}
		if g, _ := s.Get(ctx, "inconnue"); g != nil {
			t.Fatal("tâche inconnue")
		}
	})

	t.Run("fin unique", func(t *testing.T) {
		s := open(t)
		task := &delegate.Task{Goal: "g", Conversation: conv}
		_ = s.Create(ctx, task)
		var wins atomic.Int32
		var wg sync.WaitGroup
		for i := range 10 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				st := delegate.TaskDone
				if i%2 == 1 {
					st = delegate.TaskCancelled
				}
				ok, err := s.Finish(ctx, task.ID, st, "r", "", time.Now())
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
			t.Fatalf("%d fins acceptées, attendu 1", wins.Load())
		}
		got, _ := s.Get(ctx, task.ID)
		if got.Status == delegate.TaskRunning || got.FinishedAt.IsZero() {
			t.Fatalf("après fin : %+v", got)
		}
		if ok, _ := s.Finish(ctx, "inconnue", delegate.TaskDone, "", "", time.Now()); ok {
			t.Fatal("fin d'une tâche inconnue")
		}
	})

	t.Run("liste et filtres", func(t *testing.T) {
		s := open(t)
		var ids []string
		for i, sess := range []string{"s1", "s2", "s1"} {
			c := conv
			c.Session = sess
			task := &delegate.Task{Goal: string(rune('a' + i)), Conversation: c}
			_ = s.Create(ctx, task)
			ids = append(ids, task.ID)
		}
		_, _ = s.Finish(ctx, ids[2], delegate.TaskDone, "ok", "", time.Now())
		l, _ := s.List(ctx, delegate.TaskFilter{Session: "s1"})
		if len(l) != 2 || l[0].ID != ids[2] || l[1].ID != ids[0] {
			t.Fatalf("par session (récent d'abord) : %v", l)
		}
		l, _ = s.List(ctx, delegate.TaskFilter{Status: []delegate.TaskStatus{delegate.TaskRunning}})
		if len(l) != 2 {
			t.Fatalf("par état : %d", len(l))
		}
		l, _ = s.List(ctx, delegate.TaskFilter{Limit: 1})
		if len(l) != 1 || l[0].ID != ids[2] {
			t.Fatalf("limite : %v", l)
		}
	})

	t.Run("reprise sans relance", func(t *testing.T) {
		s := open(t)
		a := &delegate.Task{Goal: "a", Conversation: conv}
		b := &delegate.Task{Goal: "b", Conversation: conv}
		_ = s.Create(ctx, a)
		_ = s.Create(ctx, b)
		_, _ = s.Finish(ctx, b.ID, delegate.TaskDone, "ok", "", time.Now())
		lost, err := s.Recover(ctx, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if len(lost) != 1 || lost[0].ID != a.ID || lost[0].Status != delegate.TaskFailed || lost[0].Conversation != conv {
			t.Fatalf("reprise : %+v", lost)
		}
		got, _ := s.Get(ctx, a.ID)
		if got.Status != delegate.TaskFailed || got.Error != delegate.Interrupted {
			t.Fatalf("après reprise : %+v", got)
		}
		if lost, _ := s.Recover(ctx, time.Now()); len(lost) != 0 {
			t.Fatal("seconde reprise non vide")
		}
	})
}
