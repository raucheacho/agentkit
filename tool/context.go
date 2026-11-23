package tool

import "context"

// Conversation identifie à qui et pour quoi un outil agit. Elle voyage dans le
// context.Context de chaque appel : un outil ne peut agir que pour la
// conversation qui l'a appelé, même si plusieurs conversations s'exécutent en
// parallèle sur le même registre.
type Conversation struct {
	// Session est la clé de la session (historique, verrou).
	Session string
	// Channel et ChatID disent où répondre (« telegram », identifiant du chat).
	Channel string
	ChatID  string
	// Sender identifie l'auteur du message (peut différer de ChatID dans un groupe).
	Sender string
	// Source dit ce qui a déclenché le tour : "user", "schedule", "event",
	// "task", "resume", "delegate"…
	Source string
	// Turn identifie le tour en cours.
	Turn string
	// Parent : pour un sous-agent, la session de l'agent qui lui a confié la
	// tâche (les outils qui rangent des données par session s'en servent).
	Parent string
}

type convKey struct{}

// WithConversation attache c à ctx.
func WithConversation(ctx context.Context, c Conversation) context.Context {
	return context.WithValue(ctx, convKey{}, c)
}

// ConversationFrom renvoie la conversation attachée à ctx.
func ConversationFrom(ctx context.Context) (Conversation, bool) {
	c, ok := ctx.Value(convKey{}).(Conversation)
	return c, ok
}
