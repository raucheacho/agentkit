# agentkit

Runtime Go réutilisable pour construire des **agents conversationnels à tools** (WhatsApp via [wotp](https://github.com/raucheacho/wotp), ou tout autre canal). Extrait du projet Livro, où il propulse l'assistant WhatsApp.

La lib est **agnostique du métier** : elle fournit la boucle d'agent, le bus de messages, les canaux, les sessions et le serveur HTTP — votre application fournit les tools, la persona et les instructions.

## Packages

| Package | Rôle |
|---|---|
| `agent` | Boucle ReAct (LLM → tool calls → réponse), verrouillage par conversation, subagents (`spawn`) |
| `tools` | Registre/manager d'outils (défs JSON Schema, exécution, `ConversationContext`) |
| `bus` | Bus de messages entrants/sortants entre canaux et agent |
| `channels` | Interface `Channel` + transport WhatsApp via wotp (webhook entrant, REST sortant) |
| `session` | Historique de conversation persisté en SQLite, safe en concurrence |
| `store` | Ouverture de la base SQLite partagée (WAL) |
| `llm` | Interface `Provider` + implémentation Groq (API compatible OpenAI) |
| `server` | Serveur HTTP : API admin (statut/modèle) + montage de webhooks |
| `httpauth` | Middleware JWT (HS256, claim `roles`) pour l'API admin |
| `bootstrap` | `AGENTS.md` éditable à chaud, injecté dans le prompt système |

## Usage

```go
import (
    "github.com/raucheacho/agentkit/agent"
    "github.com/raucheacho/agentkit/bus"
    "github.com/raucheacho/agentkit/channels"
    "github.com/raucheacho/agentkit/llm"
    "github.com/raucheacho/agentkit/session"
    "github.com/raucheacho/agentkit/store"
    "github.com/raucheacho/agentkit/tools"
)

// 1. Infra générique
db, _ := store.Open("agent.db")
sessions, _ := session.NewStore(db)
provider := llm.NewGroqProvider(llm.Config{APIKey: apiKey, Model: model})
messageBus := bus.New()

// 2. Vos tools métier (c'est ici que vit votre application)
toolMgr := tools.NewToolManager(workspaceDir)
toolMgr.RegisterTool(tools.ToolDef{ /* nom, description, schéma */ }, monExecuteur)

// 3. L'agent : persona et instructions injectées, jamais codées dans la lib
ag := agent.New(agent.Config{
    Provider:     provider,
    SessionStore: sessions,
    Tools:        toolMgr,
    Identity:     "# Mon Assistant\n\nTu es …",
    Instructions: "## Instructions\n\n…",
    Workspace:    workspaceDir,
    Bus:          messageBus,
})

// 4. Un canal (ex: WhatsApp via wotp) + la boucle de dispatch
wa := channels.NewWotp(channels.WotpConfig{ChatURL: url, ChatAPIKey: key}, messageBus)
// … voir livro-plateform/services/agent/cmd/agent/main.go pour un assemblage complet
```

L'identité de la conversation (canal + chat ID — le numéro de téléphone pour WhatsApp) circule via `tools.ConversationContext` dans le `context.Context` de chaque appel de tool : un tool ne peut jamais agir sur une autre identité que celle qui écrit.

## Notes

- `mattn/go-sqlite3` impose **cgo** : compilez avec `CGO_ENABLED=1`.
- Le format des tools (nom, description, JSON Schema, résultat) est volontairement aligné sur les concepts MCP / function-calling OpenAI-compatible, pour laisser la porte ouverte à un adaptateur MCP sans casser l'API.

## Tests

```bash
make test
```
