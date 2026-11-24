// Package openai implémente llm.Provider pour toute API compatible avec
// /chat/completions d'OpenAI : OpenAI, Groq, DeepSeek, OpenRouter, Mistral,
// Ollama (/v1), vLLM, LM Studio…
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/raucheacho/agentkit/llm"
)

// Config d'un fournisseur.
type Config struct {
	// Name apparaît dans les erreurs (« groq », « openrouter »…). Défaut : "openai".
	Name    string
	BaseURL string // ex. https://api.groq.com/openai/v1
	APIKey  string
	Model   string // modèle par défaut, remplacé par llm.Request.Model
	// Headers ajoutés à chaque requête (ex. HTTP-Referer pour OpenRouter).
	Headers    map[string]string
	HTTPClient *http.Client
}

// Provider est un client /chat/completions.
type Provider struct {
	cfg  Config
	http *http.Client
}

// New crée un fournisseur.
func New(cfg Config) *Provider {
	if cfg.Name == "" {
		cfg.Name = "openai"
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.openai.com/v1"
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	hc := cfg.HTTPClient
	if hc == nil {
		hc = defaultHTTPClient()
	}
	return &Provider{cfg: cfg, http: hc}
}

// defaultHTTPClient borne chaque étape d'un appel : une connexion perdue en
// route (réseau instable, connexion HTTP/2 morte) devient vite une erreur
// passagère, que llm.Retry rejoue, au lieu de bloquer le tour jusqu'au délai
// global.
func defaultHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	tr.TLSHandshakeTimeout = 10 * time.Second
	// Sans flux, la réponse entière précède les en-têtes : laisser au modèle
	// le temps de rédiger.
	tr.ResponseHeaderTimeout = 120 * time.Second
	// Connexion silencieuse 15 s : ping ; sans réponse en 10 s, elle est
	// fermée et les requêtes en cours échouent.
	tr.HTTP2 = &http.HTTP2Config{SendPingTimeout: 15 * time.Second, PingTimeout: 10 * time.Second}
	return &http.Client{Transport: tr, Timeout: 180 * time.Second}
}

// Groq renvoie la configuration de Groq.
func Groq(apiKey, model string) Config {
	return Config{Name: "groq", BaseURL: "https://api.groq.com/openai/v1", APIKey: apiKey, Model: model}
}

// OpenRouter renvoie la configuration d'OpenRouter.
func OpenRouter(apiKey, model string) Config {
	return Config{Name: "openrouter", BaseURL: "https://openrouter.ai/api/v1", APIKey: apiKey, Model: model}
}

// DeepSeek renvoie la configuration de DeepSeek.
func DeepSeek(apiKey, model string) Config {
	return Config{Name: "deepseek", BaseURL: "https://api.deepseek.com/v1", APIKey: apiKey, Model: model}
}

