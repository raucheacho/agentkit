package turn

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Memory est une Queue en mémoire, pour les tests et les agents éphémères.
type Memory struct {
	mu    sync.Mutex
	turns map[string]*Turn
	seq   map[string]int // ordre d'arrivée
	n     int
}

// NewMemory crée une Queue en mémoire.
func NewMemory() *Memory { return &Memory{turns: map[string]*Turn{}, seq: map[string]int{}} }

func clone(t *Turn) *Turn {
	c := *t
	if t.Meta != nil {
		c.Meta = make(map[string]string, len(t.Meta))
		for k, v := range t.Meta {
			c.Meta[k] = v
		}
	}
	return &c
}

func (q *Memory) Enqueue(_ context.Context, t *Turn) error {
	if t.Session == "" && t.Channel == "" && t.ChatID == "" {
		return errors.New("turn : session obligatoire")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	prepare(t, time.Now().UTC())
	if _, dup := q.turns[t.ID]; dup {
		return fmt.Errorf("turn : %s existe déjà", t.ID)
	}
	q.n++
	q.turns[t.ID] = clone(t)
	q.seq[t.ID] = q.n
	return nil
}

func (q *Memory) ordered() []*Turn {
	out := make([]*Turn, 0, len(q.turns))
	for _, t := range q.turns {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return q.seq[out[i].ID] < q.seq[out[j].ID] })
	return out
}

func (q *Memory) Claim(_ context.Context, now time.Time, max int) ([]*Turn, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	busy := map[string]bool{}
	for _, t := range q.turns {
		if t.Status == Running {
			busy[t.Session] = true
		}
	}
	var out []*Turn
	for _, t := range q.ordered() {
		if len(out) >= max {
			break
		}
		if t.Status != Queued || busy[t.Session] || (!t.NotBefore.IsZero() && t.NotBefore.After(now)) {
			continue
		}
		busy[t.Session] = true
		t.Status, t.StartedAt = Running, now
		out = append(out, clone(t))
	}
	return out, nil
}

func (q *Memory) TakeQueued(_ context.Context, session, channel, into string) ([]*Turn, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []*Turn
	now := time.Now().UTC()
	for _, t := range q.ordered() {
		if t.Session == session && t.Channel == channel && t.Source == SourceUser && t.Status == Queued {
			t.Status, t.MergedInto, t.FinishedAt = Merged, into, now
			out = append(out, clone(t))
		}
	}
	return out, nil
}

func (q *Memory) Finish(_ context.Context, id string, status Status, reply, errMsg string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	t, ok := q.turns[id]
	if !ok {
		return fmt.Errorf("turn : %s introuvable", id)
	}
	if t.Status != Running {
		return fmt.Errorf("turn : %s n'est pas en cours (%s)", id, t.Status)
	}
	t.Status, t.Reply, t.Error, t.FinishedAt = status, reply, errMsg, time.Now().UTC()
	return nil
}

func (q *Memory) Cancel(_ context.Context, id string) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	t, ok := q.turns[id]
	if !ok || t.Status != Queued {
		return false, nil
	}
	t.Status, t.FinishedAt = Cancelled, time.Now().UTC()
	return true, nil
}

func (q *Memory) Get(_ context.Context, id string) (*Turn, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if t, ok := q.turns[id]; ok {
		return clone(t), nil
	}
	return nil, nil
}

func (q *Memory) List(_ context.Context, f Filter) ([]*Turn, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	want := map[Status]bool{}
	for _, s := range f.Status {
		want[s] = true
	}
	all := q.ordered()
	var out []*Turn
	for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
		t := all[i]
		if (f.Session == "" || t.Session == f.Session) && (len(want) == 0 || want[t.Status]) {
			out = append(out, clone(t))
		}
	}
	return out, nil
}

func (q *Memory) Recover(_ context.Context) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for _, t := range q.turns {
		if t.Status == Running {
			t.Status, t.Error, t.FinishedAt = Failed, ErrInterrupted, time.Now().UTC()
			n++
		}
	}
	return n, nil
}

// ErrInterrupted est l'erreur d'un tour interrompu par un arrêt du processus.
const ErrInterrupted = "interrompu par un arrêt du processus"
