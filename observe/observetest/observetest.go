// Package observetest vérifie qu'une implémentation de observe.Seen respecte le contrat.
package observetest

import (
	"context"
	"testing"
	"time"

	"github.com/raucheacho/agentkit/observe"
)

// Run exécute le contrat sur des Seen neufs produits par open.
func Run(t *testing.T, open func(t *testing.T) observe.Seen) {
	ctx := context.Background()
	s := open(t)
	now := time.Now()
	if got, err := s.Seen(ctx, []string{"a"}); err != nil || len(got) != 0 {
		t.Fatalf("vide : %v %v", got, err)
	}
	if err := s.Mark(ctx, []string{"a", "b"}, now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	_ = s.Mark(ctx, []string{"c"}, now)
	_ = s.Mark(ctx, []string{"c"}, now) // idempotent
	got, _ := s.Seen(ctx, []string{"a", "c", "z"})
	if !got["a"] || !got["c"] || got["z"] || len(got) != 2 {
		t.Fatalf("vues : %v", got)
	}
	n, err := s.Forget(ctx, now.Add(-24*time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("oubli : %d %v", n, err)
	}
	if got, _ := s.Seen(ctx, []string{"a", "c"}); got["a"] || !got["c"] {
		t.Fatalf("après oubli : %v", got)
	}
	many := make([]string, 1200)
	for i := range many {
		many[i] = "k" + time.Duration(i).String()
	}
	_ = s.Mark(ctx, many, now)
	if got, _ := s.Seen(ctx, many); len(got) != len(many) {
		t.Fatalf("beaucoup de clés : %d", len(got))
	}
}
