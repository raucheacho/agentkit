# Configuration

## `kit.Config`

Seul `Provider` est obligatoire. La porte de confiance est toujours en place ; tout le reste est désactivé tant qu'on ne le demande pas.

| Champ | Rôle |
|---|---|
| `Provider`, `Model`, `Temperature`, `MaxTokens` | le modèle (`Model` remplace celui du fournisseur) |
| `DB`, `TablePrefix` | base SQL (SQLite) pour tout l'état ; `nil` : tout en mémoire, perdu à l'arrêt ; préfixe `agentkit_` |
| `Instructions` | identité et consignes, en tête du prompt système |
| `ContextDir`, `ContextFiles` | fichiers de contexte lus à la création de chaque session (défaut `AGENTS.md`, `SOUL.md`) |
| `Prompt` | sections de prompt supplémentaires (`prompt.Section`) |
| `Tools`, `Toolsets` | outils métier |
| `Channels`, `Allow`, `Session` | canaux ; filtre des expéditeurs ; clé de session d'un message |
| `FailureReply` | message envoyé quand un tour échoue |
| `Trust` | `Policy`, `TTL`, `AskHuman` (outil `ask_human`), `Summarize`, `OnChange` (notifier le responsable) |
| `Memory` | `Embedder`, `Scope`, `NoNotes`, `NoHistory`, `SharedHistory`, `Providers` |
| `Skills` | `Dirs`, `Writable` (dossier où l'agent écrit ses skills) |
| `Compaction` | compression de l'historique (`&agent.Compaction{}` pour les défauts) |
| `Delegate` | `Provider`, `Model`, `MaxSteps` des sous-agents ; `Background` (tâches de fond) |
| `Schedule` | `AgentTools` (l'agent planifie lui-même), `Tick` |
| `Observe` | `*observe.Config` (sources, regroupement, heures calmes…) |
| `Computer`, `ComputerTools` | ordinateur de l'agent et réglages de ses outils |
| `MCP` | serveurs MCP (`[]mcp.Config`) |
| `Prices`, `TraceFile` | tarifs pour le compteur de coûts ; trace JSONL |
| `Location`, `Logger` | fuseau ; journal |
| `Agent` | dernier mot sur `agent.Config` (par exemple `ParseInlineTools`) |

### Recettes

Assistant personnel (un propriétaire, mémoire partagée) :
```go
Memory:   &kit.Memory{Scope: func(tool.Conversation) string { return "owner" }, SharedHistory: true},
Skills:   &kit.Skills{Writable: filepath.Join(dir, "skills")},
Delegate: &kit.Delegate{Background: true},
Schedule: &kit.Schedule{AgentTools: true},
```

Agent multi-clients (mémoire par client, rien de sensible hors d'une demande) :
```go
Memory: &kit.Memory{},
Trust: kit.Trust{AskHuman: true, TTL: 48 * time.Hour,
	Policy: trust.Rules(trust.Default(), trust.Rule{
		Effects: []tool.Effect{tool.Sensitive}, Sources: []string{"schedule", "event", "task"},
		Decision: trust.Deny, Reason: "action sensible hors d'une demande client"})},
```

Ordinateur isolé :
```go
ws := docker.New(docker.Config{Network: "public"})
// …
Computer: ws,
```

## API d'administration

`k.Admin(jeton)` ; à monter sous un préfixe, par exemple `http.StripPrefix("/admin", k.Admin(jeton))`. Authentification : `Authorization: Bearer <jeton>` ou `?token=`. Sans jeton configuré, tout est refusé (401).

| Route | Rôle |
|---|---|
| `GET /requests` | demandes en attente (`?session=`) |
| `POST /requests/{id}/approve` | valider : l'action s'exécute une fois ; 409 si déjà décidée |
| `POST /requests/{id}/deny` | refuser (corps : la raison) |
| `POST /requests/{id}/answer` | répondre à une question (corps : la réponse) |
| `POST /sessions/take?session=` | reprise en main : l'agent ne répond plus |
| `POST /sessions/release?session=` | rendre la main |
| `POST /sessions/reply?session=&channel=&chat_id=` | répondre à la place de l'agent (corps : le texte) |
| `GET /sessions/held` | conversations reprises en main |
| `GET /tasks` | tâches de fond |
| `GET /jobs` ; `POST /jobs/{id}/pause`, `/resume` ; `DELETE /jobs/{id}` | planifications |
| `GET /events` | flux SSE (`?session=`, `?kind=`, `?source=`, `?after=`) |
| `GET /costs` | coûts et usage (`?top=`) |

L'en-tête `X-Admin-User` nomme la personne qui décide (enregistrée dans `DecidedBy`, défaut `admin`).

## Variables des exemples

| Variable | Exemples | Défaut | Rôle |
|---|---|---|---|
| `AGENTKIT_BASE_URL` | tous | Ollama local (`cli` : OpenAI) | adresse de l'API compatible OpenAI |
| `AGENTKIT_API_KEY` | tous | — | clé de l'API |
| `AGENTKIT_MODEL` | tous | `llama3.1:8b` (`cli` : `gpt-4o-mini`) | modèle |
| `AGENTKIT_DIR` | tous | dossier temporaire | données : `agent.db`, `trace.jsonl`, `AGENTS.md`, `SOUL.md`, `skills/` |
| `AGENTKIT_ADDR` | assistant, support | `:8080` | adresse HTTP |
| `ADMIN_TOKEN` | assistant, support | tiré au hasard et affiché | jeton de l'administration |
| `AGENTKIT_EMBED_MODEL` | assistant | — | modèle d'embeddings (recherche hybride) |
| `AGENTKIT_COMPUTER` | assistant | aucun | `docker` ou `local` |
| `AGENTKIT_FEEDS` | assistant | — | flux RSS/Atom surveillés (séparés par des espaces) |
| `AGENTKIT_MCP` | assistant | — | serveur MCP : `nom commande arg…` |
| `TELEGRAM_TOKEN`, `TELEGRAM_OWNER` | assistant | — | Telegram ; identifiant du propriétaire (seul autorisé) |
| `WHATSAPP_URL`, `WHATSAPP_KEY`, `WHATSAPP_SECRET` | support | — | passerelle WhatsApp ; webhook sur `POST /whatsapp/webhook` |

Les routes web des exemples : `POST /web/messages {"chat_id":"…","text":"…"}` et `GET /web/events?chat_id=…` (SSE).
