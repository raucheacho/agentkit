package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// Toolset groupe des outils qui vont ensemble (« web », « agenda »,
// « fichiers »). Available dit si le groupe est utilisable maintenant
// (clé configurée, module activé, service joignable) : un groupe indisponible
// n'est pas montré au modèle. nil = toujours disponible.
type Toolset struct {
	Name      string
	Tools     []Tool
	Available func(ctx context.Context) bool
}

type entry struct {
	tool   Tool
	spec   Spec
	schema map[string]any // schéma décodé, pour la validation
	set    *Toolset
}

// Registry est l'ensemble des outils connus d'un agent. L'ordre
// d'enregistrement est conservé : la liste envoyée au modèle reste identique
// d'un tour à l'autre (cache de prompt).
type Registry struct {
	mu    sync.RWMutex
	byKey map[string]*entry
	order []string
}

// NewRegistry crée un registre vide.
func NewRegistry() *Registry { return &Registry{byKey: map[string]*entry{}} }

var validName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// Add enregistre des outils toujours disponibles.
func (r *Registry) Add(tools ...Tool) error {
	return r.AddSet(Toolset{Tools: tools})
}

// AddSet enregistre un groupe d'outils.
func (r *Registry) AddSet(set Toolset) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := set
	for _, t := range set.Tools {
		spec := t.Spec()
		if !validName.MatchString(spec.Name) {
			return fmt.Errorf("nom d'outil invalide : %q", spec.Name)
		}
		if _, dup := r.byKey[spec.Name]; dup {
			return fmt.Errorf("outil déjà enregistré : %s", spec.Name)
		}
		if len(spec.Parameters) == 0 {
			spec.Parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		var schema map[string]any
		if err := json.Unmarshal(spec.Parameters, &schema); err != nil {
			return fmt.Errorf("outil %s : schéma invalide : %w", spec.Name, err)
		}
		r.byKey[spec.Name] = &entry{tool: t, spec: spec, schema: schema, set: &s}
		r.order = append(r.order, spec.Name)
	}
	return nil
}

// Get renvoie un outil par son nom exact.
func (r *Registry) Get(name string) (Tool, Spec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.byKey[name]
	if !ok {
		return nil, Spec{}, false
	}
	return e.tool, e.spec, true
}

// Specs renvoie, dans l'ordre d'enregistrement, les outils disponibles
// retenus par keep (nil = tous).
func (r *Registry) Specs(ctx context.Context, keep func(Spec) bool) []Spec {
	r.mu.RLock()
	entries := make([]*entry, 0, len(r.order))
	for _, n := range r.order {
		entries = append(entries, r.byKey[n])
	}
	r.mu.RUnlock()

	avail := map[*Toolset]bool{}
	var out []Spec
	for _, e := range entries {
		ok, seen := avail[e.set]
		if !seen {
			ok = e.set.Available == nil || e.set.Available(ctx)
			avail[e.set] = ok
		}
		if ok && (keep == nil || keep(e.spec)) {
			out = append(out, e.spec)
		}
	}
	return out
}

// Validate vérifie des arguments contre le schéma de l'outil name, après
// avoir corrigé les nombres et booléens envoyés en texte (voir Coerce) :
// args est modifié sur place.
func (r *Registry) Validate(name string, args map[string]any) error {
	r.mu.RLock()
	e, ok := r.byKey[name]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("outil inconnu : %s", name)
	}
	Coerce(args, e.schema)
	return Validate(args, e.schema)
}

// Resolve renvoie le nom réel d'un outil parmi specs, en réparant les
// variantes que les modèles émettent (« functions.x », casse, tirets, jetons
// de format collés au nom). "" si aucun ne correspond : un modèle ne peut
// jamais appeler un outil qui ne lui a pas été montré.
func Resolve(raw string, specs []Spec) string {
	for _, s := range specs {
		if s.Name == raw {
			return raw
		}
	}
	n := strings.TrimSpace(raw)
	if i := strings.Index(n, "<|"); i >= 0 {
		n = n[:i]
	}
	n = strings.TrimPrefix(n, "functions.")
	n = strings.TrimPrefix(n, "tools.")
	n = strings.ToLower(strings.NewReplacer("-", "_", " ", "_").Replace(n))
	for _, s := range specs {
		if strings.ToLower(s.Name) == n {
			return s.Name
		}
	}
	return ""
}
