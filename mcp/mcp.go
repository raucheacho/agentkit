// Package mcp branche les outils d'un serveur MCP (Model Context Protocol)
// sur un agent, sans SDK : transport stdio (sous-processus) ou HTTP
// (Streamable HTTP), JSON-RPC 2.0.
//
// Invariants :
//   - les résultats d'un serveur MCP sont des données non fiables ;
//   - l'effet d'un outil MCP ne repose pas sur la parole du serveur : par
//     défaut Write, Sensitive s'il se déclare destructif, Read seulement si
//     l'application fait confiance à ses annotations (TrustAnnotations) ou le
//     fixe elle-même (Effects) ;
//   - la liste des outils est relevée à la connexion et ne change plus
//     pendant la vie du client (prompt stable).
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/raucheacho/agentkit/tool"
)

// ProtocolVersion est la version du protocole demandée au serveur.
const ProtocolVersion = "2025-06-18"

// Config décrit un serveur MCP : Command (stdio) ou URL (HTTP).
type Config struct {
	// Name identifie le serveur ; il préfixe ses outils (« github_create_issue »).
	Name string
	// Stdio : commande, arguments, variables d'environnement ajoutées
	// (« CLE=valeur ») et dossier de travail.
	Command string
	Args    []string
	Env     []string
	Dir     string
	// HTTP : adresse du point d'entrée et en-têtes (authentification).
	URL        string
	Headers    map[string]string
	HTTPClient *http.Client

	// Prefix remplace le préfixe des outils (défaut : Name + "_" ; "-" : aucun).
	Prefix string
	// Include, s'il n'est pas vide, ne garde que ces outils (noms du serveur).
	Include []string
	// Exclude écarte des outils (noms du serveur).
	Exclude []string
	// Effects fixe l'effet de certains outils (noms du serveur).
	Effects map[string]tool.Effect
	// TrustAnnotations : un outil annoncé en lecture seule (readOnlyHint)
	// devient Read. Sinon, il reste Write.
	TrustAnnotations bool
	// CallTimeout borne un appel d'outil (défaut 2 min).
	CallTimeout time.Duration
	// MaxResultChars borne le texte d'un résultat (défaut 20 000).
	MaxResultChars int
	Logger         *slog.Logger
}

// ToolInfo est un outil annoncé par le serveur.
type ToolInfo struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations struct {
		ReadOnlyHint    *bool `json:"readOnlyHint,omitempty"`
		DestructiveHint *bool `json:"destructiveHint,omitempty"`
	} `json:"annotations"`
}

// Client est une connexion à un serveur MCP.
type Client struct {
	cfg    Config
	t      transport
	log    *slog.Logger
	server struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	instructions string
	tools        []ToolInfo
}

// Connect lance ou joint le serveur, négocie le protocole et relève ses outils.
func Connect(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Name == "" {
		return nil, errors.New("mcp : Config.Name est obligatoire")
	}
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = 2 * time.Minute
	}
	if cfg.MaxResultChars <= 0 {
		cfg.MaxResultChars = 20000
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With("mcp", cfg.Name)
	c := &Client{cfg: cfg, log: log}
	switch {
	case cfg.Command != "" && cfg.URL != "":
		return nil, errors.New("mcp : Command et URL sont exclusifs")
	case cfg.Command != "":
		t, err := startStdio(ctx, cfg.Command, cfg.Args, cfg.Env, cfg.Dir, log)
		if err != nil {
			return nil, err
		}
		c.t = t
	case cfg.URL != "":
		hc := cfg.HTTPClient
		if hc == nil {
			hc = &http.Client{Timeout: 5 * time.Minute}
		}
		c.t = &httpTransport{url: cfg.URL, headers: cfg.Headers, client: hc}
	default:
		return nil, errors.New("mcp : Command ou URL est obligatoire")
	}
	if err := c.initialize(ctx); err != nil {
		_ = c.t.close()
		return nil, err
	}
	return c, nil
}

