# Feuille de route

État au 23/11/2026 : la v0.2 est sur la branche `develop`, un commit par phase. Ce document récapitule ce qui est fait et liste ce qui reste, avec pour chaque chantier l'approche prévue dans le code existant, les critères d'acceptation et les dépendances. Les tailles sont indicatives (S ≈ quelques jours, M ≈ une à deux semaines, L ≈ plus).

## Fait (v0.2)

| Phase | Contenu | Commit |
|---|---|---|
| 1 | Noyau : `llm`, `llm/openai`, `tool`, `agent`, `prompt`, `session` (+ SQLite), règle d'import | `nouveau noyau v0.2` |
| 2 | `turn`, `runner`, `channel` + gateway + reprise en main ; web, Telegram, WhatsApp | `tours, runner et canaux` |
| 3 | `trust` : politique, demandes liées aux arguments, exécution unique, reprise, `ask_human` | `porte de confiance` |
| 4 | `delegate` : sous-agents en parallèle, tâches de fond durables | `sous-agents et tâches de fond` |
| 5 | `memory`, notes, `history_search`, `skills`, compression, embeddings, `contrib/vectix` | `mémoire, skills et compression` |
| 6 | `schedule`, `observe`, tours silencieux | `planification et proactivité` |
| 7b | `computer` : workspace Docker verrouillé et local | `ordinateur de l'agent` |
| 7 | `mcp`, `event`, compteur de coûts | `client mcp et événements` |
| 8 | `kit`, `examples/assistant`, `examples/support` | `kit et exemples` |
| 0.1 | CI GitHub Actions : Linux, macOS, tests Docker sur Linux | `ajout de la ci` |

Chaque phase a été vérifiée par ses tests (`-race`, désormais aussi en CI), puis par un essai réel avec un modèle local 8B (`llama3.1:8b`, Ollama).

## 0. Préalables

| # | Chantier | Détail | Taille |
|---|---|---|---|
| 0.1 | ~~CI~~ | fait : `.github/workflows/ci.yml` (`gofmt`, `go vet`, `go test -race` sur Linux et macOS ; tests Docker sur Linux, qui échouent au lieu de sauter grâce à `AGENTKIT_REQUIRE_DOCKER`) | — |
| 0.2 | Fusion et version | fusionner `develop` dans `main`, taguer `v0.2.0`, notes de version annonçant la rupture d'API | S |
| 0.3 | VectixDB publié | publier VectixDB avec ses bibliothèques compilées, retirer les `replace` de `contrib/vectix`, taguer `contrib/vectix/v0.1.0` | S |
| 0.4 | Essai avec un modèle capable | rejouer les scénarios des exemples avec un modèle de la taille d'un GPT-4o : les écarts restants deviennent des tâches (les essais 8B mélangent défauts du modèle et du framework) | S |
| 0.5 | Licence | le dépôt n'a pas de fichier de licence : à choisir avant toute diffusion (MIT, Apache-2.0, AGPL…) | — |

## 1. Le navigateur de l'agent — L

### 1.1 Conteneur et pilotage
- **Approche** : comme Nod, un conteneur Chromium séparé du workspace (aucun volume ni réseau partagé, racine en lecture seule, Internet public seulement), piloté par le protocole DevTools (CDP) en WebSocket, **sans SDK** (stdlib : `net/http` + une petite implémentation WebSocket). Un paquet `browser` (contrat : `Open`, `Snapshot`, `Click`, `Type`, `Navigate`, `Screenshot`, `Download`) et `browser/chromium`.
- **Lecture de page** : arbre d'accessibilité en texte avec des références stables (`@e12`), comme agent-browser chez Nod ; captures d'écran pour les modèles qui voient.
- **Critères** : un navigateur par conversation (et par sous-agent), au plus N à la fois ; une page qui ne répond plus est tuée avec tous ses processus ; le contenu de la page est non fiable.

### 1.2 Actions qui engagent
- **Approche** : `click` sur un bouton d'envoi, de paiement ou de validation de formulaire passe par la porte avec la cible exacte (origine, URL, méthode du formulaire) ; `Sensitive` par défaut pour un formulaire `POST` hors de l'origine demandée.
- **Critères** : aucune soumission de formulaire sans passage par `trust` ; la validation est liée à la page et à la cible.

