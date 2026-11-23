package tool

import (
	"context"
	"encoding/json"
	"fmt"
)

// Option règle un outil créé par Func ou New.
type Option func(*Spec)

// WithEffect fixe l'effet de l'outil (Read par défaut).
func WithEffect(e Effect) Option { return func(s *Spec) { s.Effect = e } }

// MainOnly réserve l'outil à l'agent principal (voir Spec.MainOnly).
func MainOnly() Option { return func(s *Spec) { s.MainOnly = true } }

// WithSchema remplace le schéma des paramètres.
func WithSchema(schema json.RawMessage) Option { return func(s *Spec) { s.Parameters = schema } }

type funcTool struct {
	spec Spec
	run  func(ctx context.Context, call Call) (Result, error)
}

func (f *funcTool) Spec() Spec                                      { return f.spec }
func (f *funcTool) Run(ctx context.Context, c Call) (Result, error) { return f.run(ctx, c) }

// Func crée un outil à partir d'une fonction et d'un schéma explicite.
func Func(name, description string, schema json.RawMessage, run func(ctx context.Context, call Call) (Result, error), opts ...Option) Tool {
	s := Spec{Name: name, Description: description, Parameters: schema}
	for _, o := range opts {
		o(&s)
	}
	return &funcTool{spec: s, run: run}
}

// New crée un outil typé : le schéma est déduit de In (voir SchemaOf) et les
// arguments sont décodés dans In avant l'appel.
//
//	type bookIn struct {
//		Date string `json:"date" desc:"jour au format AAAA-MM-JJ"`
//		Note string `json:"note,omitempty"`
//	}
//	book := tool.New("book", "Réserve un créneau.", func(ctx context.Context, in bookIn) (tool.Result, error) { … },
//		tool.WithEffect(tool.Sensitive))
func New[In any](name, description string, run func(ctx context.Context, in In) (Result, error), opts ...Option) Tool {
	var zero In
	schema, err := SchemaOf(zero)
	if err != nil {
		panic(fmt.Sprintf("tool %s: %v", name, err))
	}
	return Func(name, description, schema, func(ctx context.Context, c Call) (Result, error) {
		var in In
		if err := c.Decode(&in); err != nil {
			return Result{}, fmt.Errorf("arguments invalides: %w", err)
		}
		return run(ctx, in)
	}, opts...)
}
