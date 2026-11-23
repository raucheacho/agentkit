# Compatibilité

## Modèles

| Fournisseur | Comment | Vérifié |
|---|---|---|
| Ollama (local) | `openai.Ollama("", "llama3.1:8b")` ; embeddings `nomic-embed-text` | essais réels de toutes les phases (`llama3.1:8b`) |
| OpenAI | `openai.Config{APIKey, Model}` | tests avec faux serveur |
| Groq | `openai.Groq(clé, modèle)` | tests avec faux serveur |
| OpenRouter | `openai.OpenRouter(clé, modèle)` | tests avec faux serveur |
| DeepSeek | `openai.DeepSeek(clé, modèle)` | tests avec faux serveur |
| vLLM, LM Studio, Mistral… | toute API `/chat/completions` : `openai.Config{BaseURL, APIKey, Model}` | non testés |
| Autres API | un paquet `llm/<nom>` (voir `DEVELOPPEMENT.md` §5) | — |

Pour les petits modèles (≤ 8B), activer `ParseInlineTools` et limiter le nombre d'outils exposés.

## Canaux

| Canal | Réception | Envoi | Particularités |
|---|---|---|---|
| Web | `POST /messages` | SSE `GET /events`, rattrapage après coupure | identification par `chat_id` ou fonction `Identify` |
| Telegram | API Bot, attente longue | HTML (repli texte), découpe à 3 800 caractères | liste d'expéditeurs autorisés ; « en train d'écrire » |
| WhatsApp | webhook signé (HMAC) | API REST d'une passerelle | « en train d'écrire » ; messages pris en main côté passerelle ignorés |

## Systèmes

| | macOS | Linux | Windows |
|---|---|---|---|
| Module (hors ordinateur) | ✅ testé (CI) | ✅ testé (CI) | prévu (Go pur, sans cgo) |
| `computer/local` | ✅ testé (CI) | ✅ testé (CI) | arrêt du seul processus principal, pas de son groupe |
| `computer/docker` | ✅ testé en local (Docker Desktop) | ✅ testé (CI, filtrage réseau compris) | prévu (Docker Desktop) |
| `contrib/vectix` | ✅ testé (arm64) | bibliothèques amd64 et arm64 fournies | non fourni par VectixDB |

« Testé (CI) » : vérifié à chaque push par `.github/workflows/ci.yml` (`go vet`, `go test -race`, tests Docker sur Linux). « Prévu » veut dire que rien ne l'empêche, pas que c'est vérifié. `contrib/vectix` n'est pas dans la CI tant que VectixDB n'est pas publié (`FEUILLE_DE_ROUTE.md` §0.3).

## Bases de données

| Base | État |
|---|---|
| SQLite (`modernc.org/sqlite`, sans cgo) | ✅ testé : tous les stockages, FTS5 |
| SQLite (`mattn/go-sqlite3`) | devrait fonctionner ; FTS5 seulement avec le tag de compilation `sqlite_fts5`, sinon recherche par `LIKE` |
| Postgres | à écrire (`FEUILLE_DE_ROUTE.md` §4.1) |
| Mémoire | ✅ tous les stockages ont une version en mémoire (tests, agents éphémères) |

## Protocoles

| Protocole | Version |
|---|---|
| MCP | `2025-06-18`, stdio et Streamable HTTP (réponses JSON ou SSE) |
| Skills | format `SKILL.md` (agentskills.io) : en-tête `name`, `description`, champs libres |
| Go | 1.26 ou plus récent |
