# Référence des packages

Pour chaque package : son rôle, ses types et fonctions principaux, et ses valeurs par défaut. La documentation complète est dans le code (`go doc ./<paquet>`). Les paquets marqués **noyau** n'importent aucune implémentation concrète (ADR-002).

## llm — noyau
Contrat neutre avec un modèle.

| Élément | Rôle |
|---|---|
| `Message{Role, Content, ToolCalls, ToolCallID, Name, Reasoning}` | un message ; rôles `RoleSystem`, `RoleUser`, `RoleAssistant`, `RoleTool` |
| `Request{Model, Messages, Tools, ToolChoice, Temperature, MaxTokens}` / `Response{Message, Usage, FinishReason, Model}` | un appel |
| `Provider` (`Chat`), `Streamer` (`Stream`), `ModelLister` | ce qu'un fournisseur implémente ; `ProviderFunc` pour les tests |
| `Usage{InputTokens, OutputTokens, CachedTokens}` | jetons consommés |
| `Retry(p, essais, délai)` | réessaie les erreurs passagères (408, 409, 425, 429, 5xx, 529, réseau) ; pour un flux, seulement l'ouverture |
| `Fallback(p1, p2, …)` | passe au fournisseur suivant sur erreur passagère |
| `StatusError`, `Transient(err)` | erreur HTTP typée ; erreur passagère ou non (une annulation ne l'est jamais) |
| `TrimHistory`, `SanitizeHistory` | coupe l'historique à une frontière de tour ; répare les appels d'outils sans réponse |
| `Embedder`, `EmbedderFunc`, `Cosine` | vecteurs (recherche sémantique) |

## llm/openai
Tout fournisseur compatible `/chat/completions` et `/embeddings`.

- `openai.New(Config{Name, BaseURL, APIKey, Model, Headers, HTTPClient})` : `Chat`, `Stream` (appels d'outils assemblés, usage final), `ListModels`.
- Préréglages : `openai.Groq(clé, modèle)`, `OpenRouter`, `DeepSeek`, `Ollama(url, modèle)` (défaut `http://localhost:11434/v1`).
- `openai.NewEmbedder(cfg, modèle)` : embeddings (`text-embedding-3-small`, `nomic-embed-text`…).
- Lit `reasoning_content` / `reasoning` et les jetons en cache ; transmet le schéma des outils tel quel.

## tool — noyau
Les outils.

| Élément | Rôle |
|---|---|
| `Tool` (`Spec`, `Run`) | un outil |
| `Spec{Name, Description, Parameters, Effect, MainOnly}` | sa description ; `Parameters` est un JSON Schema transmis tel quel |
| `Effect` : `Read`, `Write`, `Sensitive` | ce qu'il fait au monde (la porte décide d'après lui) |
| `New[In](nom, description, run, options…)` | outil typé : schéma déduit de `In` (tags `json`, `desc`, `enum`) |
| `Func(nom, description, schéma, run, options…)` | outil à schéma brut |
| `WithEffect`, `WithSchema`, `MainOnly` | options |
| `Call{ID, Name, Args, Raw, Spec}`, `Call.Decode(&v)` | un appel résolu et validé |
| `Result{Content, Untrusted, Wait}` ; `Text`, `External`, `Pause` | résultat ; `External` = donnée non fiable ; `Pause` suspend le tour |
| `Wait{Kind, Ref, Reply}` | ce que le tour attend (validation, question) |
| `Registry` : `Add`, `AddSet`, `Get`, `Specs`, `Validate` | registre ordonné (liste stable pour le cache) |
| `Toolset{Name, Tools, Available}` | groupe d'outils disponible sous condition |
| `Conversation{Session, Channel, ChatID, Sender, Source, Turn, Parent}` ; `WithConversation`, `ConversationFrom` | pour qui un outil agit (dans le `context`) |
| `SchemaOf`, `Validate`, `Coerce`, `Resolve` | schéma par réflexion ; validation ; conversion des types ; résolution des variantes de nom |

## agent — noyau
La boucle.

`agent.New(Config)` puis `Run(ctx, Input) (*Outcome, error)` ; `Record` (ajoute des messages sans tour : reprise en main), `Reset`, `RefreshSystem`, `Tools`, `Sessions`.

| `Config` | Défaut | Rôle |
|---|---|---|
| `Provider`, `Model`, `Temperature`, `MaxTokens` | — | le modèle |
| `Tools`, `Expose`, `Middleware` | registre vide | outils, filtre par tour, middlewares (la porte en premier) |
| `Sessions` | en mémoire | historique |
| `System` | `Texts.Identity` | construit le prompt d'une nouvelle session (signature de `prompt.Builder.Build`) |
| `MaxSteps` | 20 | appels au modèle par tour |
| `HistoryLimit` | 60 | messages relus (sans compression) |
| `MaxToolResultChars` | 12 000 | résultat d'outil tronqué (début et fin) |
| `ParseInlineTools` | non | lire un appel écrit en texte (petits modèles) |
| `Clock`, `Location`, `NoTimeNote` | heure locale | note de date |
| `Recall`, `AfterTurn` | — | crochets mémoire (rappel par tour, après le tour) |
| `Compaction` | — | compression : `MaxMessages` 80, `MaxChars` 60 000, `Keep` 20 (≤ `MaxMessages`/2), `Summarize`, `OnCompact` |
| `Texts` | français | textes adressés au modèle |
| `OnEvent`, `Logger` | — | événements (`EventModel`, `EventTool`, `EventTurn`), journal |

`Input{Conversation, Content, Note, Steer, NoTools, Stream}` ; `Outcome{Status (Done|Waiting), Reply, Wait, Saved, Usage, Steps}`.

## prompt — noyau
`prompt.New(sections…).Build` : `Text`, `Func`, `Heading(titre, section)`, `File(chemin, max)`, `Files(dossier, max, noms…)` (absent = rien ; trop long = début et fin gardés).

## session — noyau
`Store` (`Load`, `Append`, `SetSystem`, `Reset`) ; facultatifs `Compactor` (`Compact` : archive + résumé) et `Searcher` (`Search(Query{Text, Keys, Limit})`). `NewMemory()` implémente les trois. `Fold` (minuscules sans accents) et `Terms` (termes de recherche).
- **session/sqlite** : `New(ctx, db, préfixe)` ; tables `agentkit_sessions`, `agentkit_messages`, `agentkit_messages_fts`.
- **session/sessiontest** : contrat compression + recherche.

## turn — noyau
`Turn{ID, Session, Source, Channel, ChatID, Sender, Content, Note, Meta, Status, Reply, Error, MergedInto, CreatedAt, NotBefore, StartedAt, FinishedAt}`.
- Sources : `SourceUser`, `SourceSchedule`, `SourceEvent`, `SourceTask`, `SourceResume`, `SourceSystem`.
- Statuts : `Queued`, `Running`, `Done`, `Waiting`, `Failed`, `Cancelled`, `Merged`, `Held`.
- `Meta` : `MetaTools` (`"none"` = sans outils), `MetaSilence` (jeton de silence) ; `Silent(t, réponse)`.
- `Queue` : `Enqueue`, `Claim`, `TakeQueued`, `Finish`, `Cancel`, `Get`, `List`, `Recover`. `NewMemory()`, **turn/sqlite**, **turn/turntest**.

## runner — noyau
`runner.New(Config)` : `Submit`, `Wake`, `Run`, `Stop(id)`, `StopSession(session)`, `Queue`, `Agent`.

| `Config` | Défaut |
|---|---|
| `Agent`, `Queue` | obligatoires |
| `Outbox` (livraison), `Holds` (reprises en main) | — |
| `Workers` | 8 |
| `Poll` | 1 s |
| `TypingEvery` | 4 s |
| `Prepare` | note propre au tour |
| `FailureReply` | message en cas d'échec d'un tour `user` |
| `OnFinish`, `Logger` | — |

## channel — noyau
`Adapter` (`Name`, `Run(ctx, sink)`, `Send(ctx, Outgoing)`) ; facultatifs `Typer`, `Limiter`. `Message{Channel, ChatID, Sender, SenderName, Text, Media, ID, Group, At, Meta}`.

`NewGateway(Config, adaptateurs…)` : `Attach(engine, recorder)`, `Run`, `Send`, `SendText`, `Typing`, `Take`, `Release`, `Reply`, `Held`, `HeldSessions`, `LoadHolds`. `Config{Allow, Session, Describe, Holds, DedupWindow (10 min), Logger}`.

| Canal | Construction | Notes |
|---|---|---|
| **channel/web** | `web.New(Config{Name, Identify, Backlog, KeepAlive})` | `POST /messages`, `GET /events` (SSE, rattrapage `Last-Event-ID`) |
| **channel/telegram** | `telegram.New(Config{Token, AllowFrom, …})` | attente longue, HTML avec repli, jeton jamais dans une erreur, 401/409 arrêtent le canal |
| **channel/whatsapp** | `whatsapp.New(Config{BaseURL, APIKey, WebhookSecret, Name, HTTPClient})` | webhook signé HMAC (`Handler`), API REST |
| **channel/sqlite** | `NewHolds(ctx, db, préfixe)` | reprises en main durables |

## trust — noyau
La porte.

- Politique : `Default()` (Read et Write autorisés, Sensitive à valider), `ByEffect(table)`, `Rules(repli, Rule{Tools, Effects, Sources, Channels, Decision, Reason}…)`, `PolicyFunc`. Décisions `Allow`, `Deny`, `Ask`.
- `trust.New(Config{Store, Tools, Policy, Resume, TTL, ExecTimeout (60 s), Summarize, OnChange, AskToolName ("ask_human"), AskToolDescription, Texts, Logger})`.
- `Manager` : `Middleware`, `AskTool`, `Approve(id, par)`, `Deny(id, par, raison)`, `Answer(id, par, réponse)`, `Expire`, `Run`, `Attach`, `Store`.
- `Request{ID, Kind, Tool, Args, ArgsHash, Summary, Conversation, Status, Result, DecidedBy, CreatedAt, DecidedAt, ExpiresAt}` ; statuts `Pending`, `Approved`, `Executing`, `Executed`, `Failed`, `Denied`, `Answered`, `Expired`.
- **trust/sqlite**, **trust/trusttest**.

## delegate — noyau
`delegate.New(Config)` : `Tools()` (`delegate`, `start_task`, `list_tasks`, `cancel_task`), `Delegate`, `DelegateAll`, `Start`, `Cancel`, `Run`, `Running`, `Attach`.

| `Config` | Défaut |
|---|---|
| `Child` (configuration des sous-agents) | sessions éphémères, `MaxSteps` 12 |
| `MaxTasks` / `MaxParallel` | 5 / 3 |
| `Timeout` | 5 min |
| `Store` | sans : pas de tâches de fond |
| `MaxBackground` / `BackgroundTimeout` | 4 / 30 min |
| `Results`, `OnFinish`, `Texts`, `Logger` | — |

**delegate/sqlite**, **delegate/delegatetest**.

## memory — noyau
`memory.New(Config{RecallTimeout (5 s), SyncTimeout (2 min), Logger}, fournisseurs…)` : `Section`, `Recall`, `AfterTurn`, `OnCompact`, `Tools`, `Wait`. Interfaces d'un fournisseur : `Provider` (`Name`), `Prompter`, `Recaller`, `ToolProvider`, `Syncer`, `PreCompactor`. `Scope(conv)`. `HistoryTool(HistoryConfig{Searcher, Keys, Limit (8), MaxChars (400)})`.

## memory/notes — noyau
`notes.New(Config)` : fournisseur (prompt, rappel, outils `memory_save`, `memory_read`, `memory_search`, `memory_delete`), `Save`, `Search`.

| `Config` | Défaut |
|---|---|
| `Store` | obligatoire (`NewMemory()`, **memory/notes/sqlite**, `contrib/vectix`) |
| `Embedder` | sans : plein texte seul |
| `Scope` | `memory.Scope` (la conversation) |
| `IndexLimit` | 50 notes dans le prompt |
| `RecallLimit` | 3 notes rappelées par tour |
| `MinSimilarity` | 0,3 |
| `MaxNoteChars` | 4 000 |

Chemins `zone/nom` (`CleanPath`, `Area`). Interfaces `Store` et `VectorStore`. Contrat **memory/notes/notestest**.

## skills — noyau
`skills.Load(dossiers…)` : `List`, `Get`, `Reload`, `Errors`, `Section` (index pour le prompt), `ViewTool` (`skill_view`, sans sortir du dossier du skill), `SaveTool(dossier)` (`skill_save`). `Parse(texte)` lit un `SKILL.md`.

## schedule — noyau
`schedule.New(Config)` : `Add`, `Pause`, `Resume`, `Run`, `Tick`, `Tools` (`schedule_create`, `schedule_list`, `schedule_cancel`), `Attach`. `Parse(texte, fuseau, maintenant)`.

| `Config` | Défaut |
|---|---|
| `Store`, `Submit` | obligatoires (ou `Attach`) |
| `Location` | heure locale |
| `Tick` | 30 s |
| `Grace` | 1 h (au-delà, échéance sautée) |
| `MinInterval` / `MaxPerSession` | 5 min / 20 (outils de l'agent) |
| `Silence` | `[SILENCE]` |
| `OnRun`, `Texts`, `Logger`, `Now` | — |

**schedule/sqlite**, **schedule/scheduletest**.

## observe — noyau
`observe.New(Config)` : `Push`, `Poll`, `Flush`, `Run`, `Attach`. `Event{Key, Source, Title, Body, At, Urgent, Conversation, Meta}` ; `Poller`, `PollerFunc` ; `Quiet{Start, End, Location}` ; `Seen` (`NewMemorySeen`, **observe/sqlite**, **observe/observetest**).

| `Config` | Défaut |
|---|---|
| `Interval` | 5 min entre deux relèves |
| `Window` | 1 min de regroupement (< 0 : aucun) |
| `MaxBatch` | 20 événements par tour |
| `Route` | conversation d'un événement qui n'en a pas |
| `Quiet` | aucune heure calme |
| `WithTools` | non : le tour d'observation n'a pas d'outils |
| `Silence` | `[SILENCE]` |
| `Retention` | 30 jours |

## computer — noyau
Contrat `Workspace` (`Open(ctx, clé)`, `Close`) et `Session` (`Dir`, `Exec`, `ReadFile`, `WriteFile`) ; `ExecOptions{Stdin, Timeout, MaxOutput}`, `ExecResult{ExitCode, Output, Truncated, TimedOut, Duration}` ; `ErrUncertain`, `ErrPath` ; `Folder`, `Clean`, `Key`, `Output`.

`computer.Tools(ws, ToolsConfig)` : `bash`, `read_file`, `write_file`, `edit_file`.

| `ToolsConfig` | Défaut |
|---|---|
| `BashEffect` | `Write` (`Sensitive` : chaque commande validée) |
| `DefaultTimeout` / `MaxTimeout` | 10 min / 60 min |
| `MaxOutputChars` | 30 000 |
| `Redact` | aucun (`Redactor(secrets…)`) |
| `RecordChars` | 16 000 |
| `OnExec` | historique des commandes |

| Implémentation | Construction | Défauts |
|---|---|---|
| **computer/docker** | `docker.New(Config{Name, Image, Network, Memory, CPUs, Pids, DefaultTimeout, MaxOutput, Docker})`, `Ensure`, `Remove` | `agentkit-workspace`, image `agentkit-workspace:2` (construite depuis le `Dockerfile` embarqué), réseau `public`, `2g`, 2 CPU, 1 024 processus, 10 min, 1 Mio |
| **computer/local** | `local.New(Config{Root, Shell, Env, DefaultTimeout, MaxOutput})` | **aucune isolation** |

Contrat **computer/computertest**.

## mcp — noyau
`mcp.Connect(ctx, Config)` : `Toolset`, `Call`, `Infos`, `Instructions`, `Alive`, `Close`.

| `Config` | Défaut |
|---|---|
| `Name` | obligatoire, préfixe des outils |
| `Command`, `Args`, `Env`, `Dir` | stdio |
| `URL`, `Headers`, `HTTPClient` | HTTP |
| `Prefix` | `Name_` (`-` : aucun) |
| `Include`, `Exclude` | tous les outils |
| `Effects`, `TrustAnnotations` | `Write` par défaut, `Sensitive` si destructif |
| `CallTimeout` / `MaxResultChars` | 2 min / 20 000 |

Protocole `2025-06-18` (`ProtocolVersion`).

## event — noyau
`event.NewBus(Config{Backlog (500), Buffer (256), TextChars (300)})` : `Publish`, `Subscribe`, `Since`, `Attach` (récepteur synchrone), `Dropped`. Branchements : `Agent()`, `Runner()`, `Trust()`, `Delegate()`, `Schedule()`, `Computer()`. Récepteurs : `JSONL(w)`, `Log(logger)`. HTTP : `Handler(bus, HandlerConfig{Filter, KeepAlive})` (SSE), `QueryFilter`, `CostHandler(meter)`.

Types d'événements : `model`, `tool`, `turn`, `finished`, `approval`, `delegate`, `schedule`, `command`, `custom`.

`event.NewMeter(tarifs)` : `Observe`, `Total`, `Session`, `Snapshot(top)` ; `Price{Input, CachedInput, Output}` par million de jetons (le plus long préfixe de nom de modèle s'applique).

## kit
Assemblage. `kit.New(ctx, Config)` → `*Kit` : `Run`, `Submit`, `Close`, `Admin(jeton)`, et ses briques (`Agent`, `Runner`, `Gateway`, `Gate`, `Team`, `Scheduler`, `Observer`, `Memory`, `Skills`, `Bus`, `Meter`, `Tools`, `Sessions`, `Queue`, `Requests`, `Jobs`, `Tasks`). Détail de `Config` et routes d'administration : `docs/configuration.md`.

## contrib/vectix (module à part)
`vectix.New(db, Config{Collection, Dim, Model, Quantized})` : `notes.Store` et `notes.VectorStore` sur VectixDB. Demande cgo et la bibliothèque native de VectixDB.
