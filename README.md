# agentkit

Framework Go pour construire des agents à outils : un assistant personnel proactif, un agent
multi-canaux qui apprend, un agent d'entreprise supervisé… sans réécrire la boucle, les sessions,
les canaux, les subagents ou la planification à chaque projet.

Le noyau ne connaît aucun métier, aucun canal et aucun fournisseur. L'application apporte ses
outils, son prompt et sa politique ; agentkit apporte le reste.

> **v0.2 en cours.** Phases 1 à 8 livrées (l'ordinateur de l'agent n'a pas encore de navigateur). Voir la [feuille de route](#feuille-de-route).

## Packages

| Package | Rôle |
|---|---|
| `kit` | **Assemblage en un appel** : stockage (SQLite ou mémoire), file, canaux, porte, mémoire, skills, sous-agents, planification, observation, ordinateur, MCP, événements et coûts ; `Run`, `Submit`, API d'administration protégée par jeton |
| `llm` | Contrat neutre avec un modèle : `Provider`, `Request`/`Response`, flux, usage (dont jetons en cache), erreurs typées, `Retry`, `Fallback`, réparation d'historique, `Embedder` |
| `llm/openai` | Toute API compatible `/chat/completions` : OpenAI, Groq, DeepSeek, OpenRouter, Ollama, vLLM… ; embeddings (`/embeddings`) |
| `tool` | Outils : JSON Schema complet, effet (`Read`, `Write`, `Sensitive`), résultat non fiable, attente (`Wait`), outils typés avec schéma déduit, registre, groupes à disponibilité dynamique |
| `agent` | La boucle : modèle → outils → réponse, middlewares d'outils, attente, steering, flux, événements |
| `prompt` | Assemblage du prompt système : texte, fichiers de contexte (`AGENTS.md`, `SOUL.md`…), sections |
| `session` | Historique en ajout seul avec prompt figé ; compression (`Compactor` : archive + résumé) et recherche (`Searcher`) facultatives ; `session.Memory` |
| `session/sqlite` | `session.Store` SQLite, sans pilote imposé (modernc.org/sqlite recommandé, sans cgo) ; recherche FTS5 sans accents, LIKE à défaut ; `session/sessiontest` vérifie le contrat |
| `memory` | Fournisseurs de mémoire à cycle de vie (prompt, rappel à chaque tour, outils, synchronisation après le tour, avant compression) ; outil `history_search` limité à la conversation |
| `memory/notes` | Notes « zone/nom » : index dans le prompt, rappel des notes pertinentes, outils `memory_save/read/search/delete`, recherche hybride (plein texte + vecteurs) ; `memory/notes/sqlite` (FTS5) ; `notestest` |
| `skills` | Skills `SKILL.md` (agentskills.io) : index dans le prompt, `skill_view` (sans sortir du dossier du skill), `skill_save` |
| `schedule` | Tâches planifiées (« every 2h », cron, date, « in 20m ») : chaque échéance devient un tour `schedule`, au plus une fois ; outils `schedule_create/list/cancel` ; `schedule/sqlite` ; `scheduletest` |
| `observe` | Proactivité : sources relevées ou poussées, dédoublonnage durable, regroupement par conversation, heures calmes, tour `event` qui peut se taire ; `observe/sqlite` ; `observetest` |
| `mcp` | Client MCP sans SDK (stdio et Streamable HTTP) : les outils d'un serveur deviennent un `tool.Toolset`, résultats non fiables, effet décidé par l'application et non par le serveur |
| `event` | Flux unique (modèle, outils, tours, validations, sous-agents, planifications) : branchements prêts pour chaque package, trace JSONL, journal, SSE avec rattrapage, compteur de coûts par session et par modèle |
| `computer` | L'ordinateur de l'agent : contrat `Workspace`/`Session`, outils `bash`, `read_file`, `write_file`, `edit_file`, historique des commandes expurgé ; `computertest` |
| `computer/docker` | Conteneur verrouillé via la commande docker (sans SDK) : aucune capacité, racine en lecture seule, Internet public seulement, un utilisateur Linux par conversation, superviseur de commandes (délai, groupe de processus, issue incertaine) |
| `computer/local` | Un dossier de la machine, **sans isolation** : développement et tests |
| `contrib/vectix` | Module à part (cgo) : notes dans VectixDB (HNSW + BM25 + filtres) pour les grands volumes |
| `turn` | Le tour, unité de travail durable : source (message, planification, événement, tâche, reprise), où répondre, statut ; file `turn.Queue` et `turn.Memory` |
| `turn/sqlite` | `turn.Queue` SQLite ; `turn/turntest` vérifie le contrat de toute implémentation |
| `runner` | Exécute la file : un tour à la fois par session, sessions en parallèle, Stop, steering, reprise en main, « en train d'écrire », réponse en cas d'échec |
| `channel` | Interface d'adaptateur et capacités facultatives ; `Gateway` : filtrage, anti-doublon, création des tours, découpe des réponses longues, reprise en main (`Take`, `Reply`, `Release`) |
| `channel/sqlite` | Reprises en main durables (`channel.HoldStore`) |
| `trust` | Porte de confiance : politique (`Default`, `ByEffect`, `Rules`), demandes de validation liées aux arguments exacts et exécutées une seule fois, tours de reprise, outil `ask_human`, expiration |
| `trust/sqlite` | `trust.Store` SQLite ; `trust/trusttest` vérifie le contrat |
| `delegate` | Sous-agents : tâches confiées en parallèle dans le tour (outil `delegate`) ou en tâche de fond dont le compte rendu revient comme un tour `task` (`start_task`, `list_tasks`, `cancel_task`) |
| `delegate/sqlite` | `delegate.Store` SQLite ; `delegate/delegatetest` vérifie le contrat |
| `channel/whatsapp` | WhatsApp par une passerelle HTTP (webhook signé + API REST) |
| `channel/telegram` | Telegram, API Bot en attente longue, sans SDK ; le jeton n'apparaît jamais dans une erreur |
| `channel/web` | Site ou application : POST pour écrire, SSE pour recevoir, rattrapage après coupure (`Last-Event-ID`) |

## Exemple

```go
k, err := kit.New(ctx, kit.Config{
	Provider:     llm.Retry(openai.New(openai.Ollama("", "llama3.1:8b")), 3, time.Second),
	DB:           db, // SQLite (modernc.org/sqlite) ; nil : tout en mémoire
	Instructions: "Tu es le conseiller clientèle de l'atelier.",
	Channels:     []channel.Adapter{telegram.New(telegram.Config{Token: token})},
	Tools:        []tool.Tool{catalogSearch, createOrder}, // createOrder : tool.Sensitive
	Trust:        kit.Trust{AskHuman: true, TTL: 48 * time.Hour},
	Memory:       &kit.Memory{},
	Compaction:   &agent.Compaction{},
})
if err != nil {
	return err
}
defer k.Close()
http.Handle("/admin/", http.StripPrefix("/admin", k.Admin(adminToken)))
return k.Run(ctx)
```

Tout est facultatif sauf le modèle, et la porte de confiance est toujours en place. Chaque brique
reste utilisable seule (voir `examples/cli` : un agent en quelques lignes, sans `kit`).

Trois exemples :

| Exemple | Style | Ce qu'il montre |
|---|---|---|
| `examples/cli` | minimal | la boucle seule, dans un terminal |
| `examples/assistant` | Nod, Hermes | un propriétaire, mémoire partagée entre ses conversations, skills écrits par l'agent, tâches de fond, rappels planifiés, veille RSS, ordinateur (Docker ou local), MCP |
| `examples/support` | persibot | de nombreux clients, mémoire par client, catalogue et commandes, commande validée par le responsable, questions au responsable, reprise en main, pas d'ordinateur |

```sh
AGENTKIT_MODEL=llama3.1:8b ADMIN_TOKEN=… go run ./examples/support
curl -X POST localhost:8080/web/messages -d '{"chat_id":"c1","text":"Avez-vous des bougies ?"}'
curl -H "Authorization: Bearer $ADMIN_TOKEN" localhost:8080/admin/requests
```

## Invariants

1. **Le prompt système est figé pour la session.** Il est calculé à la création de la session,
   puis réutilisé tel quel : le fournisseur sert le début de la conversation depuis son cache.
   Ce qui change à chaque tour (date, fiche, souvenirs rappelés) passe par la note éphémère.
   `RefreshSystem` le recalcule explicitement. Seule entorse : la compression, qui ajoute un
   résumé après le prompt — une fois par compression, jamais à chaque tour.
2. **L'historique est en ajout seul et toujours valide.** Chaque tour enregistre son message, ses
   appels d'outils, leurs résultats et sa réponse, même en cas d'erreur ou d'annulation. La relecture
   coupe à une frontière de tour et répare les appels sans réponse (`llm.SanitizeHistory`). La
   compression archive, elle n'efface pas : l'historique complet reste consultable.
3. **Seul un outil montré au modèle peut être exécuté.** Les variantes de nom sont réparées, un
   nombre ou un booléen envoyé en texte est converti selon le schéma ; arguments invalides ou
   paramètres manquants reviennent au modèle comme une erreur à corriger.
4. **Le contenu non fiable est une donnée.** Un résultat `tool.External` est délimité et le
   prompt système interdit d'obéir à ce qu'il contient.
5. **Une politique, des middlewares.** Les middlewares reçoivent un appel résolu et validé, avec
   son effet ; ils peuvent refuser, suspendre (`tool.Pause`) ou observer.
6. **Une session, un tour à la fois.** Les sessions différentes tournent en parallèle.
7. **Un tour n'est jamais rejoué automatiquement.** Seuls les appels au modèle sont réessayés
   (`llm.Retry`), jamais les outils.
8. **Tout est un tour, et un tour est durable.** Un message, une planification ou une reprise
   devient une ligne de la file ; un tour resté « en cours » après un arrêt brutal est marqué en
   échec, jamais rejoué, car ses effets sont inconnus.
9. **Un message reçu pendant un tour est absorbé par ce tour** (steering) plutôt que de déclencher
   une seconde réponse.
10. **Une seule porte décide, pas le modèle.** Une validation est liée à l'outil et à ses arguments
    exacts ; une action validée s'exécute une seule fois, sans repasser par le modèle ; une action
    interrompue en cours d'exécution est marquée en échec, jamais relancée. Un tour de reprise
    informe ensuite l'interlocuteur, dans la conversation d'origine.
11. **Un sous-agent ne fait qu'exécuter.** Il a son propre contexte et ne voit ni les outils
    réservés à l'agent principal (`tool.MainOnly` : déléguer, `ask_human`), ni les actions
    sensibles ; aucun de ses outils ne peut suspendre son tour. Son compte rendu est une donnée non
    fiable pour l'agent principal. Une tâche de fond coupée par un arrêt brutal échoue, n'est jamais
    relancée, et sa conversation en est informée.
12. **La mémoire est cloisonnée.** Notes et recherche dans l'historique sont limitées à la
    conversation (un sous-agent partage celle de son parent) ; les partager entre conversations
    est un choix explicite de l'application (`Scope`, `Keys`).
13. **La proactivité ne dérange pas pour rien.** Une échéance planifiée s'exécute au plus une fois
    (même avec plusieurs processus) et une échéance manquée de longue date est sautée, pas
    rattrapée en rafale ; un tour planifié ne peut pas planifier. Un événement n'est traité qu'une
    fois ; son contenu est une donnée, le tour qui l'examine n'a pas d'outils par défaut, peut se
    taire (`turn.MetaSilence`) et attend la fin des heures calmes, sauf urgence.
14. **Un serveur MCP est un tiers.** Ses résultats sont non fiables, et l'effet de ses outils
    (donc le besoin de validation) est fixé par l'application : il ne peut pas se déclarer
    « lecture seule » pour éviter la porte, sauf si l'application lui fait confiance.
15. **L'ordinateur de l'agent n'a rien à voler.** Aucune variable de l'application n'y entre ;
    chaque conversation a son dossier et son utilisateur Linux ; aucun chemin d'outil ne sort de
    son dossier (liens symboliques compris) ; une commande ne laisse aucun processus derrière
    elle ; une commande à l'issue inconnue n'est jamais relancée.
16. **Le module ne dépend que de la bibliothèque standard**, et le noyau ne choisit ni fournisseur,
    ni stockage, ni canal (vérifié par `internal/archtest`). Ce qui demande cgo vit dans un
    sous-module (`contrib/`).

## Feuille de route

1. ✅ Noyau : `llm`, `llm/openai`, `tool`, `agent`, `prompt`, `session`, `session/sqlite`.
2. ✅ Runtime et canaux : `turn`, `runner`, `channel` + passerelle + reprise en main, WhatsApp, Telegram, web.
3. ✅ Confiance : `trust`, validations asynchrones, `ask_human`, reprises en main durables.
4. ✅ Délégation : sous-agents en parallèle dans le tour, tâches de fond durables.
5. ✅ Mémoire, skills, compression : `memory` (fournisseurs à cycle de vie), notes, recherche dans
   l'historique, `SKILL.md`, embeddings, backend facultatif vectixdb.
6. ✅ Planification et proactivité : `schedule`, `observe`, tours silencieux.
7. ✅ MCP et observabilité : `mcp`, `event`, comptage des coûts.
7b. ✅ Ordinateur de l'agent : `computer`, `computer/docker`, `computer/local`. Navigateur à suivre.
8. ✅ Assemblage (`kit`) et exemples variés (`assistant`, `support`).
9. Navigateur de l'agent ; migration de persibot sur `kit` (chantier séparé).

## Tests

```bash
make test
```
