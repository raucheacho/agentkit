// Package sqlite implémente turn.Queue sur une base SQL compatible SQLite,
// sans imposer de pilote (voir session/sqlite).
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/raucheacho/agentkit/turn"
)

// Store est une turn.Queue SQLite. Les dates sont stockées en nanosecondes
// Unix UTC pour ne dépendre d'aucune conversion de date du pilote.
type Store struct {
	db    *sql.DB
	table string
	mu    sync.Mutex // sérialise Claim/TakeQueued dans le processus
}

var validPrefix = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// New crée la table si besoin. prefix vide : "agentkit_".
func New(ctx context.Context, db *sql.DB, prefix string) (*Store, error) {
	if prefix == "" {
		prefix = "agentkit_"
	}
	if !validPrefix.MatchString(prefix) {
		return nil, fmt.Errorf("préfixe de table invalide : %q", prefix)
	}
	s := &Store{db: db, table: prefix + "turns"}
	for _, q := range []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			seq         INTEGER PRIMARY KEY AUTOINCREMENT,
			id          TEXT NOT NULL UNIQUE,
			session     TEXT NOT NULL,
			source      TEXT NOT NULL,
			channel     TEXT NOT NULL DEFAULT '',
			chat_id     TEXT NOT NULL DEFAULT '',
			sender      TEXT NOT NULL DEFAULT '',
			content     TEXT NOT NULL DEFAULT '',
			note        TEXT NOT NULL DEFAULT '',
			meta        TEXT,
			status      TEXT NOT NULL,
			reply       TEXT NOT NULL DEFAULT '',
			error       TEXT NOT NULL DEFAULT '',
			merged_into TEXT NOT NULL DEFAULT '',
			created_at  INTEGER NOT NULL,
			not_before  INTEGER NOT NULL DEFAULT 0,
			started_at  INTEGER NOT NULL DEFAULT 0,
			finished_at INTEGER NOT NULL DEFAULT 0)`, s.table),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_status ON %s (status, seq)`, s.table, s.table),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_session ON %s (session, seq)`, s.table, s.table),
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return nil, fmt.Errorf("turn sqlite : création de la table : %w", err)
		}
	}
	return s, nil
}

const cols = `id, session, source, channel, chat_id, sender, content, note, meta, status, reply, error, merged_into, created_at, not_before, started_at, finished_at`

func nanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func fromNanos(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

type scanner interface{ Scan(...any) error }

func scanTurn(sc scanner) (*turn.Turn, error) {
	var t turn.Turn
	var source, status string
	var meta sql.NullString
	var created, notBefore, started, finished int64
	if err := sc.Scan(&t.ID, &t.Session, &source, &t.Channel, &t.ChatID, &t.Sender, &t.Content, &t.Note, &meta,
		&status, &t.Reply, &t.Error, &t.MergedInto, &created, &notBefore, &started, &finished); err != nil {
		return nil, err
	}
	t.Source, t.Status = turn.Source(source), turn.Status(status)
	t.CreatedAt, t.NotBefore, t.StartedAt, t.FinishedAt = fromNanos(created), fromNanos(notBefore), fromNanos(started), fromNanos(finished)
	if meta.Valid && meta.String != "" {
		if err := json.Unmarshal([]byte(meta.String), &t.Meta); err != nil {
			return nil, fmt.Errorf("turn sqlite : meta illisible : %w", err)
		}
	}
	return &t, nil
}

func (s *Store) Enqueue(ctx context.Context, t *turn.Turn) error {
	if t.Session == "" && t.Channel == "" && t.ChatID == "" {
		return errors.New("turn : session obligatoire")
	}
	if t.ID == "" {
		t.ID = turn.NewID()
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	if t.Source == "" {
		t.Source = turn.SourceUser
	}
	if t.Session == "" {
		t.Session = t.Channel + ":" + t.ChatID
	}
	t.Status = turn.Queued
	var meta any
	if len(t.Meta) > 0 {
		b, err := json.Marshal(t.Meta)
		if err != nil {
			return err
		}
		meta = string(b)
	}
	_, err := s.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s (%s) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, s.table, cols),
		t.ID, t.Session, string(t.Source), t.Channel, t.ChatID, t.Sender, t.Content, t.Note, meta,
		string(t.Status), t.Reply, t.Error, t.MergedInto, nanos(t.CreatedAt), nanos(t.NotBefore), 0, 0)
	if err != nil {
		return fmt.Errorf("turn sqlite : ajout : %w", err)
	}
	return nil
}

func (s *Store) Claim(ctx context.Context, now time.Time, max int) ([]*turn.Turn, error) {
	if max <= 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM %s
		WHERE status = 'queued' AND (not_before = 0 OR not_before <= ?)
		AND session NOT IN (SELECT session FROM %s WHERE status = 'running')
		ORDER BY seq`, cols, s.table, s.table), now.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("turn sqlite : sélection : %w", err)
	}
	seen := map[string]bool{}
	var out []*turn.Turn
	for rows.Next() && len(out) < max {
		t, err := scanTurn(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if seen[t.Session] {
			continue
		}
		seen[t.Session] = true
		out = append(out, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, t := range out {
		t.Status, t.StartedAt = turn.Running, now.UTC()
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET status = 'running', started_at = ? WHERE id = ? AND status = 'queued'`, s.table),
			now.UnixNano(), t.ID); err != nil {
			return nil, fmt.Errorf("turn sqlite : démarrage : %w", err)
		}
	}
	return out, tx.Commit()
}

func (s *Store) TakeQueued(ctx context.Context, session, channel, into string) ([]*turn.Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM %s
		WHERE session = ? AND channel = ? AND source = 'user' AND status = 'queued' ORDER BY seq`, cols, s.table), session, channel)
	if err != nil {
		return nil, err
	}
	var out []*turn.Turn
	for rows.Next() {
		t, err := scanTurn(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	for _, t := range out {
		t.Status, t.MergedInto, t.FinishedAt = turn.Merged, into, now
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET status = 'merged', merged_into = ?, finished_at = ? WHERE id = ?`, s.table),
			into, now.UnixNano(), t.ID); err != nil {
			return nil, err
		}
	}
	return out, tx.Commit()
}

func (s *Store) Finish(ctx context.Context, id string, status turn.Status, reply, errMsg string) error {
	res, err := s.db.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET status = ?, reply = ?, error = ?, finished_at = ? WHERE id = ? AND status = 'running'`, s.table),
		string(status), reply, errMsg, time.Now().UTC().UnixNano(), id)
	if err != nil {
		return fmt.Errorf("turn sqlite : fin de %s : %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("turn : %s n'est pas en cours", id)
	}
	return nil
}

func (s *Store) Cancel(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET status = 'cancelled', finished_at = ? WHERE id = ? AND status = 'queued'`, s.table),
		time.Now().UTC().UnixNano(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) Get(ctx context.Context, id string) (*turn.Turn, error) {
	t, err := scanTurn(s.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM %s WHERE id = ?`, cols, s.table), id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

func (s *Store) List(ctx context.Context, f turn.Filter) ([]*turn.Turn, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	var where []string
	var args []any
	if f.Session != "" {
		where = append(where, "session = ?")
		args = append(args, f.Session)
	}
	if len(f.Status) > 0 {
		ph := make([]string, len(f.Status))
		for i, st := range f.Status {
			ph[i] = "?"
			args = append(args, string(st))
		}
		where = append(where, "status IN ("+strings.Join(ph, ",")+")")
	}
	q := fmt.Sprintf(`SELECT %s FROM %s`, cols, s.table)
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY seq DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*turn.Turn
	for rows.Next() {
		t, err := scanTurn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) Recover(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET status = 'failed', error = ?, finished_at = ? WHERE status = 'running'`, s.table),
		turn.ErrInterrupted, time.Now().UTC().UnixNano())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

var _ turn.Queue = (*Store)(nil)