// Gemini renvoie la configuration de l'API Gemini de Google (Google AI
// Studio), par son point d'accès compatible OpenAI.
func Gemini(apiKey, model string) Config {
	return Config{Name: "gemini", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", APIKey: apiKey, Model: model}
}

// Ollama renvoie la configuration d'un serveur Ollama local (API /v1).
func Ollama(baseURL, model string) Config {
	if baseURL == "" {
		baseURL = "http://localhost:11434/v1"
	}
	return Config{Name: "ollama", BaseURL: baseURL, Model: model}
}

// --- format de l'API ---

type wireFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireToolCall struct {
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function wireFunction `json:"function"`
	// ExtraContent (Gemini) doit revenir tel quel avec l'appel.
	ExtraContent json.RawMessage `json:"extra_content,omitempty"`
}

type wireMessage struct {
	Role             string         `json:"role"`
	Content          *string        `json:"content"`
	ToolCalls        []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	Name             string         `json:"name,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	Reasoning        string         `json:"reasoning,omitempty"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type wireRequest struct {
	Model         string         `json:"model"`
	Messages      []wireMessage  `json:"messages"`
	Tools         []wireTool     `json:"tools,omitempty"`
	ToolChoice    string         `json:"tool_choice,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
}

type wireResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      wireMessage `json:"message"`
		Delta        wireMessage `json:"delta"`
		FinishReason *string     `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
	Error *wireError `json:"error"`
}

type wireError struct {
	Message string `json:"message"`
	// Details (Google) : RetryInfo donne le délai avant une nouvelle tentative.
	Details []struct {
		Type       string `json:"@type"`
		RetryDelay string `json:"retryDelay"`
	} `json:"details"`
}

func (u *wireUsage) usage() llm.Usage {
	if u == nil {
		return llm.Usage{}
	}
	out := llm.Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens}
	if u.PromptTokensDetails != nil {
		out.CachedTokens = u.PromptTokensDetails.CachedTokens
	}
	return out
}

func toWire(req *llm.Request, model string, stream bool) wireRequest {
	w := wireRequest{Model: model, Temperature: req.Temperature, MaxTokens: req.MaxTokens, Stream: stream}
	if stream {
		w.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	for _, m := range req.Messages {
		wm := wireMessage{Role: string(m.Role), ToolCallID: m.ToolCallID, Name: m.Name}
		content := m.Content
		// Un message assistant qui n'appelle que des outils peut avoir un contenu
		// nul ; partout ailleurs le contenu est une chaîne, même vide.
		if !(m.Role == llm.RoleAssistant && content == "" && len(m.ToolCalls) > 0) {
			wm.Content = &content
		}
		for _, tc := range m.ToolCalls {
			args := tc.Arguments
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			wm.ToolCalls = append(wm.ToolCalls, wireToolCall{ID: tc.ID, Type: "function", Function: wireFunction{Name: tc.Name, Arguments: args}, ExtraContent: tc.Extra})
		}
		w.Messages = append(w.Messages, wm)
	}
	for _, t := range req.Tools {
		var wt wireTool
		wt.Type = "function"
		wt.Function.Name = t.Name
		wt.Function.Description = t.Description
		wt.Function.Parameters = t.Parameters
		if len(wt.Function.Parameters) == 0 {
			wt.Function.Parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		w.Tools = append(w.Tools, wt)
	}
	if len(req.Tools) > 0 && req.ToolChoice != llm.ToolChoiceAuto {
		w.ToolChoice = string(req.ToolChoice)
	}
	return w
}

func fromWire(m wireMessage) llm.Message {
	out := llm.Message{Role: llm.RoleAssistant, Reasoning: m.ReasoningContent}
	if out.Reasoning == "" {
		out.Reasoning = m.Reasoning
	}
	if m.Content != nil {
		out.Content = *m.Content
	}
	for _, tc := range m.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, llm.ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments, Extra: tc.ExtraContent})
	}
	return out
}

func (p *Provider) model(req *llm.Request) string {
	if req.Model != "" {
		return req.Model
	}
	return p.cfg.Model
}

func (p *Provider) post(ctx context.Context, body wireRequest) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%s: encodage de la requête: %w", p.cfg.Name, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BaseURL+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	p.headers(httpReq)
	httpReq.Header.Set("Content-Type", "application/json")
	if body.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	resp, err := p.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.cfg.Name, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, p.statusError(resp, raw)
	}
	return resp, nil
}

func (p *Provider) headers(r *http.Request) {
	if p.cfg.APIKey != "" {
		r.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}
	for k, v := range p.cfg.Headers {
		r.Header.Set(k, v)
	}
}

func (p *Provider) statusError(resp *http.Response, raw []byte) error {
	msg := strings.TrimSpace(string(raw))
	var wr wireResponse
	// Google enveloppe l'erreur dans un tableau.
	var list []wireResponse
	if json.Unmarshal(raw, &list) == nil && len(list) > 0 {
		wr = list[0]
	} else {
		_ = json.Unmarshal(raw, &wr)
	}
	var after time.Duration
	if wr.Error != nil {
		if wr.Error.Message != "" {
			msg = wr.Error.Message
		}
		for _, d := range wr.Error.Details {
			if v, err := time.ParseDuration(d.RetryDelay); err == nil && d.RetryDelay != "" {
				after = v
			}
		}
	}
	if s := resp.Header.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			after = time.Duration(n) * time.Second
		} else if t, err := http.ParseTime(s); err == nil {
			after = time.Until(t)
		}
	}
	if len(msg) > 500 {
		msg = msg[:500] + "…"
	}
	return &llm.StatusError{Provider: p.cfg.Name, Status: resp.StatusCode, Message: msg, RetryAfter: max(after, 0)}
}

