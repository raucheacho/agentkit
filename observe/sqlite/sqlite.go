// Package sqlite implémente observe.Seen sur une base SQL compatible SQLite,
// sans imposer de pilote.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/raucheacho/agentkit/observe"
)

// Seen est un observe.Seen SQLite.
type Seen struct {
	db    *sql.DB
	table string
}

var validPrefix = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// New crée la table si besoin. prefix vide : "agentkit_".
func New(ctx context.Context, db *sql.DB, prefix string) (*Seen, error) {
	if prefix == "" {
		prefix = "agentkit_"
	}
	if !validPrefix.MatchString(prefix) {
		return nil, fmt.Errorf("préfixe de table invalide : %q", prefix)
	}
	s := &Seen{db: db, table: prefix + "seen"}
	for _, q := range []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (key TEXT PRIMARY KEY, seen_at INTEGER NOT NULL)`, s.table),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_at ON %s (seen_at)`, s.table, s.table),
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return nil, fmt.Errorf("observe sqlite : création de la table : %w", err)
		}
	}
	return s, nil
}

func (s *Seen) Seen(ctx context.Context, keys []string) (map[string]bool, error) {
	out := map[string]bool{}
	for start := 0; start < len(keys); start += 500 {
		chunk := keys[start:min(start+500, len(keys))]
		args := make([]any, len(chunk))
		for i, k := range chunk {
			args[i] = k
		}
		rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT key FROM %s WHERE key IN (%s)`, s.table,
			strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")), args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				rows.Close()
				return nil, err
			}
			out[k] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Seen) Mark(ctx context.Context, keys []string, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := fmt.Sprintf(`INSERT INTO %s (key, seen_at) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET seen_at = excluded.seen_at`, s.table)
	for _, k := range keys {
		if _, err := tx.ExecContext(ctx, q, k, at.UnixNano()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Seen) Forget(ctx context.Context, before time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE seen_at < ?`, s.table), before.UnixNano())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

var _ observe.Seen = (*Seen)(nil)
