// Package llm provides a unified interface for multiple LLM providers
package llm

import (
	"context"
)

// Message represents a chat message.
// ToolCalls is set on assistant messages that triggered tool calls; ToolCallID
// links a "tool" role message back to the call it answers. Both are required
// by OpenAI-compatible APIs (Groq) to keep a valid conversation history.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall represents a tool call from the LLM
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Tool represents a tool available to the LLM
type Tool struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

// FunctionDef defines a function
type FunctionDef struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Parameters  Parameters `json:"parameters"`
}

// Parameters defines function parameters. Required uses omitempty: a nil Go
// slice marshals to JSON `null`, which Groq's strict JSON Schema validator
// rejects outright (it wants either an array or the key absent entirely) —
// this bit us on the very first real tool call (get_today, which has no
// required params).
type Parameters struct {
	Type       string              `json:"type"`
	Properties map[string]Property `json:"properties"`
	Required   []string            `json:"required,omitempty"`
}

// Property defines a parameter property
type Property struct {
	Type        string   `json:"type"`
	Description string   `json:"description"`
	Enum        []string `json:"enum,omitempty"`
}

// Response represents a response from the LLM
type Response struct {
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	Done      bool       `json:"done"`
}

// StreamingResponse represents a chunk of a streaming response
type StreamingResponse struct {
	Content  string    `json:"content"`
	ToolCall *ToolCall `json:"tool_call,omitempty"`
	Done     bool      `json:"done"`
}

// Provider defines the interface for LLM providers
type Provider interface {
	// Chat sends a chat completion request
	Chat(ctx context.Context, messages []Message, tools []Tool) (*Response, error)

	// ChatStream sends a chat completion request with streaming response
	ChatStream(ctx context.Context, messages []Message, tools []Tool) (<-chan StreamingResponse, error)

	// GetDefaultModel returns the default model for this provider
	GetDefaultModel() string

	// SetModel updates the model used by the provider (thread-safe)
	SetModel(model string)

	// ListModels returns available models (if supported)
	ListModels(ctx context.Context) ([]string, error)
}

// Config contains common configuration for all providers
type Config struct {
	BaseURL     string
	APIKey      string
	Model       string
	Temperature float32
	MaxTokens   int
}

// DefaultConfig returns a default configuration
func DefaultConfig() Config {
	return Config{
		Temperature: 0.7,
		MaxTokens:   4096,
	}
}
