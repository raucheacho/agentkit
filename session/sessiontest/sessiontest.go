// Package sessiontest vérifie la compression et la recherche d'un session.Store.
package sessiontest

import (
	"context"
	"testing"

	"github.com/raucheacho/agentkit/llm"
	"github.com/raucheacho/agentkit/session"
)

// Store est ce que le contrat exige.
type Store interface {
	session.Store
	session.Compactor
	session.Searcher
}

func msg(role llm.Role, s string) llm.Message { return llm.Message{Role: role, Content: s} }

// Run exécute le contrat sur des Store neufs produits par open.
func Run(t *testing.T, open func(t *testing.T) Store) {
	ctx := context.Background()

	t.Run("compression", func(t *testing.T) {
		s := open(t)
		_ = s.SetSystem(ctx, "k", "SYS")
		_ = s.Append(ctx, "k", msg(llm.RoleUser, "un"), msg(llm.RoleAssistant, "deux"),
			msg(llm.RoleUser, "trois"), msg(llm.RoleAssistant, "quatre"))
		if err := s.Compact(ctx, "k", "RÉSUMÉ", 2); err != nil {
			t.Fatal(err)
		}
		got, _ := s.Load(ctx, "k", 0)
		if got.System != "SYS" || got.Summary != "RÉSUMÉ" || len(got.Messages) != 2 || got.Messages[0].Content != "trois" {
			t.Fatalf("après compression : %+v", got)
		}
		_ = s.Append(ctx, "k", msg(llm.RoleUser, "cinq"))
		if got, _ := s.Load(ctx, "k", 0); len(got.Messages) != 3 {
			t.Fatalf("ajout après compression : %+v", got.Messages)
		}
		_ = s.Compact(ctx, "k", "R2", 0)
		if got, _ := s.Load(ctx, "k", 0); got.Summary != "R2" || len(got.Messages) != 0 {
			t.Fatalf("compression totale : %+v", got)
		}
		_ = s.Reset(ctx, "k")
		if got, _ := s.Load(ctx, "k", 0); got.Summary != "" || got.System != "" {
			t.Fatalf("après reset : %+v", got)
		}
	})

	t.Run("recherche", func(t *testing.T) {
		s := open(t)
		_ = s.Append(ctx, "a", msg(llm.RoleUser, "Mon vélo rouge est cassé"), msg(llm.RoleAssistant, "Je note pour le vélo."))
		_ = s.Append(ctx, "a", llm.Message{Role: llm.RoleTool, ToolCallID: "x", Content: "vélo dans un outil"})
		_ = s.Append(ctx, "b", msg(llm.RoleUser, "Le vélo de Paul est bleu"), msg(llm.RoleUser, "rien à voir"))
		_ = s.Compact(ctx, "a", "r", 0) // les archives restent consultables

		hits, err := s.Search(ctx, session.Query{Text: "vélo"})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 3 {
			t.Fatalf("toutes sessions : %+v", hits)
		}
		for _, h := range hits {
			if h.Role == llm.RoleTool {
				t.Fatal("résultat d'outil retrouvé")
			}
		}
		hits, _ = s.Search(ctx, session.Query{Text: "velo rouge", Keys: []string{"a"}})
		if len(hits) == 0 || hits[0].Key != "a" || hits[0].Content != "Mon vélo rouge est cassé" {
			t.Fatalf("restreinte et classée : %+v", hits)
		}
		for _, h := range hits {
			if h.Key != "a" {
				t.Fatalf("hors des sessions demandées : %+v", h)
			}
		}
		if hits, _ := s.Search(ctx, session.Query{Text: "vélo", Limit: 1}); len(hits) != 1 {
			t.Fatalf("limite : %d", len(hits))
		}
		if hits, _ := s.Search(ctx, session.Query{Text: "a"}); len(hits) != 0 {
			t.Fatal("termes trop courts")
		}
	})
}
