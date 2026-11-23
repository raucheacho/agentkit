// Commande cli : un agent minimal dans le terminal, sur n'importe quelle API
// compatible OpenAI, avec un historique SQLite et deux outils.
//
//	AGENTKIT_BASE_URL=https://api.groq.com/openai/v1 \
//	AGENTKIT_API_KEY=… AGENTKIT_MODEL=llama-3.3-70b-versatile \
//	go run ./examples/cli
//
// Commandes : /reset efface la conversation, /quit quitte.
package main

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/raucheacho/agentkit/agent"
	"github.com/raucheacho/agentkit/llm"
	"github.com/raucheacho/agentkit/llm/openai"
	"github.com/raucheacho/agentkit/prompt"
	"github.com/raucheacho/agentkit/session/sqlite"
	"github.com/raucheacho/agentkit/tool"
)

type noteIn struct {
	Text string `json:"text" desc:"le texte à retenir"`
}

func main() {
	ctx := context.Background()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))

	cfg := openai.Config{
		BaseURL: env("AGENTKIT_BASE_URL", "https://api.openai.com/v1"),
		APIKey:  os.Getenv("AGENTKIT_API_KEY"),
		Model:   env("AGENTKIT_MODEL", "gpt-4o-mini"),
	}
	provider := llm.Retry(openai.New(cfg), 3, time.Second)

	dir := env("AGENTKIT_DIR", filepath.Join(os.TempDir(), "agentkit-cli"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fail(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "agent.db"))
	if err != nil {
		fail(err)
	}
	defer db.Close()
	sessions, err := sqlite.New(ctx, db, "")
	if err != nil {
		fail(err)
	}

	var notes []string
	tools := tool.NewRegistry()
	err = tools.Add(
		tool.New("remember", "Retient une information donnée par l'utilisateur.", func(_ context.Context, in noteIn) (tool.Result, error) {
			notes = append(notes, in.Text)
			return tool.Text("Retenu."), nil
		}, tool.WithEffect(tool.Write)),
		tool.Func("list_notes", "Liste les informations retenues.", nil, func(context.Context, tool.Call) (tool.Result, error) {
			if len(notes) == 0 {
				return tool.Text("Aucune note."), nil
			}
			return tool.Text("- " + strings.Join(notes, "\n- ")), nil
		}),
	)
	if err != nil {
		fail(err)
	}

	ag := agent.New(agent.Config{
		Provider: provider,
		Tools:    tools,
		Sessions: sessions,
		System: prompt.New(
			prompt.Text("Tu es un assistant personnel concis. Tu retiens ce que l'utilisateur te demande de retenir."),
			prompt.Heading("Contexte", prompt.Files(dir, 0, "AGENTS.md")),
		).Build,
		ParseInlineTools: true,
	})

	conv := tool.Conversation{Session: "cli:local", Channel: "cli", ChatID: "local"}
	in := bufio.NewScanner(os.Stdin)
	fmt.Printf("agentkit cli — modèle %s (/reset, /quit)\n", cfg.Model)
	for {
		fmt.Print("> ")
		if !in.Scan() {
			return
		}
		line := strings.TrimSpace(in.Text())
		switch line {
		case "":
			continue
		case "/quit":
			return
		case "/reset":
			if err := ag.Reset(ctx, conv.Session); err != nil {
				fmt.Println("erreur :", err)
			}
			continue
		}
		out, err := ag.Run(ctx, agent.Input{Conversation: conv, Content: line, Stream: func(d string) { fmt.Print(d) }})
		if err != nil {
			fmt.Println("\nerreur :", err)
			continue
		}
		fmt.Printf("\n[%s · %d étapes · %d jetons en entrée dont %d en cache]\n",
			out.Status, out.Steps, out.Usage.InputTokens, out.Usage.CachedTokens)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
