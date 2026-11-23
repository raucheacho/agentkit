// Package channel relie un agent aux messageries : WhatsApp, Telegram, web,
// e-mail, Slack…
//
// Un adaptateur ne fait que traduire : il reçoit les messages de sa
// plateforme et les remet à la passerelle (Sink), et il envoie ce qu'on lui
// demande (Send). Les capacités propres à certaines plateformes (indicateur
// de frappe, longueur maximale) sont des interfaces facultatives. La
// passerelle s'occupe du reste : filtrage, anti-doublon, création des tours,
// livraison, reprise en main.
package channel

import (
	"context"
	"time"
)

// Media est une pièce jointe. Ref est un identifiant propre à la plateforme
// (identifiant de fichier, chemin local) ; URL n'est renseignée que si elle
// ne contient aucun secret.
type Media struct {
	Kind string // image, audio, video, file, location
	Ref  string
	URL  string
	MIME string
	Name string
}

// Message est un message reçu.
type Message struct {
	Channel    string
	ChatID     string
	Sender     string
	SenderName string
	Text       string
	Media      []Media
	// ID est l'identifiant du message sur la plateforme (anti-doublon).
	ID    string
	Group bool
	At    time.Time
	Meta  map[string]string
}

// Outgoing est un message à envoyer.
type Outgoing struct {
	ChatID  string
	Text    string
	ReplyTo string
}

// Sink reçoit les messages d'un adaptateur.
type Sink func(ctx context.Context, m Message)

// Adapter est un canal de messagerie.
type Adapter interface {
	// Name identifie le canal (« telegram », « whatsapp »…) ; c'est la valeur
	// de Message.Channel et de turn.Turn.Channel.
	Name() string
	// Run reçoit les messages et les remet à sink jusqu'à l'annulation de ctx.
	// Un adaptateur à webhook se contente d'attendre ctx.
	Run(ctx context.Context, sink Sink) error
	// Send envoie un message.
	Send(ctx context.Context, out Outgoing) error
}

// Typer est implémenté par les canaux qui savent afficher « en train d'écrire ».
type Typer interface {
	Typing(ctx context.Context, chatID string) error
}

// Limiter est implémenté par les canaux qui limitent la longueur d'un
// message : la passerelle découpe les réponses trop longues.
type Limiter interface {
	MaxLength() int
}
