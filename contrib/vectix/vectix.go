// Package vectix range les notes de la mémoire (notes.Store et
// notes.VectorStore) dans VectixDB, base embarquée de recherche hybride
// (vecteurs HNSW + BM25 + filtres). À préférer au Store SQLite quand les
// notes se comptent en centaines de milliers ou que la recherche par
// vecteurs doit rester rapide.
//
// VectixDB est une bibliothèque native (cgo) : ce package est un module à
// part, pour que le module agentkit reste en Go pur.
package vectix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/raucheacho/agentkit/memory/notes"
	"github.com/raucheacho/agentkit/session"
	vx "github.com/raucheacho/vectixdb/bindings/go"
)

// Config configure le Store.
type Config struct {
	// Collection : nom de la collection (défaut "agentkit_notes").
	Collection string
	// Dim : taille des vecteurs du modèle d'embeddings (obligatoire).
	Dim uint32
	// Model : nom du modèle d'embeddings, gardé comme étiquette de la collection.
	Model string
	// Quantized : vecteurs en int8 (moins de mémoire, cosinus seulement).
	Quantized bool
}

// Store est un notes.Store sur une base VectixDB ouverte par l'application
// (vx.Open) ; l'application la ferme.
type Store struct {
	db   *vx.DB
	coll string
}

// payload est ce qu'on relit d'une note (les champs indexés ne servent qu'à
// chercher et filtrer).
type payload struct {
	Scope     string    `json:"scope"`
	Path      string    `json:"path"`
	Content   string    `json:"content"`
	UpdatedAt time.Time `json:"updated_at"`
	Vector    []float32 `json:"vector,omitempty"`
}

// New crée la collection si besoin.
func New(db *vx.DB, cfg Config) (*Store, error) {
	if cfg.Dim == 0 {
		return nil, errors.New("vectix : Config.Dim est obligatoire")
	}
	if cfg.Collection == "" {
		cfg.Collection = "agentkit_notes"
	}
	err := db.CreateCollection(cfg.Collection, vx.Schema{
		Vector: &vx.VectorSpec{Dim: cfg.Dim, Metric: vx.Cosine, Quantized: cfg.Quantized, Tag: cfg.Model},
		Fields: []vx.Field{
			{Name: "scope", Type: vx.TypeKeyword},
			{Name: "area", Type: vx.TypeKeyword},
			{Name: "text", Type: vx.TypeText},
		},
	})
	if err != nil && !errors.Is(err, vx.ErrExists) {
		return nil, fmt.Errorf("vectix : création de la collection : %w", err)
	}
	return &Store{db: db, coll: cfg.Collection}, nil
}

func id(scope, path string) string { return scope + "\x00" + path }

func (s *Store) write(p payload) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = s.db.PutDocs(s.coll, vx.Doc{
		ID:     id(p.Scope, p.Path),
		Vector: p.Vector,
		Fields: map[string]vx.Value{
			"scope": vx.Keyword(p.Scope),
			"area":  vx.Keyword(notes.Area(p.Path)),
			// Index sans accents ni casse : « vélo » trouve « velo ».
			"text": vx.Text(session.Fold(strings.ReplaceAll(p.Path, "/", " ") + "\n" + p.Content)),
		},
		Payload: data,
	})
	return err
}

func (s *Store) read(scope, path string) (*payload, error) {
	r, err := s.db.Get(s.coll, id(scope, path))
	if err != nil || r == nil {
		return nil, err
	}
	var p payload
	if err := json.Unmarshal(r.Payload, &p); err != nil {
		return nil, fmt.Errorf("vectix : note illisible : %w", err)
	}
	return &p, nil
}

