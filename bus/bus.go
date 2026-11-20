package bus

import (
	"time"

	"github.com/rs/zerolog/log"
)

// bufferSize is generous since WhatsApp is now the sole, potentially
// high-volume channel; a full buffer means messages would otherwise be
// dropped silently.
const bufferSize = 500

// InboundMessage représente un message entrant
type InboundMessage struct {
	Channel  string
	SenderID string
	ChatID   string
	Content  string
	// Note : contexte réservé au modèle pour ce seul tour (fiche du client,
	// mission en cours…). Placé devant le message à l'envoi, il n'est JAMAIS
	// enregistré dans l'historique : chaque tour relit une note à jour au lieu
	// d'accumuler des notes périmées.
	Note      string
	Media     []string
	Timestamp time.Time
	Metadata  map[string]interface{}
}

// OutboundMessage représente un message sortant
type OutboundMessage struct {
	Channel string
	ChatID  string
	Content string
	ReplyTo string
}

// SessionKey retourne la clé de session
func (m *InboundMessage) SessionKey() string {
	return m.Channel + ":" + m.ChatID
}

// Bus gère la communication entre channels et agent
type Bus struct {
	inbound  chan *InboundMessage
	outbound chan *OutboundMessage
}

// New crée un nouveau bus
func New() *Bus {
	return &Bus{
		inbound:  make(chan *InboundMessage, bufferSize),
		outbound: make(chan *OutboundMessage, bufferSize),
	}
}

// Inbound retourne le channel entrant
func (b *Bus) Inbound() <-chan *InboundMessage {
	return b.inbound
}

// Outbound retourne le channel sortant
func (b *Bus) Outbound() <-chan *OutboundMessage {
	return b.outbound
}

// PublishInbound publie un message entrant. Non-bloquant pour ne jamais
// bloquer le handler d'événements du channel WhatsApp ; si le buffer est
// plein, le message est perdu mais l'événement est loggé bruyamment (au lieu
// d'être ignoré en silence) pour rester visible en cas de surcharge.
func (b *Bus) PublishInbound(msg *InboundMessage) {
	select {
	case b.inbound <- msg:
	default:
		log.Warn().
			Str("channel", msg.Channel).
			Str("chat_id", msg.ChatID).
			Int("content_len", len(msg.Content)).
			Msg("bus: message entrant perdu, buffer plein")
	}
}

// PublishOutbound publie un message sortant. Mêmes garanties que PublishInbound.
func (b *Bus) PublishOutbound(msg *OutboundMessage) {
	select {
	case b.outbound <- msg:
	default:
		log.Warn().
			Str("channel", msg.Channel).
			Str("chat_id", msg.ChatID).
			Msg("bus: message sortant perdu, buffer plein")
	}
}
