// Package trusttest vérifie qu'une implémentation de trust.Store respecte le contrat.
package trusttest

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/raucheacho/agentkit/tool"
	"github.com/raucheacho/agentkit/trust"
)

// Run exécute le contrat sur des Store neufs produits par open.
func Run(t *testing.T, open func(t *testing.T) trust.Store) {
	ctx := context.Background()
	now := time.Now().UTC()

	t.Run("création et lecture", func(t *testing.T) {
		s := open(t)
		r := &trust.Request{Tool: "book", Args: json.RawMessage(`{"date":"lundi"}`), Summary: "book lundi",
			Conversation: tool.Conversation{Session: "s1", Channel: "tg", ChatID: "1", Source: "user", Turn: "t1"},
			ExpiresAt:    now.Add(time.Hour)}
		if err := s.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
		if r.ID == "" || r.Status != trust.Pending || r.Kind != trust.KindApproval || r.ArgsHash != trust.HashArgs(r.Args) {
			t.Fatalf("préparation : %+v", r)
		}
		got, err := s.Get(ctx, r.ID)
		if err != nil || got == nil {
			t.Fatal(err)
		}
		if string(got.Args) != `{"date":"lundi"}` || got.Conversation != r.Conversation || got.ExpiresAt.Sub(r.ExpiresAt).Abs() > time.Millisecond {
			t.Fatalf("relu : %+v", got)
		}
		if g, _ := s.Get(ctx, "inconnu"); g != nil {
			t.Fatal("demande inconnue")
		}
	})

	t.Run("transitions atomiques", func(t *testing.T) {
		s := open(t)
		r := &trust.Request{Tool: "pay", Args: json.RawMessage(`{}`), Conversation: tool.Conversation{Session: "s"}}
		_ = s.Create(ctx, r)
		var wg sync.WaitGroup
		var mu sync.Mutex
		wins := 0
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := s.Transition(ctx, r.ID, trust.Pending, trust.Approved, "patron", "", now); err == nil {
					mu.Lock()
					wins++
					mu.Unlock()
				} else if !errors.Is(err, trust.ErrNotPending) {
					t.Errorf("erreur inattendue : %v", err)
				}
			}()
		}
		wg.Wait()
		if wins != 1 {
			t.Fatalf("%d décisions acceptées, une seule attendue", wins)
		}
		got, err := s.Transition(ctx, r.ID, trust.Approved, trust.Executing, "", "", now)
		if err != nil || got.DecidedBy != "patron" || got.DecidedAt.IsZero() {
			t.Fatalf("exécution : %+v %v", got, err)
		}
		if n, _ := s.Recover(ctx); n != 1 {
			t.Fatalf("recover : %d", n)
		}
		got, _ = s.Get(ctx, r.ID)
		if got.Status != trust.Failed || got.Result != trust.ErrUncertain {
			t.Fatalf("après recover : %+v", got)
		}
	})

	t.Run("liste filtrée", func(t *testing.T) {
		s := open(t)
		a := &trust.Request{Tool: "x", Args: json.RawMessage(`{}`), Conversation: tool.Conversation{Session: "s1"}}
		b := &trust.Request{Kind: trust.KindQuestion, Summary: "Quel prix ?", Conversation: tool.Conversation{Session: "s1"}}
		c := &trust.Request{Tool: "y", Args: json.RawMessage(`{}`), Conversation: tool.Conversation{Session: "s2"}}
		for _, r := range []*trust.Request{a, b, c} {
			if err := s.Create(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		_, _ = s.Transition(ctx, c.ID, trust.Pending, trust.Denied, "p", "non", now)
		got, _ := s.List(ctx, trust.RequestFilter{Session: "s1"})
		if len(got) != 2 || got[0].ID != b.ID {
			t.Fatalf("par session : %+v", got)
		}
		got, _ = s.List(ctx, trust.RequestFilter{Kind: trust.KindApproval, Status: []trust.Status{trust.Pending}})
		if len(got) != 1 || got[0].ID != a.ID {
			t.Fatalf("par type et statut : %+v", got)
		}
		got, _ = s.List(ctx, trust.RequestFilter{Status: []trust.Status{trust.Denied}})
		if len(got) != 1 || got[0].Result != "non" {
			t.Fatalf("refusée : %+v", got)
		}
	})
}