func (s *Store) Put(_ context.Context, n notes.Note) error {
	if n.UpdatedAt.IsZero() {
		n.UpdatedAt = time.Now().UTC()
	}
	p := payload{Scope: n.Scope, Path: n.Path, Content: n.Content, UpdatedAt: n.UpdatedAt.UTC()}
	old, err := s.read(n.Scope, n.Path)
	if err != nil {
		return err
	}
	if old != nil && old.Content == n.Content {
		p.Vector = old.Vector // le vecteur reste valable
	}
	return s.write(p)
}

func (s *Store) Get(_ context.Context, scope, path string) (*notes.Note, error) {
	p, err := s.read(scope, path)
	if err != nil || p == nil {
		return nil, err
	}
	return &notes.Note{Scope: p.Scope, Path: p.Path, Content: p.Content, UpdatedAt: p.UpdatedAt}, nil
}

func (s *Store) Delete(_ context.Context, scope, path string) (bool, error) {
	p, err := s.read(scope, path)
	if err != nil || p == nil {
		return false, err
	}
	_, err = s.db.Write(vx.Delete(s.coll, id(scope, path)))
	return err == nil, err
}

// maxList borne List : au-delà, l'index du prompt n'a plus de sens.
const maxList = 10000

func (s *Store) List(_ context.Context, scope, area string) ([]notes.Note, error) {
	f := vx.Eq("scope", vx.Keyword(scope))
	if area != "" {
		f = vx.And(f, vx.Eq("area", vx.Keyword(area)))
	}
	rs, err := s.db.Search(s.coll, vx.Query{Filter: f, K: maxList})
	if err != nil {
		return nil, err
	}
	hits, err := decode(rs, func(vx.Result) float64 { return 0 })
	out := make([]notes.Note, len(hits))
	for i, h := range hits {
		out[i] = h.Note
	}
	slices.SortFunc(out, func(a, b notes.Note) int { return strings.Compare(a.Path, b.Path) })
	return out, err
}

func (s *Store) Search(_ context.Context, scope, query string, limit int) ([]notes.Hit, error) {
	terms := session.Terms(query)
	if len(terms) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	rs, err := s.db.Search(s.coll, vx.Query{Text: strings.Join(terms, " "), TextFields: []string{"text"},
		Filter: vx.Eq("scope", vx.Keyword(scope)), K: uint32(limit)})
	if err != nil {
		return nil, err
	}
	return decode(rs, func(r vx.Result) float64 { return float64(r.TextScore) })
}

func (s *Store) SetVector(_ context.Context, scope, path string, vec []float32) error {
	p, err := s.read(scope, path)
	if err != nil || p == nil {
		return err
	}
	p.Vector = vec
	return s.write(*p)
}

func (s *Store) Nearest(_ context.Context, scope string, vec []float32, limit int) ([]notes.Hit, error) {
	if limit <= 0 {
		limit = 10
	}
	rs, err := s.db.Search(s.coll, vx.Query{Vector: vec, Filter: vx.Eq("scope", vx.Keyword(scope)), K: uint32(limit)})
	if err != nil {
		return nil, err
	}
	var near []vx.Result
	for _, r := range rs {
		if r.VectorRank > 0 { // documents sans vecteur exclus
			near = append(near, r)
		}
	}
	// Distance cosinus : 1 - similarité.
	return decode(near, func(r vx.Result) float64 { return 1 - float64(r.VectorDistance) })
}

func decode(rs []vx.Result, score func(vx.Result) float64) ([]notes.Hit, error) {
	out := make([]notes.Hit, 0, len(rs))
	for _, r := range rs {
		var p payload
		if err := json.Unmarshal(r.Payload, &p); err != nil {
			return nil, fmt.Errorf("vectix : note illisible : %w", err)
		}
		out = append(out, notes.Hit{Note: notes.Note{Scope: p.Scope, Path: p.Path, Content: p.Content, UpdatedAt: p.UpdatedAt}, Score: score(r)})
	}
	return out, nil
}

var (
	_ notes.Store       = (*Store)(nil)
	_ notes.VectorStore = (*Store)(nil)
)
