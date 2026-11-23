// Package sqlite implémente notes.Store et notes.VectorStore sur une base SQL
// compatible SQLite, sans imposer de pilote. La recherche plein texte passe
// par FTS5 si le pilote le fournit (modernc.org/sqlite : oui), sinon par
// LIKE ; les vecteurs sont comparés en Go (adapté à quelques dizaines de
// milliers de notes par scope).
package sqlite

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/raucheacho/agentkit/llm"
	"github.com/raucheacho/agentkit/memory/notes"
	"github.com/raucheacho/agentkit/session"
)

// Store est un notes.Store SQLite.
type Store struct {
	db         *sql.DB
	table, fts string
	hasFTS     bool
}

var validPrefix = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// New crée les tables si besoin. prefix vide : "agentkit_".
func New(ctx context.Context, db *sql.DB, prefix string) (*Store, error) {
	if prefix == "" {
		prefix = "agentkit_"
	}
	if !validPrefix.MatchString(prefix) {
		return nil, fmt.Errorf("préfixe de table invalide : %q", prefix)
	}
	s := &Store{db: db, table: prefix + "notes", fts: prefix + "notes_fts"}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		scope      TEXT NOT NULL,
		path       TEXT NOT NULL,
		content    TEXT NOT NULL,
		vector     BLOB,
		updated_at INTEGER NOT NULL,
		UNIQUE (scope, path))`, s.table)); err != nil {
		return nil, fmt.Errorf("notes sqlite : création de la table : %w", err)
	}
	s.hasFTS = s.setupFTS(ctx) == nil
	return s, nil
}

func (s *Store) setupFTS(ctx context.Context) error {
	var existing int
	_ = s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name = ?`, s.fts).Scan(&existing)
	stmts := []string{
		fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS %s USING fts5(path, content, content='%s', content_rowid='id', tokenize='unicode61 remove_diacritics 2')`, s.fts, s.table),
		fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %s_ai AFTER INSERT ON %s BEGIN
			INSERT INTO %s(rowid, path, content) VALUES (new.id, new.path, new.content); END`, s.fts, s.table, s.fts),
		fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %s_ad AFTER DELETE ON %s BEGIN
			INSERT INTO %s(%s, rowid, path, content) VALUES ('delete', old.id, old.path, old.content); END`, s.fts, s.table, s.fts, s.fts),
		fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %s_au AFTER UPDATE OF path, content ON %s BEGIN
			INSERT INTO %s(%s, rowid, path, content) VALUES ('delete', old.id, old.path, old.content);
			INSERT INTO %s(rowid, path, content) VALUES (new.id, new.path, new.content); END`, s.fts, s.table, s.fts, s.fts, s.fts),
	}
	if existing == 0 {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO %s(%s) VALUES ('rebuild')`, s.fts, s.fts))
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Put(ctx context.Context, n notes.Note) error {
	if n.UpdatedAt.IsZero() {
		n.UpdatedAt = time.Now().UTC()
	}
	// Un contenu modifié invalide le vecteur.
	_, err := s.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s (scope, path, content, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(scope, path) DO UPDATE SET
			vector = CASE WHEN content = excluded.content THEN vector ELSE NULL END,
			content = excluded.content, updated_at = excluded.updated_at`, s.table),
		n.Scope, n.Path, n.Content, n.UpdatedAt.UnixNano())
	if err != nil {
		return fmt.Errorf("notes sqlite : enregistrement de %s : %w", n.Path, err)
	}
	return nil
}

func (s *Store) Get(ctx context.Context, scope, path string) (*notes.Note, error) {
	n := notes.Note{Scope: scope, Path: path}
	var at int64
	err := s.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT content, updated_at FROM %s WHERE scope = ? AND path = ?`, s.table), scope, path).Scan(&n.Content, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	n.UpdatedAt = time.Unix(0, at).UTC()
	return &n, nil
}

func (s *Store) Delete(ctx context.Context, scope, path string) (bool, error) {
	res, err := s.db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE scope = ? AND path = ?`, s.table), scope, path)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *Store) List(ctx context.Context, scope, area string) ([]notes.Note, error) {
	q := fmt.Sprintf(`SELECT scope, path, content, updated_at FROM %s WHERE scope = ?`, s.table)
	args := []any{scope}
	if area != "" {
		q += ` AND path LIKE ? ESCAPE '\'`
		args = append(args, strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(area)+"/%")
	}
	q += ` ORDER BY path`
	hits, err := s.query(ctx, false, q, args...)
	out := make([]notes.Note, len(hits))
	for i, h := range hits {
		out[i] = h.Note
	}
	return out, err
}

func (s *Store) Search(ctx context.Context, scope, query string, limit int) ([]notes.Hit, error) {
	terms := session.Terms(query)
	if len(terms) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	if s.hasFTS {
		quoted := make([]string, len(terms))
		for i, t := range terms {
			quoted[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"*`
		}
		return s.query(ctx, true, fmt.Sprintf(`SELECT n.scope, n.path, n.content, n.updated_at, -f.rank FROM %s f JOIN %s n ON n.id = f.rowid
			WHERE %s MATCH ? AND n.scope = ? ORDER BY f.rank LIMIT ?`, s.fts, s.table, s.fts), strings.Join(quoted, " OR "), scope, limit)
	}
	var like []string
	args := []any{scope}
	for _, t := range terms {
		like = append(like, "lower(path || ' ' || content) LIKE ?")
		args = append(args, "%"+t+"%")
	}
	args = append(args, limit)
	return s.query(ctx, true, fmt.Sprintf(`SELECT scope, path, content, updated_at, 1 FROM %s WHERE scope = ? AND (%s) ORDER BY updated_at DESC LIMIT ?`,
		s.table, strings.Join(like, " OR ")), args...)
}

func (s *Store) query(ctx context.Context, withScore bool, q string, args ...any) ([]notes.Hit, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("notes sqlite : %w", err)
	}
	defer rows.Close()
	var out []notes.Hit
	for rows.Next() {
		var h notes.Hit
		var at int64
		dest := []any{&h.Scope, &h.Path, &h.Content, &at}
		if withScore {
			dest = append(dest, &h.Score)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		h.UpdatedAt = time.Unix(0, at).UTC()
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) SetVector(ctx context.Context, scope, path string, vec []float32) error {
	_, err := s.db.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET vector = ? WHERE scope = ? AND path = ?`, s.table), encode(vec), scope, path)
	return err
}

func (s *Store) Nearest(ctx context.Context, scope string, vec []float32, limit int) ([]notes.Hit, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT path, content, updated_at, vector FROM %s WHERE scope = ? AND vector IS NOT NULL`, s.table), scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []notes.Hit
	for rows.Next() {
		h := notes.Hit{Note: notes.Note{Scope: scope}}
		var at int64
		var blob []byte
		if err := rows.Scan(&h.Path, &h.Content, &at, &blob); err != nil {
			return nil, err
		}
		h.UpdatedAt = time.Unix(0, at).UTC()
		h.Score = llm.Cosine(vec, decode(blob))
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Tri décroissant par score, puis par chemin.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && (out[j].Score > out[j-1].Score || out[j].Score == out[j-1].Score && out[j].Path < out[j-1].Path); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func encode(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

func decode(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}

var (
	_ notes.Store       = (*Store)(nil)
	_ notes.VectorStore = (*Store)(nil)
)
