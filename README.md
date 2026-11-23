# agentkit

Framework Go pour construire **n'importe quel agent** — assistant personnel proactif, agent de support multi-clients, agent qui travaille dans son propre ordinateur — sans réécrire la boucle, les canaux, la mémoire ni la sécurité. Bibliothèque standard seulement ; tout l'état tient dans une base SQLite.

- **La boucle** (`agent`) : outils typés, prompt figé pour le cache du fournisseur, contenu non fiable délimité, réparation des appels des petits modèles, compression de l'historique.
- **Le runtime** (`turn`, `runner`, `channel`) : tout est un tour durable ; web, Telegram, WhatsApp ; reprise en main par un humain.
- **La confiance** (`trust`) : une seule porte ; une action sensible est validée par un humain, liée à ses arguments exacts, exécutée une seule fois.
- **Les capacités** : mémoire et recherche hybride (`memory`), skills (`skills`), sous-agents et tâches de fond (`delegate`), planification (`schedule`), proactivité (`observe`), ordinateur isolé (`computer`), MCP (`mcp`).
- **L'observabilité** (`event`) : trace, tableau de bord en direct, coûts par session et par modèle.
- **L'assemblage** (`kit`) : tout ça en un appel.

Documentation :
- **`docs/APPRENDRE.md`** : comprendre le projet (notions, visite guidée d'une commande dans le code, exercices, débogage, glossaire) ;
- **`docs/DEVELOPPEMENT.md`** : reprendre le développement (architecture, tests, règles, recettes) ;
- `docs/reference.md` (packages), `docs/configuration.md` (`kit.Config`, administration, variables), `docs/compatibilite.md` (modèles, canaux, systèmes) ;
- `docs/decisions.md` (ADR) et `docs/FEUILLE_DE_ROUTE.md` (la suite).

## Démarrage

Prérequis : Go 1.26. Pour un essai réel, un modèle : une clé d'API compatible OpenAI, ou [Ollama](https://ollama.com) en local.

```sh
go get github.com/raucheacho/agentkit@develop
make test      # tous les tests, avec -race
```

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

Un outil :

```go
type orderIn struct {
	Ref      string `json:"ref" desc:"référence du produit"`
	Quantity int    `json:"quantity" desc:"quantité"`
}

createOrder := tool.New("create_order", "Passe une commande pour le client.",
	func(ctx context.Context, in orderIn) (tool.Result, error) {
		c, _ := tool.ConversationFrom(ctx) // pour qui l'outil agit
		return tool.Text("Commande créée pour " + c.ChatID), nil
	}, tool.WithEffect(tool.Sensitive)) // validé par un humain avant exécution
```

## Exemples

| Exemple | Style | Ce qu'il montre |
|---|---|---|
| `examples/cli` | minimal | la boucle seule, dans un terminal, sans `kit` |
| `examples/assistant` | Nod, Hermes | un propriétaire : mémoire partagée entre ses conversations, skills écrits par l'agent, tâches de fond, rappels planifiés, veille RSS, ordinateur (Docker ou local), MCP |
| `examples/support` | persibot | de nombreux clients : mémoire par client, catalogue et commandes, commande validée par le responsable, questions au responsable, reprise en main |

```sh
AGENTKIT_MODEL=llama3.1:8b ADMIN_TOKEN=secret go run ./examples/support
curl -X POST localhost:8080/web/messages -d '{"chat_id":"c1","text":"Avez-vous des bougies ?"}'
curl -H "Authorization: Bearer secret" localhost:8080/admin/requests
```

Toutes les variables : `docs/configuration.md`.

## Principes

Chacun est protégé par des tests ; le pourquoi est dans `docs/decisions.md`.

1. **Le prompt système est figé pour la session** ; ce qui change à chaque tour passe par une note éphémère. Seule entorse : la compression, qui archive sans effacer.
2. **Tout est un tour, durable, jamais rejoué** : une session à la fois, un tour interrompu échoue plutôt que de doubler une action.
3. **Une seule porte décide, pas le modèle** : validation liée aux arguments exacts, exécution unique, l'interlocuteur est prévenu.
4. **Le contenu extérieur est une donnée** (web, e-mails, MCP, sous-agents, événements), jamais une consigne.
5. **On répare la forme, jamais le fond** : noms d'outils, types envoyés en texte ; le reste revient au modèle comme une erreur.
6. **La mémoire est cloisonnée par conversation** ; la partager est un choix explicite.
7. **Un sous-agent ne fait qu'exécuter** : ni délégation, ni question à un humain, ni action sensible.
8. **La proactivité ne dérange pas pour rien** : échéance au plus une fois, événement dédoublonné, silence possible, heures calmes.
9. **L'ordinateur de l'agent n'a rien à voler** : aucun secret, un utilisateur Linux par conversation, Internet public seulement, aucun processus laissé derrière.
10. **Un serveur MCP est un tiers** : l'application fixe l'effet de ses outils.
11. **Bibliothèque standard seulement** ; le noyau ne choisit ni fournisseur, ni stockage, ni canal ; ce qui demande cgo vit dans `contrib/`.

## Branches

`main` : version stable publiée (v0.1.2). `develop` : la v0.2 en cours, qui change l'API de la v0.1.
