# Décisions (ADR courts)

Chaque entrée : contexte → décision → conséquences. Les sources d'inspiration sont Nod (n8n) et Hermes (Nous Research), lus dans leur code ; les numéros de phase renvoient à `docs/FEUILLE_DE_ROUTE.md`.

## ADR-001 — Un framework autonome, pas une bibliothèque de persibot
- **Contexte** : agentkit v0.1 était une boucle ReAct avec des crochets ajoutés pour persibot ; persibot avait dû réécrire de son côté sous-agents, canaux, validations, planification, reprise en main, traces.
- **Décision** : agentkit devient la source de vérité pour construire **n'importe quel agent** (assistant personnel à la Nod/Hermes, agent de support à la persibot…). Aucun choix n'est orienté par un projet en particulier ; persibot sera une couche au-dessus.
- **Conséquences** : la v0.2 casse l'API de la v0.1. Base de départ : le commit `79a5dd5` (boucle durcie), seul acquis repris tel quel (`agent/loop.go`, `llm/history.go`).

## ADR-002 — Bibliothèque standard seulement
- **Décision** : le module n'importe que la bibliothèque standard (hors exemples et tests). Ce qui demande une dépendance tierce ou cgo vit dans un sous-module (`contrib/…`, son propre `go.mod`).
- **Pourquoi** : une application qui importe agentkit ne doit hériter ni d'un SDK de fournisseur, ni d'un pilote de base, ni d'une bibliothèque native.
- **Vérifié par** `internal/archtest` : aucun import hors stdlib, et aucun paquet du noyau n'importe un fournisseur, un stockage ou un canal concret. Les sous-modules (dossier avec `go.mod`) sont exclus du test.

## ADR-003 — `runner` et non `runtime`
- Le paquet qui exécute la file s'appelait `runtime` dans le plan ; renommé `runner` pour ne pas masquer le paquet `runtime` de la bibliothèque standard dans les fichiers qui ont besoin des deux.

## ADR-004 — Tout est un tour, durable, jamais rejoué
- **Contexte** : v0.1 passait les messages par un canal Go en mémoire (`bus`) : tout était perdu au redémarrage. Nod modélise tout comme des tours.
- **Décision** : messages, échéances, événements, comptes rendus de tâches et reprises sont des `turn.Turn` dans une file durable. Une session exécute un tour à la fois. Un tour resté `running` après un arrêt brutal passe en échec (`Queue.Recover`) sans être relancé.
- **Pourquoi pas de reprise automatique** : un tour a pu appeler des outils avant l'arrêt ; le rejouer peut doubler une commande ou un envoi. Mieux vaut un échec visible qu'un effet en double.
- **Conséquences** : seuls les appels au modèle sont réessayés (`llm.Retry`, et seulement l'ouverture d'un flux).

## ADR-005 — Prompt figé par session, note éphémère
- **Contexte** : le cache de prompt des fournisseurs ne sert que si le début de la conversation est identique d'un appel à l'autre (Hermes en fait un invariant).
- **Décision** : le prompt système est construit à la création de la session et enregistré ; ce qui change à chaque tour (date, fiche, souvenirs rappelés) passe par une note collée au message de l'utilisateur, envoyée au modèle mais jamais enregistrée. L'historique est en ajout seul.
- **Entorses** : la compression (ADR-011) ; `RefreshSystem` (recalcul explicite) ; `LiveSystem` (ADR-026), sur demande.

## ADR-006 — Contenu non fiable délimité
- **Décision** : un résultat `tool.External` (web, e-mail, MCP, compte rendu de sous-agent, événement) est entouré de `<untrusted_data source="…">`, une balise de fin injectée dans le contenu est neutralisée, et le prompt système interdit d'obéir à ce qu'il contient.
- **Limite assumée** : un modèle peut quand même se laisser influencer ; c'est pour ça que les actions qui engagent passent par la porte (ADR-007), qui ne dépend pas du modèle.

