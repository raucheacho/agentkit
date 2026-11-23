// Package sqlite implémente schedule.Store sur une base SQL compatible
// SQLite, sans imposer de pilote.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/raucheacho/agentkit/schedule"
	"github.com/raucheacho/agentkit/tool"
)

// Store est un schedule.Store SQLite.
type Store struct {
	db    *sql.DB
	table string
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
	s := &Store{db: db, table: prefix + "jobs"}
	for _, q := range []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			seq          INTEGER PRIMARY KEY AUTOINCREMENT,
			id           TEXT NOT NULL UNIQUE,
			name         TEXT NOT NULL,
			spec         TEXT NOT NULL,
			task         TEXT NOT NULL,
			conversation TEXT NOT NULL,
			session      TEXT NOT NULL,
			silent       INTEGER NOT NULL DEFAULT 0,
			enabled      INTEGER NOT NULL,
			next_run     INTEGER NOT NULL DEFAULT 0,
			last_run     INTEGER NOT NULL DEFAULT 0,
			created_at   INTEGER NOT NULL)`, s.table),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_due ON %s (enabled, next_run)`, s.table, s.table),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_session ON %s (session, seq)`, s.table, s.table),
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return nil, fmt.Errorf("schedule sqlite : création de la table : %w", err)
		}
	}
	return s, nil
}

const cols = `id, name, spec, task, conversation, silent, enabled, next_run, last_run, created_at`

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

func scan(sc scanner) (*schedule.Job, error) {
	var j schedule.Job
	var conv string
	var next, last, created int64
	if err := sc.Scan(&j.ID, &j.Name, &j.Spec, &j.Task, &conv, &j.Silent, &j.Enabled, &next, &last, &created); err != nil {
		return nil, err
	}
	var c tool.Conversation
	if err := json.Unmarshal([]byte(conv), &c); err != nil {
		return nil, fmt.Errorf("schedule sqlite : conversation illisible : %w", err)
	}
	j.Conversation = c
	j.NextRun, j.LastRun, j.CreatedAt = fromNanos(next), fromNanos(last), fromNanos(created)
	return &j, nil
}

func (s *Store) Create(ctx context.Context, j *schedule.Job) error {
	schedule.PrepareJob(j, time.Now())
	conv, err := json.Marshal(j.Conversation)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s (%s, session) VALUES (?,?,?,?,?,?,?,?,?,?,?)`, s.table, cols),
		j.ID, j.Name, j.Spec, j.Task, string(conv), j.Silent, j.Enabled, nanos(j.NextRun), nanos(j.LastRun), nanos(j.CreatedAt), j.Conversation.Session)
	if err != nil {
		return fmt.Errorf("schedule sqlite : création : %w", err)
	}
	return nil
}

func (s *Store) Get(ctx context.Context, id string) (*schedule.Job, error) {
	j, err := scan(s.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM %s WHERE id = ?`, cols, s.table), id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

func (s *Store) query(ctx context.Context, q string, args ...any) ([]*schedule.Job, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*schedule.Job
	for rows.Next() {
		j, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) List(ctx context.Context, f schedule.Filter) ([]*schedule.Job, error) {
	if f.Limit <= 0 {
		f.Limit = 100
	}
	if f.Session != "" {
		return s.query(ctx, fmt.Sprintf(`SELECT %s FROM %s WHERE session = ? ORDER BY seq LIMIT ?`, cols, s.table), f.Session, f.Limit)
	}
	return s.query(ctx, fmt.Sprintf(`SELECT %s FROM %s ORDER BY seq LIMIT ?`, cols, s.table), f.Limit)
}

func (s *Store) Delete(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE id = ?`, s.table), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *Store) SetEnabled(ctx context.Context, id string, enabled bool, next time.Time) error {
	res, err := s.db.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET enabled = ?, next_run = ? WHERE id = ?`, s.table), enabled, nanos(next), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return schedule.ErrNotFound
	}
	return nil
}

func (s *Store) Due(ctx context.Context, now time.Time, limit int) ([]*schedule.Job, error) {
	if limit <= 0 {
		limit = 100
	}
	return s.query(ctx, fmt.Sprintf(`SELECT %s FROM %s WHERE enabled = 1 AND next_run > 0 AND next_run <= ? ORDER BY next_run LIMIT ?`, cols, s.table),
		now.UnixNano(), limit)
}

// Advance s'appuie sur une mise à jour conditionnelle (next_run = slot) :
// une échéance n'est consommée qu'une fois, même entre plusieurs processus.
func (s *Store) Advance(ctx context.Context, id string, slot, next time.Time, ran bool) (bool, error) {
	q := fmt.Sprintf(`UPDATE %s SET next_run = ?, enabled = CASE WHEN ? = 0 THEN 0 ELSE enabled END`, s.table)
	args := []any{nanos(next), nanos(next)}
	if ran {
		q += `, last_run = ?`
		args = append(args, nanos(slot))
	}
	q += ` WHERE id = ? AND next_run = ?`
	args = append(args, id, nanos(slot))
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return false, fmt.Errorf("schedule sqlite : échéance de %s : %w", id, err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

var _ schedule.Store = (*Store)(nil)
