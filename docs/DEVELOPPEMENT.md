# Guide de développement

Ce guide s'adresse aux développeurs qui reprennent agentkit : comment le projet est construit, comment le faire tourner et le tester, les règles à respecter et les recettes des changements courants. Les décisions sont dans `docs/decisions.md` (ADR), la suite du travail dans `docs/FEUILLE_DE_ROUTE.md`, la référence des packages dans `docs/reference.md`. Pour **comprendre** le projet avant de le modifier (notions, visite guidée d'une commande dans le code, exercices), commencer par `docs/APPRENDRE.md`.

## 1. Vue d'ensemble

agentkit est un framework Go pour construire **n'importe quel agent** — assistant personnel proactif, agent de support multi-clients, agent qui code dans son propre ordinateur — sans réécrire la boucle, les canaux, la mémoire ou la sécurité. Il ne dépend que de la bibliothèque standard.

```
  Telegram ─┐                                   ┌─► modèle (llm/openai : OpenAI, Groq, Ollama…)
  WhatsApp ─┼─► channel.Gateway ─► turn.Queue ─► runner ─► agent (boucle)
  Web (SSE)─┘        ▲                 ▲  ▲                   │
                     │  schedule ──────┘  │                   ├─► outils ─► trust (porte unique)
                     │  observe ──────────┤                   │        └─► métier, mémoire, skills,
                     │  delegate (tâches)─┤                   │            delegate, computer, MCP
                     │  trust (reprises)──┘                   │
                     └──────────── réponse ◄──────────────────┘
                     event.Bus : trace JSONL, SSE, coûts      session (SQLite, prompt figé)
```

