package llm

import (
	"context"
	"math"
)

// Embedder transforme des textes en vecteurs (recherche sémantique dans la
// mémoire et l'historique). Les vecteurs renvoyés sont dans l'ordre des textes.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// EmbedderFunc adapte une fonction en Embedder.
type EmbedderFunc func(ctx context.Context, texts []string) ([][]float32, error)

func (f EmbedderFunc) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return f(ctx, texts)
}

// Cosine renvoie la similarité cosinus de deux vecteurs (0 si l'un est nul ou
// si leurs tailles diffèrent).
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