// Chat envoie une requête et attend la réponse complète.
func (p *Provider) Chat(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	resp, err := p.post(ctx, toWire(req, p.model(req), false))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var wr wireResponse
	if err := json.NewDecoder(resp.Body).Decode(&wr); err != nil {
		return nil, fmt.Errorf("%s: décodage de la réponse: %w", p.cfg.Name, err)
	}
	if wr.Error != nil {
		return nil, &llm.StatusError{Provider: p.cfg.Name, Status: http.StatusOK, Message: wr.Error.Message}
	}
	out := &llm.Response{Model: wr.Model, Usage: wr.Usage.usage(), Message: llm.Message{Role: llm.RoleAssistant}}
	if len(wr.Choices) > 0 {
		out.Message = fromWire(wr.Choices[0].Message)
		if fr := wr.Choices[0].FinishReason; fr != nil {
			out.FinishReason = *fr
		}
	}
	return out, nil
}

// Stream envoie une requête et renvoie la réponse en fragments. Les appels
// d'outils sont assemblés et livrés dans l'événement final.
func (p *Provider) Stream(ctx context.Context, req *llm.Request) (<-chan llm.StreamEvent, error) {
	resp, err := p.post(ctx, toWire(req, p.model(req), true))
	if err != nil {
		return nil, err
	}
	ch := make(chan llm.StreamEvent, 16)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		emit := func(ev llm.StreamEvent) bool {
			select {
			case ch <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		}

		final := &llm.Response{Message: llm.Message{Role: llm.RoleAssistant}}
		var content, reasoning strings.Builder
		calls := map[int]*llm.ToolCall{}

		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				break
			}
			var chunk wireResponse
			if json.Unmarshal([]byte(payload), &chunk) != nil {
				continue
			}
			if chunk.Error != nil {
				emit(llm.StreamEvent{Err: &llm.StatusError{Provider: p.cfg.Name, Status: http.StatusOK, Message: chunk.Error.Message}})
				return
			}
			if chunk.Model != "" {
				final.Model = chunk.Model
			}
			if chunk.Usage != nil {
				final.Usage = chunk.Usage.usage()
			}
			if len(chunk.Choices) == 0 {
				continue
			}
			c := chunk.Choices[0]
			if c.FinishReason != nil {
				final.FinishReason = *c.FinishReason
			}
			d := c.Delta
			if d.Content != nil && *d.Content != "" {
				content.WriteString(*d.Content)
				if !emit(llm.StreamEvent{Delta: *d.Content}) {
					return
				}
			}
			if r := d.ReasoningContent + d.Reasoning; r != "" {
				reasoning.WriteString(r)
				if !emit(llm.StreamEvent{ReasoningDelta: r}) {
					return
				}
			}
			for i, tc := range d.ToolCalls {
				idx := i
				if tc.Index != nil {
					idx = *tc.Index
				} else if prev, ok := calls[idx]; ok && tc.ID != "" && prev.ID != "" && prev.ID != tc.ID {
					// Sans index (Gemini), chaque appel arrive entier avec son
					// propre identifiant : un nouvel identifiant est un nouvel appel.
					idx = len(calls)
					for calls[idx] != nil {
						idx++
					}
				}
				cur, ok := calls[idx]
				if !ok {
					cur = &llm.ToolCall{}
					calls[idx] = cur
				}
				if tc.ID != "" {
					cur.ID = tc.ID
				}
				cur.Name += tc.Function.Name
				cur.Arguments += tc.Function.Arguments
				if len(tc.ExtraContent) > 0 {
					cur.Extra = tc.ExtraContent
				}
			}
		}
		if err := sc.Err(); err != nil {
			emit(llm.StreamEvent{Err: fmt.Errorf("%s: lecture du flux: %w", p.cfg.Name, err)})
			return
		}
		final.Message.Content = content.String()
		final.Message.Reasoning = reasoning.String()
		idx := make([]int, 0, len(calls))
		for i := range calls {
			idx = append(idx, i)
		}
		sort.Ints(idx)
		for _, i := range idx {
			final.Message.ToolCalls = append(final.Message.ToolCalls, *calls[i])
		}
		emit(llm.StreamEvent{Response: final})
	}()
	return ch, nil
}

// ListModels liste les modèles exposés par /models.
func (p *Provider) ListModels(ctx context.Context) ([]string, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.BaseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	p.headers(httpReq)
	resp, err := p.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.cfg.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, p.statusError(resp, raw)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("%s: décodage des modèles: %w", p.cfg.Name, err)
	}
	models := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		models = append(models, m.ID)
	}
	return models, nil
}

var (
	_ llm.Provider    = (*Provider)(nil)
	_ llm.Streamer    = (*Provider)(nil)
	_ llm.ModelLister = (*Provider)(nil)
)
