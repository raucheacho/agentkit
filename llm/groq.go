package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// GroqProvider implements the Provider interface against Groq's OpenAI-compatible API.
type GroqProvider struct {
	config Config
	client *http.Client
	mu     sync.RWMutex // Protects config.Model for thread-safe updates
}

// NewGroqProvider creates a new Groq provider.
func NewGroqProvider(config Config) *GroqProvider {
	if config.BaseURL == "" {
		config.BaseURL = "https://api.groq.com/openai/v1"
	}
	if config.Model == "" {
		config.Model = "llama-3.3-70b-versatile"
	}

	return &GroqProvider{
		config: config,
		client: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// groqChatRequest represents the request body for Groq's chat completions endpoint.
type groqChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	Stream      bool      `json:"stream"`
	Temperature float32   `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
}

// groqChoice represents a single completion choice.
type groqChoice struct {
	Message      groqMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// groqMessage represents a message in Groq's response.
type groqMessage struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// groqChatResponse represents the response from Groq.
type groqChatResponse struct {
	Choices []groqChoice `json:"choices"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// groqStreamChunk represents a single SSE chunk from Groq's streaming endpoint.
type groqStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

// Chat sends a chat completion request.
func (p *GroqProvider) Chat(ctx context.Context, messages []Message, tools []Tool) (*Response, error) {
	p.mu.RLock()
	model := p.config.Model
	temperature := p.config.Temperature
	maxTokens := p.config.MaxTokens
	baseURL := p.config.BaseURL
	apiKey := p.config.APIKey
	p.mu.RUnlock()

	reqBody := groqChatRequest{
		Model:       model,
		Messages:    messages,
		Tools:       tools,
		Stream:      false,
		Temperature: temperature,
		MaxTokens:   maxTokens,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal error: %w", err)
	}

	url := fmt.Sprintf("%s/chat/completions", baseURL)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("request error: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http error: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read error: %w", err)
	}

	var chatResp groqChatResponse
	decodeErr := json.Unmarshal(body, &chatResp)

	// Le code HTTP d'abord : une passerelle en panne renvoie souvent du HTML,
	// et l'erreur doit garder son statut (429, 502…) pour être réessayée.
	if resp.StatusCode != http.StatusOK {
		if decodeErr == nil && chatResp.Error != nil {
			return nil, fmt.Errorf("groq error (status %d): %s", resp.StatusCode, chatResp.Error.Message)
		}
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, truncateBody(body))
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("decode error: %w", decodeErr)
	}

	if len(chatResp.Choices) == 0 {
		return &Response{Done: true}, nil
	}

	choice := chatResp.Choices[0]
	return &Response{
		Content:   choice.Message.Content,
		ToolCalls: choice.Message.ToolCalls,
		Done:      true,
	}, nil
}

// ChatStream sends a chat completion request with a streaming response.
func (p *GroqProvider) ChatStream(ctx context.Context, messages []Message, tools []Tool) (<-chan StreamingResponse, error) {
	p.mu.RLock()
	model := p.config.Model
	temperature := p.config.Temperature
	maxTokens := p.config.MaxTokens
	baseURL := p.config.BaseURL
	apiKey := p.config.APIKey
	p.mu.RUnlock()

	reqBody := groqChatRequest{
		Model:       model,
		Messages:    messages,
		Tools:       tools,
		Stream:      true,
		Temperature: temperature,
		MaxTokens:   maxTokens,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal error: %w", err)
	}

	url := fmt.Sprintf("%s/chat/completions", baseURL)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("request error: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http error: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}

	ch := make(chan StreamingResponse, 10)

	go func() {
		defer resp.Body.Close()
		defer close(ch)

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				select {
				case ch <- StreamingResponse{Done: true}:
				case <-ctx.Done():
				}
				return
			}

			var chunk groqStreamChunk
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				continue
			}
			if len(chunk.Choices) == 0 {
				continue
			}

			done := chunk.Choices[0].FinishReason != nil
			select {
			case ch <- StreamingResponse{Content: chunk.Choices[0].Delta.Content, Done: done}:
			case <-ctx.Done():
				return
			}
		}
	}()

	return ch, nil
}

// GetDefaultModel returns the default model.
func (p *GroqProvider) GetDefaultModel() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.config.Model
}

// SetModel updates the model used by the provider (thread-safe).
func (p *GroqProvider) SetModel(model string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if model != "" {
		p.config.Model = model
	}
}

// ListModels lists available models from Groq.
func (p *GroqProvider) ListModels(ctx context.Context) ([]string, error) {
	p.mu.RLock()
	baseURL := p.config.BaseURL
	apiKey := p.config.APIKey
	p.mu.RUnlock()

	url := fmt.Sprintf("%s/models", baseURL)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	models := make([]string, 0, len(result.Data))
	for _, m := range result.Data {
		models = append(models, m.ID)
	}

	return models, nil
}

// truncateBody borne un corps d'erreur dans les messages et journaux.
func truncateBody(b []byte) string {
	const max = 500
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}