## ADR-007 — Une seule porte, liée aux arguments exacts
- **Contexte** : persibot avait des validations, Nod une vérification de confiance unique côté serveur.
- **Décision** : un seul middleware (`trust.Manager`) décide pour chaque outil (`Allow`, `Deny`, `Ask`) selon l'effet, l'outil, la source du tour et le canal. Une validation porte sur l'outil **et ses arguments canoniques** (empreinte `ArgsHash`) ; elle s'exécute **une seule fois** (transitions conditionnelles `Pending → Approved → Executing → Executed`), sans repasser par le modèle. Une action interrompue est marquée d'issue inconnue, jamais relancée. Un tour de reprise informe l'interlocuteur.
- **Détail** : le tour de reprise d'une approbation n'a **pas d'outils** (`turn.MetaTools = "none"`) : mesuré en réel, un petit modèle rappelait `ask_human` sans raison. Le tour qui suit une réponse à une question garde ses outils (l'agent peut avoir à agir).
- **Conséquences** : `kit` installe toujours la porte ; une application ne peut pas l'oublier.

## ADR-008 — Stockage SQL sans pilote imposé
- **Décision** : les stockages SQL prennent un `*sql.DB` ouvert par l'application ; les tables sont préfixées (`agentkit_` par défaut) pour cohabiter avec celles de l'application ; les dates sont des entiers (nanosecondes Unix). Les exemples utilisent `modernc.org/sqlite` (sans cgo), avec `journal_mode(WAL)` et `busy_timeout(5000)`.
- **Contrats** : chaque interface de stockage a sa suite de tests (`*test`), y compris la concurrence (10 transitions simultanées → une seule réussit).
- **Recherche** : FTS5 (`unicode61 remove_diacritics 2`) si le pilote le fournit, sinon `LIKE` ; les bases existantes sont migrées à l'ouverture (colonnes ajoutées, index plein texte reconstruit).

## ADR-009 — Sous-agents cantonnés
- **Décision** : un sous-agent ne voit ni les outils `tool.MainOnly` (déléguer, `ask_human`, planifier) ni les outils `Sensitive` ; un garde refuse ceux qui passeraient quand même, et transforme toute attente (`Wait`) en refus. Son compte rendu revient comme une donnée non fiable. Une tâche de fond coupée par un arrêt échoue, n'est pas relancée, et sa conversation en est informée.
- **Pourquoi `MainOnly` plutôt qu'une liste de noms** : un outil se déclare lui-même réservé ; un nouvel outil interactif n'a pas à être ajouté à une liste ailleurs.
- **Conséquences** : un sous-agent travaille dans la mémoire et le dossier de son parent (`tool.Conversation.Parent`), mais dans sa propre session (sinon il se bloquerait sur le verrou de session de son parent).

## ADR-010 — Mémoire cloisonnée par défaut
- **Décision** : notes, `history_search` et dossier de l'ordinateur sont rangés par conversation (`memory.Scope`, `computer.Key`). Partager entre conversations est un choix explicite (`kit.Memory.Scope`, `SharedHistory`).
- **Pourquoi** : Nod et Hermes n'ont qu'un propriétaire et partagent tout ; un agent de support qui partagerait la mémoire laisserait un client lire les données d'un autre.

## ADR-011 — Compression par archivage
- **Décision** : au-delà de `MaxMessages` messages ou `MaxChars` caractères, les messages anciens sont résumés par le modèle puis **archivés** (toujours consultables par `history_search`) ; les `Keep` derniers restent, coupés au début d'un tour. Le résumé suit le prompt système. Un échec du résumé laisse l'historique intact.
- **Mesuré en réel** : un modèle 8B recopiait un résultat d'outil au lieu de résumer ; la consigne est donc répétée dans le message utilisateur. `Keep` est borné à `MaxMessages/2`, sinon la compression se déclencherait à chaque tour.

## ADR-012 — Superviseur des commandes et marqueurs
- **Contexte** : une commande lancée par `docker exec` ne s'arrête pas quand on tue le client `docker` ; et un code de sortie 1 peut venir de la commande comme du démon Docker.
- **Décision** : dans le conteneur, `agentkit-run` lance la commande dans sa propre session (`setsid timeout -k 3 <délai>`), arrête tout le groupe à la fin, et encadre la sortie de deux marqueurs propres à la commande : `start` puis `exit:<code>`. Sans marqueur de début, la commande n'a pas tourné (erreur ordinaire) ; sans marqueur de fin, l'issue est inconnue (`computer.ErrUncertain`, jamais relancée). Une annulation appelle `agentkit-kill`.
- **Mesuré** : une première version avec un minuteur en sous-shell laissait un `sleep` orphelin par commande ; remplacée par `timeout` (test `TestNoLeftovers`). En local, un enfant né pendant la délivrance du signal échappait à l'arrêt du groupe sous macOS : le groupe est tué jusqu'à être vide.

