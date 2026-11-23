// Package notestest vérifie qu'une implémentation de notes.Store (et
// notes.VectorStore) respecte le contrat.
package notestest

import (
	"context"
	"testing"

	"github.com/raucheacho/agentkit/memory/notes"
)

// Store est ce que le contrat exige.
type Store interface {
	notes.Store
	notes.VectorStore
}

// Run exécute le contrat sur des Store neufs produits par open.
func Run(t *testing.T, open func(t *testing.T) Store) {
	ctx := context.Background()
	put := func(t *testing.T, s Store, scope, path, content string) {
		t.Helper()
		if err := s.Put(ctx, notes.Note{Scope: scope, Path: path, Content: content}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("écriture, lecture, liste, suppression", func(t *testing.T) {
		s := open(t)
		put(t, s, "a", "clients/ana", "Ana préfère le matin")
		put(t, s, "a", "clients/bob", "Bob paie par virement")
		put(t, s, "a", "projets/site", "refonte en mars")
		put(t, s, "b", "clients/ana", "autre scope")
		put(t, s, "a", "clients/ana", "Ana préfère le soir") // remplace

		n, err := s.Get(ctx, "a", "clients/ana")
		if err != nil || n == nil || n.Content != "Ana préfère le soir" || n.UpdatedAt.IsZero() {
			t.Fatalf("lecture : %+v %v", n, err)
		}
		if n, _ := s.Get(ctx, "a", "clients/zoe"); n != nil {
			t.Fatal("note inconnue")
		}
		all, _ := s.List(ctx, "a", "")
		if len(all) != 3 || all[0].Path != "clients/ana" || all[2].Path != "projets/site" {
			t.Fatalf("liste : %+v", all)
		}
		cl, _ := s.List(ctx, "a", "clients")
		if len(cl) != 2 {
			t.Fatalf("zone : %+v", cl)
		}
		if ok, _ := s.Delete(ctx, "a", "clients/bob"); !ok {
			t.Fatal("suppression")
		}
		if ok, _ := s.Delete(ctx, "a", "clients/bob"); ok {
			t.Fatal("double suppression")
		}
		if l, _ := s.List(ctx, "b", ""); len(l) != 1 {
			t.Fatalf("scopes séparés : %+v", l)
		}
	})

	t.Run("recherche plein texte", func(t *testing.T) {
		s := open(t)
		put(t, s, "a", "clients/ana", "Ana préfère être appelée le matin, jamais le vendredi.")
		put(t, s, "a", "projets/site", "Refonte du site prévue en mars.")
		put(t, s, "b", "clients/ana", "matin matin matin")
		hits, err := s.Search(ctx, "a", "Quand appeler Ana le matin ?", 5)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 1 || hits[0].Path != "clients/ana" || hits[0].Scope != "a" {
			t.Fatalf("recherche : %+v", hits)
		}
		if hits, _ := s.Search(ctx, "a", "prefere", 5); len(hits) != 1 {
			t.Fatalf("sans accents : %+v", hits)
		}
		if hits, _ := s.Search(ctx, "a", "site", 5); len(hits) != 1 || hits[0].Path != "projets/site" {
			t.Fatalf("par chemin : %+v", hits)
		}
		put(t, s, "a", "projets/site", "Refonte reportée en juin.")
		if hits, _ := s.Search(ctx, "a", "mars", 5); len(hits) != 0 {
			t.Fatalf("index mis à jour : %+v", hits)
		}
		_, _ = s.Delete(ctx, "a", "projets/site")
		if hits, _ := s.Search(ctx, "a", "juin", 5); len(hits) != 0 {
			t.Fatalf("index après suppression : %+v", hits)
		}
	})

	t.Run("vecteurs", func(t *testing.T) {
		s := open(t)
		put(t, s, "a", "x/proche", "p")
		put(t, s, "a", "x/loin", "l")
		put(t, s, "b", "x/autre", "o")
		_ = s.SetVector(ctx, "a", "x/proche", []float32{1, 0.1})
		_ = s.SetVector(ctx, "a", "x/loin", []float32{0, 1})
		_ = s.SetVector(ctx, "b", "x/autre", []float32{1, 0})
		hits, err := s.Nearest(ctx, "a", []float32{1, 0}, 5)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 2 || hits[0].Path != "x/proche" || hits[0].Score <= hits[1].Score {
			t.Fatalf("proximité : %+v", hits)
		}
		put(t, s, "a", "x/proche", "contenu changé") // le vecteur n'est plus valable
		if hits, _ := s.Nearest(ctx, "a", []float32{1, 0}, 5); len(hits) != 1 || hits[0].Path != "x/loin" {
			t.Fatalf("vecteur invalidé : %+v", hits)
		}
	})
}
