package event

import (
	"slices"
	"strings"
	"sync"

	"github.com/raucheacho/agentkit/llm"
)

// Price est le tarif d'un modèle, par million de jetons.
type Price struct {
	Input float64 `json:"input"`
	// CachedInput : jetons d'entrée servis depuis le cache (0 : tarif Input).
	CachedInput float64 `json:"cached_input"`
	Output      float64 `json:"output"`
}

// Cost renvoie le coût d'un usage.
func (p Price) Cost(u llm.Usage) float64 {
	cached := min(u.CachedTokens, u.InputTokens)
	cachedPrice := p.CachedInput
	if cachedPrice == 0 {
		cachedPrice = p.Input
	}
	return (float64(u.InputTokens-cached)*p.Input + float64(cached)*cachedPrice + float64(u.OutputTokens)*p.Output) / 1e6
}

// Totals cumule des appels au modèle.
type Totals struct {
	Calls int       `json:"calls"`
	Usage llm.Usage `json:"usage"`
	// Cost : coût des appels dont le modèle a un tarif connu.
	Cost float64 `json:"cost"`
	// Unpriced : appels dont le modèle n'a pas de tarif.
	Unpriced int `json:"unpriced"`
}

func (t *Totals) add(u llm.Usage, cost float64, priced bool) {
	t.Calls++
	t.Usage = t.Usage.Add(u)
	if priced {
		t.Cost += cost
	} else {
		t.Unpriced++
	}
}

// CacheRate renvoie la part des jetons d'entrée servis depuis le cache.
func (t Totals) CacheRate() float64 {
	if t.Usage.InputTokens == 0 {
		return 0
	}
	return float64(t.Usage.CachedTokens) / float64(t.Usage.InputTokens)
}

// Meter compte l'usage et le coût des appels au modèle, par session et par
// modèle. À brancher avec Bus.Attach(meter.Observe).
type Meter struct {
	mu        sync.Mutex
	prices    map[string]Price
	total     Totals
	bySession map[string]*Totals
	byModel   map[string]*Totals
}

// NewMeter crée un compteur. Un modèle sans tarif exact prend celui du plus
// long préfixe connu (« gpt-4o » couvre « gpt-4o-2024-08-06 »).
func NewMeter(prices map[string]Price) *Meter {
	return &Meter{prices: prices, bySession: map[string]*Totals{}, byModel: map[string]*Totals{}}
}

func (m *Meter) price(model string) (Price, bool) {
	if p, ok := m.prices[model]; ok {
		return p, true
	}
	best, found := "", false
	for name := range m.prices {
		if strings.HasPrefix(model, name) && len(name) > len(best) {
			best, found = name, true
		}
	}
	return m.prices[best], found
}

// Observe compte un événement Model (les autres sont ignorés).
func (m *Meter) Observe(ev Event) {
	if ev.Kind != Model {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p, priced := m.price(ev.Model)
	cost := p.Cost(ev.Usage)
	m.total.add(ev.Usage, cost, priced)
	for _, kt := range []struct {
		key   string
		table map[string]*Totals
	}{{ev.Session, m.bySession}, {ev.Model, m.byModel}} {
		t := kt.table[kt.key]
		if t == nil {
			t = &Totals{}
			kt.table[kt.key] = t
		}
		t.add(ev.Usage, cost, priced)
	}
}

// Total renvoie le cumul général.
func (m *Meter) Total() Totals {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.total
}

// Session renvoie le cumul d'une session.
func (m *Meter) Session(key string) Totals {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t := m.bySession[key]; t != nil {
		return *t
	}
	return Totals{}
}

// Snapshot est l'état du compteur.
type Snapshot struct {
	Total    Totals            `json:"total"`
	Models   map[string]Totals `json:"models"`
	Sessions map[string]Totals `json:"sessions"`
	// Top : les sessions les plus coûteuses (puis les plus consommatrices).
	Top []string `json:"top"`
}

// Snapshot renvoie une copie du compteur ; top borne la liste Top (0 : 10).
func (m *Meter) Snapshot(top int) Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	if top <= 0 {
		top = 10
	}
	s := Snapshot{Total: m.total, Models: map[string]Totals{}, Sessions: map[string]Totals{}}
	for k, v := range m.byModel {
		s.Models[k] = *v
	}
	for k, v := range m.bySession {
		s.Sessions[k] = *v
		s.Top = append(s.Top, k)
	}
	slices.SortFunc(s.Top, func(a, b string) int {
		ta, tb := m.bySession[a], m.bySession[b]
		switch {
		case ta.Cost != tb.Cost:
			if ta.Cost > tb.Cost {
				return -1
			}
			return 1
		case ta.Usage.InputTokens+ta.Usage.OutputTokens != tb.Usage.InputTokens+tb.Usage.OutputTokens:
			return (tb.Usage.InputTokens + tb.Usage.OutputTokens) - (ta.Usage.InputTokens + ta.Usage.OutputTokens)
		}
		return strings.Compare(a, b)
	})
	if len(s.Top) > top {
		s.Top = s.Top[:top]
	}
	return s
}
