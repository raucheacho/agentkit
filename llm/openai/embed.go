package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/raucheacho/agentkit/llm"
)

// Embedder appelle /embeddings d'une API compatible OpenAI (OpenAI, Ollama
// /v1, vLLM, OpenRouter…).
type Embedder struct {
	p     *Provider
	model string
}

// NewEmbedder crée un Embedder ; model est le modèle d'embeddings
// (ex. "text-embedding-3-small", "nomic-embed-text").
func NewEmbedder(cfg Config, model string) *Embedder {
	return &Embedder{p: New(cfg), model: model}
}

// Embed renvoie un vecteur par texte, dans l'ordre.
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	data, err := json.Marshal(map[string]any{"model": e.model, "input": texts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.p.cfg.BaseURL+"/embeddings", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	e.p.headers(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", e.p.cfg.Name, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, e.p.statusError(resp.StatusCode, raw)
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%s: décodage des embeddings: %w", e.p.cfg.Name, err)
	}
	vecs := make([][]float32, len(texts))
	for i, d := range out.Data {
		idx := d.Index
		if idx < 0 || idx >= len(texts) {
			idx = i
		}
		vecs[idx] = d.Embedding
	}
	for i, v := range vecs {
		if v == nil {
			return nil, fmt.Errorf("%s: embedding manquant pour le texte %d", e.p.cfg.Name, i)
		}
	}
	return vecs, nil
}

var _ llm.Embedder = (*Embedder)(nil)
