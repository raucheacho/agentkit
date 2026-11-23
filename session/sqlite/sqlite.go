// Package sqlite implémente session.Store sur une base SQL compatible SQLite.
//
// Le package n'impose aucun pilote : l'application ouvre la base (par exemple
// avec modernc.org/sqlite, sans cgo, ou mattn/go-sqlite3) et passe le *sql.DB.
// Les tables sont préfixées pour cohabiter avec celles de l'application.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/raucheacho/agentkit/llm"
	"github.com/raucheacho/agentkit/session"
)

// Store est un session.Store SQLite. Chaque message est une ligne : l'ajout ne
// réécrit jamais l'historique, et tout l'historique reste disponible pour une
// recherche ultérieure même si Load n'en renvoie que la fin.
type Store struct {
	db              *sql.DB
	meta, msgs, fts string
	// hasFTS : le pilote fournit FTS5 ; sinon la recherche passe par LIKE.
	hasFTS bool
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
	s := &Store{db: db, meta: prefix + "sessions", msgs: prefix + "messages", fts: prefix + "messages_fts"}
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			key        TEXT PRIMARY KEY,
			system     TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL)`, s.meta),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			key          TEXT NOT NULL,
			role         TEXT NOT NULL,
			content      TEXT NOT NULL,
			tool_calls   TEXT,
			tool_call_id TEXT,
			name         TEXT,
			reasoning    TEXT,
			created_at   TIMESTAMP NOT NULL)`, s.msgs),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_key ON %s (key, id)`, s.msgs, s.msgs),
	}
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return nil, fmt.Errorf("session sqlite : création des tables : %w", err)
		}
	}
	// Colonnes ajoutées après coup (bases créées par une version antérieure).
	for _, col := range []struct{ table, def string }{
		{s.meta, "summary TEXT NOT NULL DEFAULT ''"},
		{s.msgs, "archived INTEGER NOT NULL DEFAULT 0"},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s`, col.table, col.def)); err != nil &&
			!strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return nil, fmt.Errorf("session sqlite : migration de %s : %w", col.table, err)
		}
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_live ON %s (key, archived, id)`, s.msgs, s.msgs)); err != nil {
		return nil, err
	}
	s.hasFTS = s.setupFTS(ctx) == nil
	return s, nil
}

// setupFTS crée l'index plein texte (FTS5) s'il est disponible, et le
// remplit à sa création.
func (s *Store) setupFTS(ctx context.Context) error {
	var existing int
	_ = s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name = ?`, s.fts).Scan(&existing)
	stmts := []string{
		fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS %s USING fts5(content, content='%s', content_rowid='id', tokenize='unicode61 remove_diacritics 2')`, s.fts, s.msgs),
		fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %s_ai AFTER INSERT ON %s BEGIN
			INSERT INTO %s(rowid, content) VALUES (new.id, new.content); END`, s.fts, s.msgs, s.fts),
		fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %s_ad AFTER DELETE ON %s BEGIN
			INSERT INTO %s(%s, rowid, content) VALUES ('delete', old.id, old.content); END`, s.fts, s.msgs, s.fts, s.fts),
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

func (s *Store) Load(ctx context.Context, key string, limit int) (*session.Session, error) {
	out := &session.Session{Key: key}
	err := s.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT system, summary FROM %s WHERE key = ?`, s.meta), key).Scan(&out.System, &out.Summary)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("session sqlite : lecture de %s : %w", key, err)
	}
	q := fmt.Sprintf(`SELECT role, content, tool_calls, tool_call_id, name, reasoning FROM %s WHERE key = ? AND archived = 0 ORDER BY id DESC`, s.msgs)
	args := []any{key}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("session sqlite : lecture des messages de %s : %w", key, err)
	}
	defer rows.Close()
	for rows.Next() {
		var m llm.Message
		var role string
		var calls, callID, name, reasoning sql.NullString
		if err := rows.Scan(&role, &m.Content, &calls, &callID, &name, &reasoning); err != nil {
			return nil, err
		}
		m.Role = llm.Role(role)
		m.ToolCallID, m.Name, m.Reasoning = callID.String, name.String, reasoning.String
		if calls.Valid && calls.String != "" {
			if err := json.Unmarshal([]byte(calls.String), &m.ToolCalls); err != nil {
				return nil, fmt.Errorf("session sqlite : appels d'outils illisibles : %w", err)
			}
		}
		out.Messages = append(out.Messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(out.Messages)-1; i < j; i, j = i+1, j-1 {
		out.Messages[i], out.Messages[j] = out.Messages[j], out.Messages[i]
	}
	return out, nil
}

func (s *Store) Append(ctx context.Context, key string, msgs ...llm.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	if err := s.touch(ctx, tx, key, now); err != nil {
		return err
	}
	ins := fmt.Sprintf(`INSERT INTO %s (key, role, content, tool_calls, tool_call_id, name, reasoning, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, s.msgs)
	for _, m := range msgs {
		var calls any
		if len(m.ToolCalls) > 0 {
			b, err := json.Marshal(m.ToolCalls)
			if err != nil {
				return err
			}
			calls = string(b)
		}
		if _, err := tx.ExecContext(ctx, ins, key, string(m.Role), m.Content, calls, m.ToolCallID, m.Name, m.Reasoning, now); err != nil {
			return fmt.Errorf("session sqlite : ajout à %s : %w", key, err)
		}
	}
	return tx.Commit()
}

func (s *Store) touch(ctx context.Context, tx *sql.Tx, key string, now time.Time) error {
	_, err := tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s (key, system, created_at, updated_at) VALUES (?, '', ?, ?)
		ON CONFLICT(key) DO UPDATE SET updated_at = excluded.updated_at`, s.meta), key, now, now)
	return err
}