## ADR-013 — Un utilisateur Linux par conversation
- **Contexte** : Nod n'a qu'un propriétaire ; ses dossiers de tâches ne sont pas des frontières.
- **Décision** : `computer/docker` exécute chaque conversation sous un utilisateur dérivé de son dossier (uid ≥ 100000), dossier en `700`. Seul root garde `CHOWN`/`FOWNER` pour créer les dossiers ; les commandes n'ont aucune capacité. Les lectures et écritures de fichiers passent par le conteneur avec cet utilisateur et vérifient le chemin réel (`realpath`).
- **Conséquences** : un client ne lit pas les fichiers d'un autre, même par `bash` (test `TestSessionsIsolated`) ; un seul conteneur suffit pour plusieurs clients.

## ADR-014 — Réseau « Internet public seulement »
- **Décision** : comme Nod, un conteneur voisin (seul à avoir `NET_ADMIN`) pose des règles `iptables` qui refusent les plages privées, le lien local et les métadonnées du cloud, coupe IPv6 et fixe des DNS publics ; le conteneur de travail partage son réseau. Autres modes : `none` (aucun réseau) et `open` (sans filtre).
- **Vérifié** par `TestPublicNetwork` : `example.com` répond, `192.168.1.1`, `10.0.0.1`, `169.254.169.254` et `host.docker.internal` sont bloqués.

## ADR-015 — Observation : sans outils, peut se taire
- **Décision** : le tour d'observation n'a **pas d'outils par défaut** (`observe.Config.WithTools`) : il est piloté par un contenu non fiable et n'a qu'à décider s'il prévient. Il peut répondre par un jeton de silence (`turn.MetaSilence`, `[SILENCE]` par défaut) que le runner ne livre pas. Pendant les heures calmes, il est reporté (`NotBefore`), sauf événement urgent. Les clés sont marquées vues **avant** la soumission (perdre plutôt que doubler).
- **Mesuré** : avec ses outils, un modèle 8B appelait `ask_human` à chaque événement.

## ADR-016 — MCP : l'effet est décidé par l'application
- **Contexte** : un serveur MCP peut annoter un outil `readOnlyHint` ; s'y fier laisserait un serveur malveillant contourner la porte.
- **Décision** : un outil MCP est `Write` par défaut, `Sensitive` s'il se déclare destructif (rendre plus strict est sans risque), `Read` seulement si l'application l'autorise (`TrustAnnotations`) ou le fixe (`Effects`). Les résultats sont non fiables. La liste des outils est relevée à la connexion et ne change plus (prompt stable).
- **Sans SDK** : JSON-RPC 2.0 en stdio (une ligne par message) et Streamable HTTP (réponse JSON ou SSE, `Mcp-Session-Id`, `MCP-Protocol-Version`).

## ADR-017 — Planification : au plus une fois
- **Décision** : une échéance est consommée par une mise à jour conditionnelle (`next_run = slot`) **avant** que le tour soit soumis : en cas d'arrêt entre les deux, elle est perdue plutôt que doublée. Une échéance en retard de plus de `Grace` (1 h) est sautée, pas rattrapée en rafale. Un tour planifié ne peut pas créer de planification ; l'agent ne peut pas descendre sous `MinInterval` (5 min) ni dépasser `MaxPerSession` (20).
- **Formats** : `every 2h`, cron à cinq champs (convention « jour du mois OU jour de la semaine »), `@daily`…, une date, `in 20m` (converti en date absolue). Le passage à l'heure d'hiver est testé.

## ADR-018 — Réparer la forme, jamais le fond
- **Contexte** : mesuré en réel, un modèle 8B envoyait `"quantity": "2"` ; la validation refusait, et le modèle inventait ensuite une commande « validée ». Il écrivait aussi ses appels en texte avec `True`/`False`/`None`.
- **Décision** : avant la validation, une chaîne qui représente **exactement** un entier, un nombre ou un booléen attendu par le schéma est convertie (`tool.Coerce`) ; les littéraux Python sont réparés dans les appels écrits en texte ; les noms d'outils sont résolus (`functions.x`, casse, tirets). Tout le reste revient au modèle comme une erreur à corriger.
- **Conséquences** : la porte voit les arguments convertis ; une validation reste liée à leur forme canonique.

