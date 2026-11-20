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
| `session` | Fournit `SQLiteStore`, une implémentation de `agent.SessionStore` persistée en SQLite |
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
sessions, _ := session.NewSQLiteStore(db)
provider := llm.NewGroqProvider(llm.Config{APIKey: apiKey, Model: model})
messageBus := bus.New()

// 2. Vos tools métier (c'est ici que vit votre application)
toolMgr := tools.NewManager()
toolMgr.Register(&MonOutilMetier{}) // Outil implémentant l'interface tools.Tool

// 3. L'agent : persona et instructions injectées, jamais codées dans la lib
ag := agent.New(agent.Config{
    Provider:     provider,
    SessionStore: sessions, // L'application injecte sa propre persistance si besoin
    Tools:        toolMgr,
    Identity:     "# Mon Assistant\n\nTu es …",
    Instructions: "## Instructions\n\n…",
    Workspace:    workspaceDir,
    Bus:          messageBus,
    
    // NOUVEAU v0.1.1 : Crochets métiers
    BeforeTool: func(ctx context.Context, call llm.ToolCall) (bool, string) {
        // Ex: Demander validation humaine, appliquer un garde-fou...
        return true, ""
    },
    AfterTool: func(ctx context.Context, call llm.ToolCall, result string) {
        // Ex: Télémétrie, workflows d'automatisation
    },
    ExtraPrompt: func() string {
        // Ex: Injecter les skills actifs, la data utilisateur dans le prompt système
        return ""
    },
    EnabledTools: func(name string) bool {
        // Ex: Désactiver certains outils dynamiquement selon le contexte
        return true
    },
    ParseInlineTools: true, // Recommandé si le modèle a du mal avec les appels d'outils natifs
})

// 4. Un canal (ex: WhatsApp via wotp) + la boucle de dispatch
wa := channels.NewWotp(channels.WotpConfig{ChatURL: url, ChatAPIKey: key}, messageBus)
// … voir livro-plateform/services/agent/cmd/agent/main.go pour un assemblage complet
```

L'identité de la conversation (canal + chat ID — le numéro de téléphone pour WhatsApp) circule via `tools.ConversationContext` dans le `context.Context` de chaque appel de tool : un tool ne peut jamais agir sur une autre identité que celle qui écrit.

## Garanties de la boucle

- **Historique complet** : chaque tour enregistre le message, les appels d'outils, leurs
  résultats et la réponse. Au tour suivant, le modèle sait ce qu'il a réellement fait
  (référence, identifiant, date) au lieu de le deviner.
- **Historique toujours valide** : la coupe (`session`, 60 messages) se fait à une frontière
  de tour, et l'historique est réparé avant envoi (`llm.SanitizeHistory`) : jamais de
  résultat d'outil orphelin ni d'appel sans réponse.
- **Note éphémère** : `InboundMessage.Note` accompagne le message pour ce seul tour (fiche,
  mission en cours) et n'est jamais enregistrée.
- **Outils** : seul un outil exposé (`EnabledTools`) peut être exécuté ; les variantes de nom
  courantes (`functions.x`, casse, jetons de format) sont réparées ; un nom inconnu, des
  arguments invalides ou un paramètre obligatoire manquant renvoient une erreur lisible au
  modèle pour qu'il se corrige ; un résultat trop long est tronqué (`MaxToolResultChars`).
- **Toujours une réponse** : une réponse vide est retentée une fois ; budget d'étapes épuisé
  ou réponses vides, le modèle est appelé une dernière fois sans outils pour répondre ;
  à défaut, `FallbackReply`.

## Notes

- `mattn/go-sqlite3` impose **cgo** : compilez avec `CGO_ENABLED=1`.
- Le format des tools (nom, description, JSON Schema, résultat) est volontairement aligné sur les concepts MCP / function-calling OpenAI-compatible, pour laisser la porte ouverte à un adaptateur MCP sans casser l'API.

## Tests

```bash
make test
```