### 1.3 Reprise en main en direct
- **Approche** : un outil `browser_handoff` met le tour en attente (`tool.Pause`) ; l'interface d'administration affiche la page (images JPEG en SSE) et relaie clics et frappes ; « rendre la main » soumet un tour de reprise qui relit la page.
- **Dépendances** : 1.1.

### 1.4 Identifiants
- **Approche** : un coffre côté serveur ; le modèle demande « se connecter à tel site », le serveur saisit l'identifiant et le mot de passe dans la page sans que le modèle les voie ; masquage dans les captures et les résultats.
- **Dépendances** : 1.1 ; une ADR sur le stockage chiffré des secrets.

## 2. Persibot sur `kit` — M

- **Approche** : remplacer dans persibot la boucle, les canaux, les validations, la planification et les traces par `kit` ; garder ce qui est métier (fiches clients, ton, outils de l'entreprise, interface du patron). `examples/support` sert de squelette.
- **Critères** : les scénarios de persibot passent ; le code de persibot diminue nettement ; aucun ajout spécifique à persibot dans agentkit (s'il en faut un, il doit être générique et passer par une ADR).
- **Dépendances** : 0.2 (version publiée).

## 3. Fournisseurs et modèles

### 3.1 Autres API de modèles — S chacune
- **Approche** : un paquet `llm/<nom>` par API qui ne parle pas `/chat/completions` (format de messages propre, cache de prompt explicite, blocs de raisonnement). Contrat : `llm.Provider` + `llm.Streamer`, erreurs en `*llm.StatusError`, `Usage.CachedTokens` rempli.
- **Critères** : les tests de `agent` passent avec un faux serveur de l'API ; un essai réel par fournisseur.

### 3.2 Choix du modèle par tour — S
- **Approche** : un petit modèle pour les tours simples, un gros pour les tours difficiles (`agent.Config.Model` par tour, ou un `llm.Provider` routeur) ; le compteur de coûts mesure le gain.

### 3.3 Images et audio en entrée — M
- **Approche** : `llm.Message` porte des parties (texte, image, audio) ; les canaux remplissent déjà `channel.Media` ; transcription des messages vocaux par un `Transcriber` facultatif.

## 4. Stockage et déploiement

### 4.1 Postgres — M
- **Approche** : `*/postgres` pour chaque Store, validés par les mêmes contrats ; `FOR UPDATE SKIP LOCKED` pour `Queue.Claim`. Permet plusieurs processus sur une même file (les transitions sont déjà conditionnelles).

### 4.2 Plusieurs processus — M
- **Approche** : le runner réclame déjà les tours de façon atomique ; il faut une notion de bail (`lease`) pour qu'un processus mort libère ses tours (aujourd'hui, `Recover` au démarrage suppose un seul processus).
- **Dépendances** : 4.1.

### 4.3 Workspace distant — M
- **Approche** : `computer/remote` qui parle à un agent installé sur une autre machine (ou un service de bacs à sable) ; même contrat `computertest`.

## 5. Qualité des agents

| Proposition | Intérêt | Taille |
|---|---|---|
| **Évaluations** : un paquet `eval` qui rejoue des conversations (`trace.jsonl`) contre un modèle et vérifie des assertions (outil appelé, arguments, réponse) | mesurer l'effet d'un changement de prompt, de modèle ou de description d'outil | M |
| **Vérification des affirmations** : signaler une réponse qui affirme une action alors que l'outil a renvoyé une erreur (défaut mesuré des petits modèles) | moins de fausses confirmations au client | S |
| **Budget d'outils** : n'exposer que les groupes d'outils utiles au tour (`Expose`, `Toolset.Available`) selon la demande | les petits modèles se perdent au-delà d'une quinzaine d'outils | S |
| **Mémoire par extraction** : un `memory.Syncer` qui extrait automatiquement les faits d'un tour (au lieu d'attendre `memory_save`) | mémoire plus complète | S |

## 6. Ordre conseillé

1. Préalables 0.2 à 0.4 (0.1 fait).
2. Persibot sur `kit` (2) : la meilleure validation du framework sur un vrai métier.
3. Budget d'outils et vérification des affirmations (5) : petits chantiers, gain direct sur la qualité.
4. Navigateur (1.1 → 1.2 → 1.3 → 1.4).
5. Postgres et plusieurs processus (4.1, 4.2) quand la charge le justifie.
6. Le reste selon les besoins.

Chaque chantier se termine comme les phases précédentes : contrat ou tests, `-race`, essai réel, ADR dans `docs/decisions.md`, un commit par chantier.
