package schedule

import (
	"context"
	"errors"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/raucheacho/agentkit/tool"
	"github.com/raucheacho/agentkit/turn"
)

// Job est une tâche planifiée : à chaque échéance, un tour source=schedule
// porte sa consigne dans la conversation qui l'a créée.
type Job struct {
	ID   string
	Name string
	// Spec est la planification canonique (voir Parse).
	Spec string
	// Task est la consigne donnée à l'agent à chaque exécution.
	Task string
	// Conversation : où le tour s'exécute et répond.
	Conversation tool.Conversation
	// Silent : le tour peut décider de ne rien dire (turn.MetaSilence).
	Silent  bool
	Enabled bool
	// NextRun : prochaine échéance (zéro : plus aucune) ; LastRun : dernière exécutée.
	NextRun   time.Time
	LastRun   time.Time
	CreatedAt time.Time
}

// Filter sélectionne des tâches.
type Filter struct {
	Session string
	Limit   int // 0 : 100
}

// ErrNotFound : tâche inconnue.
var ErrNotFound = errors.New("schedule : tâche inconnue")

// Store conserve les tâches planifiées.
type Store interface {
	// Create enregistre une tâche (ID et CreatedAt remplis s'ils sont vides).
	Create(ctx context.Context, j *Job) error
	// Get renvoie une tâche ; nil si elle n'existe pas.
	Get(ctx context.Context, id string) (*Job, error)
	// List renvoie des tâches, de la plus ancienne à la plus récente.
	List(ctx context.Context, f Filter) ([]*Job, error)
	// Delete supprime une tâche ; false si elle n'existait pas.
	Delete(ctx context.Context, id string) (bool, error)
	// SetEnabled active ou suspend une tâche et fixe sa prochaine échéance
	// (ErrNotFound si elle n'existe pas).
	SetEnabled(ctx context.Context, id string, enabled bool, next time.Time) error
	// Due renvoie les tâches actives dont l'échéance est atteinte.
	Due(ctx context.Context, now time.Time, limit int) ([]*Job, error)
	// Advance passe l'échéance slot à next, seulement si la tâche est encore
	// à slot : une échéance n'est prise qu'une fois, même par plusieurs
	// processus. ran : l'échéance a été exécutée (LastRun = slot). Un next
	// nul désactive la tâche.
	Advance(ctx context.Context, id string, slot, next time.Time, ran bool) (bool, error)
}

// PrepareJob remplit les champs par défaut d'une tâche neuve (pour les
// implémentations de Store).
func PrepareJob(j *Job, now time.Time) {
	if j.ID == "" {
		j.ID = turn.NewID()
	}
	if j.CreatedAt.IsZero() {
		j.CreatedAt = now.UTC()
	}
}

// MemoryStore est un Store en mémoire.
type MemoryStore struct {
	mu   sync.Mutex
	jobs map[string]*Job
	seq  map[string]int
	n    int
}

// NewMemoryStore crée un Store en mémoire.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{jobs: map[string]*Job{}, seq: map[string]int{}}
}

func (m *MemoryStore) Create(_ context.Context, j *Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	PrepareJob(j, time.Now())
	if _, ok := m.jobs[j.ID]; ok {
		return errors.New("schedule : tâche déjà existante : " + j.ID)
	}
	c := *j
	m.jobs[j.ID] = &c
	m.n++
	m.seq[j.ID] = m.n
	return nil
}

func (m *MemoryStore) Get(_ context.Context, id string) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil, nil
	}
	c := *j
	return &c, nil
}

func (m *MemoryStore) List(_ context.Context, f Filter) ([]*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Job
	for _, j := range m.jobs {
		if f.Session == "" || j.Conversation.Session == f.Session {
			c := *j
			out = append(out, &c)
		}
	}
	sort.Slice(out, func(a, b int) bool { return m.seq[out[a].ID] < m.seq[out[b].ID] })
	if f.Limit <= 0 {
		f.Limit = 100
	}
	if len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

func (m *MemoryStore) Delete(_ context.Context, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.jobs[id]
	delete(m.jobs, id)
	return ok, nil
}

func (m *MemoryStore) SetEnabled(_ context.Context, id string, enabled bool, next time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return ErrNotFound
	}
	j.Enabled, j.NextRun = enabled, next
	return nil
}

func (m *MemoryStore) Due(_ context.Context, now time.Time, limit int) ([]*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Job
	for _, j := range m.jobs {
		if j.Enabled && !j.NextRun.IsZero() && !j.NextRun.After(now) {
			c := *j
			out = append(out, &c)
		}
	}
	slices.SortFunc(out, func(a, b *Job) int { return a.NextRun.Compare(b.NextRun) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemoryStore) Advance(_ context.Context, id string, slot, next time.Time, ran bool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok || !j.NextRun.Equal(slot) {
		return false, nil
	}
	j.NextRun = next
	if ran {
		j.LastRun = slot
	}
	if next.IsZero() {
		j.Enabled = false
	}
	return true, nil
}

var _ Store = (*MemoryStore)(nil)