func (s *Store) SetSystem(ctx context.Context, key, system string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s (key, system, created_at, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET system = excluded.system, updated_at = excluded.updated_at`, s.meta), key, system, now, now)
	if err != nil {
		return fmt.Errorf("session sqlite : prompt de %s : %w", key, err)
	}
	return nil
}

func (s *Store) Reset(ctx context.Context, key string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, t := range []string{s.msgs, s.meta} { // les déclencheurs retirent aussi l'index plein texte
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE key = ?`, t), key); err != nil {
			return fmt.Errorf("session sqlite : effacement de %s : %w", key, err)
		}
	}
	return tx.Commit()
}

// Compact archive les messages non archivés sauf les keep derniers et fixe
// le résumé, dans une transaction.
func (s *Store) Compact(ctx context.Context, key, summary string, keep int) error {
	if keep < 0 {
		keep = 0
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	if err := s.touch(ctx, tx, key, now); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET archived = 1 WHERE key = ? AND archived = 0 AND id NOT IN
		(SELECT id FROM %s WHERE key = ? AND archived = 0 ORDER BY id DESC LIMIT ?)`, s.msgs, s.msgs), key, key, keep)
	if err != nil {
		return fmt.Errorf("session sqlite : compression de %s : %w", key, err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET summary = ? WHERE key = ?`, s.meta), summary, key); err != nil {
		return err
	}
	return tx.Commit()
}

// Search cherche dans tout l'historique (archives comprises) les messages de
// l'interlocuteur et de l'agent : FTS5 classé par pertinence si le pilote le
// permet, sinon LIKE du plus récent au plus ancien.
func (s *Store) Search(ctx context.Context, q session.Query) ([]session.Hit, error) {
	terms := session.Terms(q.Text)
	if len(terms) == 0 {
		return nil, nil
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 10
	}
	var where []string
	var args []any
	var query string
	if s.hasFTS {
		quoted := make([]string, len(terms))
		for i, t := range terms {
			quoted[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"*`
		}
		query = fmt.Sprintf(`SELECT m.key, m.role, m.content, m.created_at FROM %s f JOIN %s m ON m.id = f.rowid WHERE %s MATCH ?`, s.fts, s.msgs, s.fts)
		args = append(args, strings.Join(quoted, " OR "))
	} else {
		query = fmt.Sprintf(`SELECT m.key, m.role, m.content, m.created_at FROM %s m WHERE 1`, s.msgs)
		var like []string
		for _, t := range terms {
			like, args = append(like, "lower(m.content) LIKE ?"), append(args, "%"+t+"%")
		}
		where = append(where, "("+strings.Join(like, " OR ")+")")
	}
	where = append(where, "m.role IN ('user', 'assistant')", "m.content != ''")
	if len(q.Keys) > 0 {
		ph := make([]string, len(q.Keys))
		for i, k := range q.Keys {
			ph[i], args = "?", append(args, k)
		}
		where = append(where, "m.key IN ("+strings.Join(ph, ",")+")")
	}
	query += " AND " + strings.Join(where, " AND ")
	if s.hasFTS {
		query += " ORDER BY f.rank"
	} else {
		query += " ORDER BY m.id DESC"
	}
	query += " LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("session sqlite : recherche : %w", err)
	}
	defer rows.Close()
	var out []session.Hit
	for rows.Next() {
		var h session.Hit
		var role string
		var at any
		if err := rows.Scan(&h.Key, &role, &h.Content, &at); err != nil {
			return nil, err
		}
		h.Role, h.At = llm.Role(role), toTime(at)
		out = append(out, h)
	}
	return out, rows.Err()
}

func toTime(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t.UTC()
	case string:
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999 -0700 MST"} {
			if p, err := time.Parse(layout, t); err == nil {
				return p.UTC()
			}
		}
	}
	return time.Time{}
}

var (
	_ session.Store     = (*Store)(nil)
	_ session.Compactor = (*Store)(nil)
	_ session.Searcher  = (*Store)(nil)
)