func (c *Client) initialize(ctx context.Context) error {
	raw, err := c.t.call(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "agentkit", "version": "0.2"},
	})
	if err != nil {
		return fmt.Errorf("mcp %s : initialisation : %w", c.cfg.Name, err)
	}
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("mcp %s : réponse d'initialisation illisible : %w", c.cfg.Name, err)
	}
	c.server.Name, c.server.Version, c.instructions = res.ServerInfo.Name, res.ServerInfo.Version, res.Instructions
	if ht, ok := c.t.(*httpTransport); ok {
		ht.mu.Lock()
		ht.protocol = res.ProtocolVersion
		ht.mu.Unlock()
	}
	if err := c.t.notify(ctx, "notifications/initialized", nil); err != nil {
		return fmt.Errorf("mcp %s : %w", c.cfg.Name, err)
	}
	// Outils, page par page.
	cursor := ""
	for page := 0; page < 50; page++ {
		var params any
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}
		raw, err := c.t.call(ctx, "tools/list", params)
		if err != nil {
			return fmt.Errorf("mcp %s : liste des outils : %w", c.cfg.Name, err)
		}
		var list struct {
			Tools      []ToolInfo `json:"tools"`
			NextCursor string     `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			return fmt.Errorf("mcp %s : liste des outils illisible : %w", c.cfg.Name, err)
		}
		c.tools = append(c.tools, list.Tools...)
		if cursor = list.NextCursor; cursor == "" {
			break
		}
	}
	c.log.Info("mcp : connecté", "server", c.server.Name, "version", c.server.Version, "tools", len(c.tools))
	return nil
}

// Instructions renvoie les consignes d'usage annoncées par le serveur (à
// placer dans le prompt système si on leur fait confiance).
func (c *Client) Instructions() string { return c.instructions }

// Infos renvoie les outils annoncés par le serveur.
func (c *Client) Infos() []ToolInfo { return append([]ToolInfo(nil), c.tools...) }

// Alive indique si la connexion est ouverte.
func (c *Client) Alive() bool { return c.t.alive() }

// Close ferme la connexion (et arrête le sous-processus).
func (c *Client) Close() error { return c.t.close() }

// Call appelle un outil du serveur par son nom d'origine.
func (c *Client) Call(ctx context.Context, name string, args json.RawMessage) (tool.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.CallTimeout)
	defer cancel()
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	raw, err := c.t.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return tool.Result{}, err
	}
	var res struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			MimeType string `json:"mimeType"`
			URI      string `json:"uri"`
			Resource *struct {
				URI  string `json:"uri"`
				Text string `json:"text"`
			} `json:"resource"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return tool.Result{}, fmt.Errorf("mcp : résultat illisible : %w", err)
	}
	var parts []string
	for _, p := range res.Content {
		switch p.Type {
		case "text":
			parts = append(parts, p.Text)
		case "resource":
			if p.Resource != nil && p.Resource.Text != "" {
				parts = append(parts, p.Resource.Text)
			} else if p.Resource != nil {
				parts = append(parts, "[ressource "+p.Resource.URI+"]")
			}
		case "resource_link":
			parts = append(parts, "[lien vers la ressource "+p.URI+"]")
		default:
			parts = append(parts, "["+p.Type+" "+p.MimeType+" omis]")
		}
	}
	if len(parts) == 0 && len(res.StructuredContent) > 0 {
		parts = append(parts, string(res.StructuredContent))
	}
	text := strings.Join(parts, "\n")
	if r := []rune(text); len(r) > c.cfg.MaxResultChars {
		text = string(r[:c.cfg.MaxResultChars]) + "\n[… résultat tronqué …]"
	}
	if res.IsError {
		text = "Erreur renvoyée par l'outil : " + text
	}
	return tool.External(text), nil
}

var invalidChars = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// Toolset renvoie les outils du serveur, préfixés, en un groupe disponible
// tant que la connexion est ouverte.
func (c *Client) Toolset() tool.Toolset {
	prefix := c.cfg.Prefix
	switch prefix {
	case "":
		prefix = c.cfg.Name + "_"
	case "-":
		prefix = ""
	}
	keep := func(name string) bool {
		if len(c.cfg.Include) > 0 && !contains(c.cfg.Include, name) {
			return false
		}
		return !contains(c.cfg.Exclude, name)
	}
	var tools []tool.Tool
	for _, info := range c.tools {
		if !keep(info.Name) {
			continue
		}
		name := invalidChars.ReplaceAllString(prefix+info.Name, "_")
		if len(name) > 64 {
			name = name[:64]
		}
		desc := info.Description
		if desc == "" {
			desc = info.Title
		}
		schema := info.InputSchema
		if len(schema) == 0 || string(schema) == "null" {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		remote := info.Name
		tools = append(tools, tool.Func(name, desc, schema, func(ctx context.Context, call tool.Call) (tool.Result, error) {
			return c.Call(ctx, remote, call.Raw)
		}, tool.WithEffect(c.effect(info))))
	}
	return tool.Toolset{Name: "mcp:" + c.cfg.Name, Tools: tools, Available: func(context.Context) bool { return c.Alive() }}
}

func (c *Client) effect(info ToolInfo) tool.Effect {
	if e, ok := c.cfg.Effects[info.Name]; ok {
		return e
	}
	a := info.Annotations
	switch {
	case a.DestructiveHint != nil && *a.DestructiveHint && (a.ReadOnlyHint == nil || !*a.ReadOnlyHint):
		return tool.Sensitive
	case c.cfg.TrustAnnotations && a.ReadOnlyHint != nil && *a.ReadOnlyHint:
		return tool.Read
	}
	return tool.Write
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
