// Commande assistant : un assistant personnel, à la manière de Nod ou
// d'Hermes. Un seul propriétaire, une mémoire partagée entre toutes ses
// conversations, des skills qu'il écrit lui-même, des tâches de fond, des
// rappels qu'il planifie, des flux RSS qu'il surveille, son propre
// ordinateur et des serveurs MCP.
//
//	AGENTKIT_MODEL=llama3.1:8b go run ./examples/assistant
//	TELEGRAM_TOKEN=… TELEGRAM_OWNER=<votre id>    # facultatif : Telegram
//	AGENTKIT_EMBED_MODEL=nomic-embed-text           # facultatif : recherche hybride
//	AGENTKIT_COMPUTER=docker|local                  # facultatif : son ordinateur
//	AGENTKIT_FEEDS="https://…/rss https://…/atom"   # facultatif : veille
//	AGENTKIT_MCP="nom commande arg…"                # facultatif : serveur MCP
//
// Web : POST /web/messages {"chat_id":"moi","text":"…"}, GET /web/events?chat_id=moi.
// Administration : /admin/… avec ADMIN_TOKEN (voir kit.Admin).
package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/raucheacho/agentkit/agent"
	"github.com/raucheacho/agentkit/channel"
	"github.com/raucheacho/agentkit/channel/telegram"
	"github.com/raucheacho/agentkit/channel/web"
	"github.com/raucheacho/agentkit/computer"
	"github.com/raucheacho/agentkit/computer/docker"
	"github.com/raucheacho/agentkit/computer/local"
	"github.com/raucheacho/agentkit/examples/internal/setup"
	"github.com/raucheacho/agentkit/kit"
	"github.com/raucheacho/agentkit/llm"
	"github.com/raucheacho/agentkit/llm/openai"
	"github.com/raucheacho/agentkit/mcp"
	"github.com/raucheacho/agentkit/observe"
	"github.com/raucheacho/agentkit/tool"
)

func main() { setup.Main(run) }

func run(ctx context.Context) error {
	dir := setup.Env("AGENTKIT_DIR", filepath.Join(os.TempDir(), "agentkit-assistant"))
	db, err := setup.Open(dir)
	if err != nil {
		return err
	}
	defer db.Close()
	provider, llmCfg := setup.Model()

	// La conversation où l'assistant prévient son propriétaire.
	home := tool.Conversation{Session: "web:moi", Channel: "web", ChatID: "moi"}
	webCh := web.New(web.Config{})
	channels := []channel.Adapter{webCh}
	if tok := os.Getenv("TELEGRAM_TOKEN"); tok != "" {
		channels = append(channels, telegram.New(telegram.Config{Token: tok, AllowFrom: []string{os.Getenv("TELEGRAM_OWNER")}}))
		home = tool.Conversation{Session: "telegram:" + os.Getenv("TELEGRAM_OWNER"), Channel: "telegram", ChatID: os.Getenv("TELEGRAM_OWNER")}
	}

	var embedder llm.Embedder
	if m := os.Getenv("AGENTKIT_EMBED_MODEL"); m != "" {
		embedder = openai.NewEmbedder(llmCfg, m)
	}

	var ws computer.Workspace
	switch os.Getenv("AGENTKIT_COMPUTER") {
	case "docker":
		d := docker.New(docker.Config{Name: "agentkit-assistant"})
		if err := d.Ensure(ctx); err != nil {
			return err
		}
		ws = d
	case "local":
		if ws, err = local.New(local.Config{Root: filepath.Join(dir, "workspace")}); err != nil {
			return err
		}
	}

	var servers []mcp.Config
	if spec := strings.Fields(os.Getenv("AGENTKIT_MCP")); len(spec) >= 2 {
		servers = append(servers, mcp.Config{Name: spec[0], Command: spec[1], Args: spec[2:]})
	}

	var watch *observe.Config
	if feeds := strings.Fields(os.Getenv("AGENTKIT_FEEDS")); len(feeds) > 0 {
		watch = &observe.Config{
			Pollers:  []observe.Poller{rssPoller(feeds)},
			Interval: 15 * time.Minute,
			Route:    func(observe.Event) (tool.Conversation, bool) { return home, true },
			Quiet:    &observe.Quiet{Start: "22:00", End: "07:30"},
		}
	}

	k, err := kit.New(ctx, kit.Config{
		Provider: provider, DB: db,
		Instructions: "Tu es l'assistant personnel de ton propriétaire. Tu es direct, concis et proactif : " +
			"tu retiens ce qui compte (memory_save), tu planifies les rappels qu'il demande, tu délègues les " +
			"recherches longues et tu écris un skill quand tu trouves une bonne méthode.",
		ContextDir: dir,
		Channels:   channels,
		Trust:      kit.Trust{TTL: 24 * time.Hour},
		// Un seul propriétaire : une mémoire et un historique communs à toutes ses conversations.
		Memory:     &kit.Memory{Embedder: embedder, Scope: func(tool.Conversation) string { return "owner" }, SharedHistory: true},
		Skills:     &kit.Skills{Writable: filepath.Join(dir, "skills")},
		Compaction: &agent.Compaction{},
		Delegate:   &kit.Delegate{Background: true},
		Schedule:   &kit.Schedule{AgentTools: true},
		Observe:    watch,
		Computer:   ws,
		MCP:        servers,
		TraceFile:  filepath.Join(dir, "trace.jsonl"),
		Agent:      func(c *agent.Config) { c.ParseInlineTools = true },
	})
	if err != nil {
		return err
	}
	defer k.Close()

	mux := http.NewServeMux()
	mux.Handle("/web/", http.StripPrefix("/web", webCh.Handler()))
	mux.Handle("/admin/", http.StripPrefix("/admin", k.Admin(setup.AdminToken())))
	return setup.Serve(ctx, setup.Env("AGENTKIT_ADDR", ":8080"), mux, k.Run)
}