## ADR-019 — `kit` : tout est facultatif sauf la porte
- **Décision** : `kit.New` assemble stockage (SQLite si une base est fournie, mémoire sinon), canaux, runner, porte, et seulement les briques demandées (mémoire, skills, compression, sous-agents, planification, observation, ordinateur, MCP). Les événements, la trace et les coûts sont branchés d'office. Chaque brique reste accessible et utilisable sans `kit`.
- **Administration** : `Kit.Admin(token)` exige un jeton (comparaison en temps constant) ; sans jeton configuré, tout est refusé. Le flux d'événements contient des extraits de conversations : il est réservé à l'administration.

## ADR-020 — `channel/whatsapp`, pas « wotp »
- L'adaptateur WhatsApp passe par une passerelle HTTP (webhook signé HMAC + API REST) ; il s'appelle `whatsapp` dans le code et la documentation, sans le nom de la passerelle.

## ADR-021 — Textes en français, remplaçables
- Tous les textes adressés au modèle ou à l'interlocuteur (consignes internes, refus, comptes rendus, tours de reprise) sont regroupés dans des `Texts` avec une valeur française par défaut (`DefaultTexts`). Une application les traduit ou les adapte sans toucher au code.

## ADR-022 — Recherche hybride en Go pur, VectixDB en option
- **Décision** : les notes combinent plein texte et vecteurs (fusion par rangs réciproques, k = 60) avec un Store SQLite en Go pur (vecteurs comparés en Go, adapté à quelques dizaines de milliers de notes par propriétaire). Pour les grands volumes, `contrib/vectix` range les notes dans VectixDB (HNSW + BM25 + filtres), sous-module à cause de cgo.
- **Mesuré** : sans seuil, la recherche par vecteurs remontait des notes sans rapport ; ajout de `MinSimilarity` (0,3 par défaut).

## ADR-023 — Branches et dépendants
- **Décision** : deux branches, `main` (stable, v0.1.2) et `develop` (v0.2) ; un commit par phase. Persibot dépend de la version publiée du commit `79a5dd5` (pseudo-version), plus d'un `go.work` vers un dossier local.
- **Suite** : `develop` fusionné dans `main` et tagué `v0.2.0` ; `contrib/vectix` tagué à part (`contrib/vectix/v0.1.0`) sur VectixDB publié.

## ADR-024 — Licence Apache-2.0
- **Décision** : Apache-2.0, comme VectixDB. Permissive (un framework n'a de valeur que s'il est adopté ; persibot et les autres applications restent libres de leur licence), avec une licence explicite sur les brevets et un `NOTICE` à conserver.
- **Écartées** : MIT (rien sur les brevets) ; AGPL (oblige tout service bâti dessus à publier son code, frein à l'adoption).
- **Détail** : `contrib/vectix` est un module à part ; il porte sa propre copie de `LICENSE` et `NOTICE`, car l'archive d'un module Go n'inclut pas les fichiers du module parent.

## ADR-025 — Tour à réponse fixe, annonce des décisions par l'application
- **Contexte** : persibot prévient le client d'une décision du patron par un message écrit par le module (« Votre rendez-vous du 12/12 à 10 h est confirmé »), fiable même avec un petit modèle. Avec agentkit, le tour de reprise faisait toujours rédiger l'annonce par le modèle.
- **Décision** : `turn.MetaReply` : le runner livre le texte tel quel et l'enregistre dans la session comme réponse de l'agent, sans appel au modèle, dans l'ordre de la session. `trust.Config.Notice` (et `kit.Trust.Notice`) rédige l'annonce d'une décision ; sans texte, le modèle la rédige comme avant.
- **Pourquoi un tour** : l'annonce reste durable, ordonnée après le tour en cours et visible dans l'historique (le modèle sait au tour suivant ce qui a été annoncé).

## ADR-026 — Prompt système vivant, sur demande
- **Contexte** : persibot change ses Instructions et ses modules à chaud depuis le tableau de bord ; avec un prompt figé (ADR-005), le changement n'atteignait pas les conversations ouvertes.
- **Décision** : `agent.Config.LiveSystem` (et `kit.Config.LiveSystem`) recalcule le prompt à chaque tour et ne le remplace **que s'il a changé** : le cache du fournisseur n'est perdu qu'au changement réel. Désactivé par défaut.
- **Condition** : les sections doivent être stables d'un tour à l'autre ; ce qui varie (heure, fiche, souvenirs) reste dans la note.
