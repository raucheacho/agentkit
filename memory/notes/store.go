// Package notes est une mémoire en notes adressées par zone : « clients/ana »,
// « projets/site-web », « preferences/horaires ». L'agent les écrit et les
// relit par des outils ; l'index des notes est donné dans le prompt système ;
// les notes pertinentes pour chaque message sont rappelées dans la note
// éphémère. La recherche est plein texte, et hybride (plein texte +
// vecteurs, fusion par rangs) si un llm.Embedder est fourni.
package notes

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/raucheacho/agentkit/llm"
	"github.com/raucheacho/agentkit/session"
)

// Note est un souvenir.
type Note struct {
	// Scope : à qui appartient la note (une session, un utilisateur, "global"…).
	Scope string
	// Path : « zone/nom » en minuscules (voir CleanPath).
	Path      string
	Content   string
	UpdatedAt time.Time
}

// Hit est une note retrouvée.
type Hit struct {
	Note
	Score float64
}

// Store conserve les notes.
type Store interface {
	// Put crée ou remplace une note (UpdatedAt rempli s'il est vide).
	Put(ctx context.Context, n Note) error
	// Get renvoie une note ; nil si elle n'existe pas.
	Get(ctx context.Context, scope, path string) (*Note, error)
	// Delete supprime une note ; false si elle n'existait pas.
	Delete(ctx context.Context, scope, path string) (bool, error)
	// List renvoie les notes d'un scope, triées par chemin ; area non vide
	// restreint à une zone (« clients »).
	List(ctx context.Context, scope, area string) ([]Note, error)
	// Search cherche en plein texte (chemin et contenu), du plus pertinent
	// au moins pertinent.
	Search(ctx context.Context, scope, query string, limit int) ([]Hit, error)
}

// VectorStore est implémenté par les Store qui conservent des vecteurs.
type VectorStore interface {
	// SetVector associe un vecteur à une note existante.
	SetVector(ctx context.Context, scope, path string, vec []float32) error
	// Nearest renvoie les notes les plus proches de vec (Score : cosinus).
	Nearest(ctx context.Context, scope string, vec []float32, limit int) ([]Hit, error)
}

var validPath = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*(/[a-z0-9][a-z0-9_-]*){1,3}$`)

// ErrPath : chemin de note invalide.
var ErrPath = errors.New(`chemin invalide : attendu « zone/nom » (lettres, chiffres, - et _), par exemple "clients/ana-martin"`)

// CleanPath normalise un chemin (minuscules, sans accents, espaces en
// tirets) et le valide.
func CleanPath(p string) (string, error) {
	p = session.Fold(strings.TrimSpace(p))
	p = strings.Trim(strings.ReplaceAll(p, " ", "-"), "/")
	p = strings.TrimSuffix(p, ".md")
	if !validPath.MatchString(p) {
		return "", ErrPath
	}
	return p, nil
}

// Area renvoie la zone d'un chemin (« clients » pour « clients/ana »).
func Area(path string) string {
	area, _, _ := strings.Cut(path, "/")
	return area
}

// Memory est un Store en mémoire (tests, agents éphémères). Il implémente
// VectorStore.
type Memory struct {
	mu    sync.Mutex
	notes map[[2]string]Note
	vecs  map[[2]string][]float32
}

// NewMemory crée un Store en mémoire.
func NewMemory() *Memory {
	return &Memory{notes: map[[2]string]Note{}, vecs: map[[2]string][]float32{}}
}

func (m *Memory) Put(_ context.Context, n Note) error {
	if n.UpdatedAt.IsZero() {
		n.UpdatedAt = time.Now().UTC()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := [2]string{n.Scope, n.Path}
	if old, ok := m.notes[k]; ok && old.Content != n.Content {
		delete(m.vecs, k) // le vecteur ne correspond plus
	}
	m.notes[k] = n
	return nil
}

func (m *Memory) Get(_ context.Context, scope, path string) (*Note, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.notes[[2]string{scope, path}]
	if !ok {
		return nil, nil
	}
	return &n, nil
}

func (m *Memory) Delete(_ context.Context, scope, path string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := [2]string{scope, path}
	_, ok := m.notes[k]
	delete(m.notes, k)
	delete(m.vecs, k)
	return ok, nil
}

func (m *Memory) List(_ context.Context, scope, area string) ([]Note, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Note
	for k, n := range m.notes {
		if k[0] == scope && (area == "" || Area(n.Path) == area) {
			out = append(out, n)
		}
	}
	slices.SortFunc(out, func(a, b Note) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

func (m *Memory) Search(_ context.Context, scope, query string, limit int) ([]Hit, error) {
	terms := session.Terms(query)
	if len(terms) == 0 {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Hit
	for k, n := range m.notes {
		if k[0] != scope {
			continue
		}
		text := session.Fold(n.Path + " " + n.Content)
		score := 0.0
		for _, t := range terms {
			score += float64(strings.Count(text, t))
		}
		if score > 0 {
			out = append(out, Hit{Note: n, Score: score})
		}
	}
	return top(out, limit), nil
}

func (m *Memory) SetVector(_ context.Context, scope, path string, vec []float32) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := [2]string{scope, path}
	if _, ok := m.notes[k]; ok {
		m.vecs[k] = vec
	}
	return nil
}

func (m *Memory) Nearest(_ context.Context, scope string, vec []float32, limit int) ([]Hit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Hit
	for k, v := range m.vecs {
		if k[0] == scope {
			out = append(out, Hit{Note: m.notes[k], Score: llm.Cosine(vec, v)})
		}
	}
	return top(out, limit), nil
}

func top(hits []Hit, limit int) []Hit {
	slices.SortStableFunc(hits, func(a, b Hit) int {
		switch {
		case a.Score > b.Score:
			return -1
		case a.Score < b.Score:
			return 1
		}
		return strings.Compare(a.Path, b.Path)
	})
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

var (
	_ Store       = (*Memory)(nil)
	_ VectorStore = (*Memory)(nil)
)
