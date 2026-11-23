// Package llm définit le contrat entre un agent et un modèle de langage.
//
// Le package ne dépend d'aucun fournisseur : un fournisseur (OpenAI, Groq,
// Ollama, Mistral…) implémente Provider dans son propre sous-package, et
// l'application choisit lequel assembler.
package llm

import (
	"context"
	"encoding/json"
)

// Role d'un message dans une conversation.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message est un message de conversation, dans un format neutre.
//
// ToolCalls est rempli sur un message assistant qui appelle des outils ;
// ToolCallID rattache un message tool à l'appel auquel il répond.
// Reasoning garde le raisonnement exposé par certains modèles : il est
// conservé pour l'historique mais n'est jamais montré à l'interlocuteur.
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
	Reasoning  string     `json:"reasoning,omitempty"`
}

// ToolCall est un appel d'outil émis par le modèle. Arguments est le JSON brut
// tel que le modèle l'a écrit, éventuellement invalide.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolSpec décrit un outil au modèle. Parameters est un JSON Schema complet,
// transmis tel quel (objets imbriqués, items, enums…).
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolChoice contraint l'usage des outils pour une requête.
type ToolChoice string

const (
	ToolChoiceAuto     ToolChoice = ""
	ToolChoiceNone     ToolChoice = "none"
	ToolChoiceRequired ToolChoice = "required"
)

// Request est une requête de complétion.
type Request struct {
	// Model remplace le modèle par défaut du fournisseur s'il est renseigné.
	Model       string
	Messages    []Message
	Tools       []ToolSpec
	ToolChoice  ToolChoice
	Temperature *float64
	MaxTokens   int
}

// Usage compte les jetons d'une requête.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// CachedTokens : jetons d'entrée servis par le cache de prompt du fournisseur.
	CachedTokens int `json:"cached_tokens,omitempty"`
}

// Add additionne deux usages.
func (u Usage) Add(o Usage) Usage {
	return Usage{
		InputTokens:  u.InputTokens + o.InputTokens,
		OutputTokens: u.OutputTokens + o.OutputTokens,
		CachedTokens: u.CachedTokens + o.CachedTokens,
	}
}

// Response est la réponse d'un modèle : un message assistant.
type Response struct {
	Message      Message
	Usage        Usage
	FinishReason string
	Model        string
}

// Provider est un modèle de langage.
type Provider interface {
	Chat(ctx context.Context, req *Request) (*Response, error)
}

// ProviderFunc adapte une fonction en Provider.
type ProviderFunc func(ctx context.Context, req *Request) (*Response, error)

func (f ProviderFunc) Chat(ctx context.Context, req *Request) (*Response, error) { return f(ctx, req) }

// StreamEvent est un fragment d'une réponse en flux. Le dernier événement porte
// Response (réponse complète, appels d'outils assemblés) ou Err.
type StreamEvent struct {
	Delta          string
	ReasoningDelta string
	Response       *Response
	Err            error
}

// Streamer est implémenté par les fournisseurs capables de répondre en flux.
type Streamer interface {
	Stream(ctx context.Context, req *Request) (<-chan StreamEvent, error)
}

// ModelLister est implémenté par les fournisseurs capables de lister leurs modèles.
type ModelLister interface {
	ListModels(ctx context.Context) ([]string, error)
}