| Dossier | Rôle |
|---|---|
| `llm/` | contrat neutre avec un modèle : messages, requêtes, flux, usage, `Retry`, `Fallback`, réparation d'historique, `Embedder` |
| `llm/openai/` | tout fournisseur compatible `/chat/completions` et `/embeddings` |
| `tool/` | outils : schéma JSON (déduit d'une struct), effet, validation, conversion des types, registre, groupes |
| `agent/` | la boucle : prompt figé, note éphémère, outils, middlewares, attente, steering, flux, compression, événements |
| `prompt/` | assemblage du prompt système (texte, fichiers de contexte, sections) |
| `session/` | historique en ajout seul, compression (archivage + résumé), recherche ; `session/sqlite` |
| `turn/` | le tour et la file durable ; `turn/sqlite`, `turn/turntest` (contrat) |
| `runner/` | exécute la file : une session à la fois, sessions en parallèle, Stop, steering, reprise en main, silence |
| `channel/` | interface des canaux et `Gateway` ; `channel/web`, `channel/telegram`, `channel/whatsapp`, `channel/sqlite` (reprises en main) |
| `trust/` | la porte : politique, demandes liées aux arguments, exécution unique, reprise, `ask_human` ; `trust/sqlite`, `trust/trusttest` |
| `delegate/` | sous-agents en parallèle et tâches de fond ; `delegate/sqlite`, `delegate/delegatetest` |
| `memory/` | fournisseurs de mémoire à cycle de vie, `history_search` ; `memory/notes` (+ `sqlite`, `notestest`) |
| `skills/` | skills `SKILL.md` : index, `skill_view`, `skill_save` |
| `schedule/` | planification (every, cron, date) ; `schedule/sqlite`, `schedule/scheduletest` |
| `observe/` | proactivité : sources, dédoublonnage, regroupement, heures calmes ; `observe/sqlite`, `observe/observetest` |
| `computer/` | l'ordinateur de l'agent : contrat et outils ; `computer/docker`, `computer/local`, `computer/computertest` |
| `mcp/` | client MCP (stdio, HTTP) sans SDK |
| `event/` | flux d'événements, trace, SSE, compteur de coûts |
| `kit/` | assemblage complet en un appel, API d'administration |
| `contrib/vectix/` | **module à part** (cgo) : notes dans VectixDB |
| `examples/` | `cli` (minimal), `assistant` (façon Nod/Hermes), `support` (façon persibot), `internal/setup` (commun) |
| `internal/archtest/` | règles d'import vérifiées par un test |

Le **noyau** (`llm`, `tool`, `agent`, `prompt`, `session`, `turn`, `runner`, `channel`, `trust`, `delegate`, `memory`, `memory/notes`, `skills`, `schedule`, `observe`, `computer`, `mcp`, `event`) n'importe ni fournisseur, ni stockage, ni canal concret : ce sont des choix de l'application, faits dans `kit` ou dans son `main`.

## 2. Mise en route

Prérequis : **Go 1.26** ou plus récent. Pour les essais réels : un modèle (clé d'API, ou [Ollama](https://ollama.com) en local). Facultatif : Docker (ordinateur de l'agent), Zig et VectixDB (`contrib/vectix`).

```sh
make test         # tous les tests, avec -race
make vet
make build
```

Un exemple, avec un modèle local :

```sh
ollama pull llama3.1:8b
AGENTKIT_MODEL=llama3.1:8b ADMIN_TOKEN=secret go run ./examples/support
# autre terminal
curl -N "localhost:8080/web/events?chat_id=c1" &
curl -X POST localhost:8080/web/messages -d '{"chat_id":"c1","text":"Avez-vous des bougies ?"}'
curl -H "Authorization: Bearer secret" localhost:8080/admin/requests
```

Avec un fournisseur en ligne : `AGENTKIT_BASE_URL=https://api.groq.com/openai/v1 AGENTKIT_API_KEY=… AGENTKIT_MODEL=…`. Toutes les variables des exemples sont dans `docs/configuration.md`.

Données locales : `AGENTKIT_DIR` (défaut : un dossier dans le répertoire temporaire). On y trouve `agent.db` (tout l'état), `trace.jsonl`, et les fichiers de contexte facultatifs `AGENTS.md` et `SOUL.md`, lus à la création de chaque session.

**`go.work`** : si un `go.work` parent remplace ce module par une autre copie, lancer les commandes avec `GOWORK=off` (le `Makefile` le fait).

## 3. Tests

| Commande | Ce qui tourne |
|---|---|
| `make test` | tous les paquets, `-race` ; les tests Docker tournent si un démon Docker répond |
| `go test -short ./...` | sans les tests Docker (rapide) |
| `go test ./computer/docker/` | conteneur réel : contrat, isolation entre conversations, filtrage réseau, processus restants (construit l'image au premier passage, quelques minutes) |
| `cd contrib/vectix && go test ./...` | VectixDB (cgo, bibliothèque native) |
| `go test -count=20 ./paquet/` | débusquer un test instable |

Les **contrats** (`*test`) sont des suites partagées qu'une implémentation doit passer : `turntest`, `trusttest`, `delegatetest`, `sessiontest`, `notestest`, `scheduletest`, `observetest`, `computertest`. Une nouvelle implémentation (Postgres, Redis, un autre bac à sable) se branche sur le contrat existant : c'est lui qui garantit les invariants (exécution unique, reprise sans relance, cloisonnement…).

`internal/archtest` vérifie que le module ne dépend que de la bibliothèque standard et que le noyau n'importe aucune implémentation.

## 4. Règles du projet

- **Langues** : identifiants en **anglais** ; commentaires, messages destinés aux utilisateurs et au modèle, documentation en **français**. Les textes adressés au modèle sont regroupés dans des `Texts` remplaçables (ADR-021).
- **Bibliothèque standard seulement** dans le module (ADR-002). Une dépendance tierce ou cgo va dans un sous-module (`contrib/…`, avec son `go.mod`). Les exemples et les tests peuvent importer `modernc.org/sqlite`.
- **Le noyau ne choisit rien** : pas d'import de fournisseur, de stockage ou de canal concret depuis un paquet du noyau (`internal/archtest`).
- **Les invariants** (README) sont protégés par des tests ; un changement qui en affaiblit un passe par une ADR.
- **Décisions** : toute décision de conception va dans `docs/decisions.md` (ADR numérotée : contexte, décision, conséquences).
- **Un stockage = un contrat** : toute nouvelle implémentation passe la suite `*test` du paquet.
- **Pas de nouvelle tentative implicite** d'une action : seuls les appels au modèle sont réessayés (`llm.Retry`) ; un tour, une demande validée, une tâche de fond, une commande à l'issue inconnue ne sont jamais rejoués.
- **Concurrence** : `-race` obligatoire ; une course est un bogue même si le test passe.
- **Git** : branches `main` (stable) et `develop` (travail en cours) ; messages courts en français, en minuscules, sans préfixe (« ajout du navigateur ») ; aucune mention d'outils d'IA (ni auteur, ni `Co-Authored-By`) ; aucun fichier de configuration d'assistant de code dans le dépôt.

## 5. Recettes

### Ajouter un outil métier
1. Une struct d'arguments avec des tags `json` (`omitempty` ou pointeur = facultatif), `desc` (description) et `enum` (`enum:"a,b"`).
2. `tool.New("nom", "description", func(ctx, in T) (tool.Result, error) {…}, tool.WithEffect(…))`. L'effet compte : `Sensitive` fait valider par un humain.
3. `tool.ConversationFrom(ctx)` dit pour qui l'outil agit : ne jamais agir pour une autre conversation.
4. Un contenu venu de l'extérieur se renvoie avec `tool.External` ; une erreur que le modèle peut corriger, avec `tool.Text("Erreur : …")` (pas une `error` Go, réservée aux pannes).
5. Ajout dans `kit.Config.Tools` (ou `Toolsets` avec une fonction `Available`).

### Ajouter un canal
1. Un paquet `channel/<nom>` qui implémente `channel.Adapter` : `Name`, `Run(ctx, sink)` (reçoit, appelle `sink` pour chaque message), `Send(ctx, Outgoing)`.
2. Facultatif : `Typer` (« en train d'écrire »), `Limiter` (longueur maximale : le gateway découpe).
3. Remplir `Message.ID` (le gateway dédoublonne dessus) et `Message.Media` pour les pièces jointes.
4. Jamais de secret (jeton) dans une erreur renvoyée (voir `channel/telegram`, `APIError`).
5. Test : un faux serveur `httptest`, comme `channel/telegram/telegram_test.go`.

### Ajouter un fournisseur de modèle
1. Si l'API parle `/chat/completions` : rien à écrire, `openai.Config{BaseURL, APIKey, Model}` suffit (préréglages `openai.Groq`, `openai.OpenRouter`, `openai.DeepSeek`, `openai.Ollama`).
2. Sinon : un paquet `llm/<nom>` qui implémente `llm.Provider` (et `llm.Streamer` pour le flux), en traduisant `llm.Request` ; renvoyer `*llm.StatusError` pour les erreurs HTTP, pour que `llm.Retry` et `llm.Fallback` reconnaissent les erreurs passagères.
3. Remplir `Usage.CachedTokens` si l'API le fournit (compteur de coûts, taux de cache).

### Ajouter un stockage (Postgres, Redis…)
1. Un paquet `<brique>/<moteur>` qui implémente l'interface `Store` de la brique.
2. Les transitions exclusives (`Transition`, `Finish`, `Advance`, `Claim`) doivent être **atomiques** : mise à jour conditionnelle (`UPDATE … WHERE status = ?`), pas lecture puis écriture.
3. Passer la suite de contrat (`trusttest.Run`, `scheduletest.Run`…).
4. Le brancher dans `kit` (`openStores`) si on veut qu'il soit choisi automatiquement.

### Ajouter un fournisseur de mémoire
Implémenter `memory.Provider` (`Name`) et seulement les interfaces utiles : `Prompter` (bloc figé du prompt), `Recaller` (rappel par tour, doit rendre la main vite : borné par `RecallTimeout`), `ToolProvider`, `Syncer` (après le tour, en arrière-plan), `PreCompactor`. L'ajouter à `kit.Memory.Providers`.

### Ajouter une source d'observation
`observe.PollerFunc("nom", func(ctx) ([]observe.Event, error))` : chaque événement a une `Key` stable (le dédoublonnage repose dessus) ; la source peut renvoyer une fenêtre glissante, les déjà vus sont écartés. Pour un webhook, appeler `Observer.Push`. Exemple complet : `examples/assistant/rss.go`.

### Ajouter un bac à sable pour l'ordinateur
Implémenter `computer.Workspace` et `computer.Session`, puis passer `computertest.Run` : pas de fuite d'environnement, aucun chemin hors du dossier (liens symboliques compris), délai, annulation, processus détachés arrêtés, sortie bornée. Une commande dont on ne sait pas si elle s'est terminée renvoie `computer.ErrUncertain`.

### Changer une règle de confiance
Composer avec `trust.Rules(trust.Default(), trust.Rule{…})` : une règle filtre par outils, effets, sources et canaux, et donne une décision (`Allow`, `Deny`, `Ask`) avec une raison. Ajouter le cas dans un test : c'est la partie la plus sensible du framework.

### Ajouter un texte adressé au modèle
Le placer dans le `Texts` du paquet (avec sa valeur française par défaut dans `DefaultTexts`), jamais en dur dans le code : une application doit pouvoir le traduire ou l'adapter.

## 6. Livrer

- `develop` → `main` par fusion, puis un tag `vX.Y.Z` : c'est ce que récupèrent `go get github.com/raucheacho/agentkit@vX.Y.Z` et le proxy Go.
- Le sous-module se tague à part : `contrib/vectix/vX.Y.Z`. Retirer ses `replace` locaux avant (ils pointent vers les dossiers voisins de la machine de développement) et attendre que VectixDB publie ses bibliothèques compilées.
- La v0.2 **change l'API** par rapport à la v0.1 : annoncer la rupture dans les notes de version.

## 7. Problèmes connus et limites

| Sujet | État |
|---|---|
| Navigateur de l'agent | pas encore fait (feuille de route) |
| Petits modèles | un modèle 8B se perd au-delà d'une quinzaine d'outils et peut affirmer avoir fait ce qu'un outil a refusé ; le framework n'exécute rien de tel, mais la réponse peut être fausse |
| Fournisseurs | seule l'API compatible OpenAI est fournie ; une API différente demande son propre paquet `llm/…` |
| `computer/local` | **aucune isolation** ; non pris en charge sous Windows au-delà de l'arrêt simple du processus |
| `computer/docker` | une seule machine ; les processus qu'une commande détache dans une nouvelle session (`setsid`) échappent à l'arrêt du groupe |
| Observation poussée | les événements reçus par `Push` et pas encore regroupés sont perdus si le processus s'arrête (les sources relevées, elles, les renvoient) |
| `contrib/vectix` | dépend de `replace` locaux tant que VectixDB n'est pas publié avec ses bibliothèques |
| Persibot | utilise encore l'API v0.1 (commit `79a5dd5`) ; migration sur `kit` à faire |
