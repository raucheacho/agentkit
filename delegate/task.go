package delegate

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

// TaskStatus est l'état d'une tâche de fond.
type TaskStatus string

const (
	TaskRunning   TaskStatus = "running"
	TaskDone      TaskStatus = "done"
	TaskFailed    TaskStatus = "failed"
	TaskCancelled TaskStatus = "cancelled"
)

// Task est une tâche de fond confiée à un sous-agent.
type Task struct {
	ID      string
	Goal    string
	Context string
	// Conversation de l'agent qui a lancé la tâche : son résultat y revient.
	Conversation tool.Conversation
	Status       TaskStatus
	Result       string
	Error        string
	CreatedAt    time.Time
	FinishedAt   time.Time
}

// TaskFilter sélectionne des tâches.
type TaskFilter struct {
	Session string
	Status  []TaskStatus
	Limit   int // 0 : 100
}

// ErrNotRunning : le Delegator ne tourne pas (Run n'a pas été lancé).
var ErrNotRunning = errors.New("delegate : tâches de fond indisponibles (Run n'est pas lancé)")

// Store conserve les tâches de fond.
type Store interface {
	// Create enregistre une tâche (ID et CreatedAt remplis s'ils sont vides,
	// Status Running).
	Create(ctx context.Context, t *Task) error
	// Finish termine une tâche en cours ; false si elle était déjà terminée
	// (annulée entre-temps, par exemple).
	Finish(ctx context.Context, id string, status TaskStatus, result, errMsg string, at time.Time) (bool, error)
	// Get renvoie une tâche ; nil si elle n'existe pas.
	Get(ctx context.Context, id string) (*Task, error)
	// List renvoie des tâches, de la plus récente à la plus ancienne.
	List(ctx context.Context, f TaskFilter) ([]*Task, error)
	// Recover passe en Failed les tâches restées Running après un arrêt
	// brutal et les renvoie : elles ne sont jamais relancées.
	Recover(ctx context.Context, at time.Time) ([]*Task, error)
}

// PrepareTask remplit les champs par défaut d'une tâche neuve (pour les
// implémentations de Store).
func PrepareTask(t *Task, now time.Time) {
	if t.ID == "" {
		t.ID = turn.NewID()
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now.UTC()
	}
	t.Status = TaskRunning
	t.Result, t.Error, t.FinishedAt = "", "", time.Time{}
}

// MemoryStore est un Store en mémoire.
type MemoryStore struct {
	mu    sync.Mutex
	tasks map[string]*Task
	seq   map[string]int
	n     int
}

// NewMemoryStore crée un Store en mémoire.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{tasks: map[string]*Task{}, seq: map[string]int{}}
}

func (m *MemoryStore) Create(_ context.Context, t *Task) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	PrepareTask(t, time.Now())
	if _, ok := m.tasks[t.ID]; ok {
		return errors.New("delegate : tâche déjà existante : " + t.ID)
	}
	c := *t
	m.tasks[t.ID] = &c
	m.n++
	m.seq[t.ID] = m.n
	return nil
}

func (m *MemoryStore) Finish(_ context.Context, id string, status TaskStatus, result, errMsg string, at time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok || t.Status != TaskRunning {
		return false, nil
	}
	t.Status, t.Result, t.Error, t.FinishedAt = status, result, errMsg, at.UTC()
	return true, nil
}

func (m *MemoryStore) Get(_ context.Context, id string) (*Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return nil, nil
	}
	c := *t
	return &c, nil
}

func (m *MemoryStore) List(_ context.Context, f TaskFilter) ([]*Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Task
	for _, t := range m.tasks {
		if f.Session != "" && t.Conversation.Session != f.Session {
			continue
		}
		if len(f.Status) > 0 && !slices.Contains(f.Status, t.Status) {
			continue
		}
		c := *t
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return m.seq[out[i].ID] > m.seq[out[j].ID] })
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemoryStore) Recover(_ context.Context, at time.Time) ([]*Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Task
	for _, t := range m.tasks {
		if t.Status == TaskRunning {
			t.Status, t.Error, t.FinishedAt = TaskFailed, Interrupted, at.UTC()
			c := *t
			out = append(out, &c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return m.seq[out[i].ID] < m.seq[out[j].ID] })
	return out, nil
}

// Interrupted est l'erreur d'une tâche coupée par un arrêt brutal.
const Interrupted = "interrompue par un redémarrage"
