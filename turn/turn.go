// Package turn définit le tour : l'unité de travail d'un agent.
//
// Tout est un tour : un message reçu, une tâche planifiée qui se déclenche, un
// événement observé, une reprise après validation. Chaque tour est
// enregistré dans une file durable avec sa source et l'endroit où répondre ;
// il survit donc à un redémarrage. Une session n'exécute qu'un tour à la
// fois ; un tour n'est jamais rejoué automatiquement.
package turn

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"
)

// Source dit ce qui a créé un tour.
type Source string

const (
	SourceUser     Source = "user"     // un message de l'interlocuteur
	SourceSchedule Source = "schedule" // une tâche planifiée
	SourceEvent    Source = "event"    // un événement observé (proactivité)
	SourceTask     Source = "task"     // une tâche de fond qui avance ou rend son résultat
	SourceResume   Source = "resume"   // une reprise après une attente (validation, réponse)
	SourceSystem   Source = "system"   // l'application elle-même
)

// Status est l'état d'un tour.
type Status string

const (
	Queued    Status = "queued"
	Running   Status = "running"
	Done      Status = "done"
	Waiting   Status = "waiting" // terminé, en attente de quelqu'un
	Failed    Status = "failed"
	Cancelled Status = "cancelled"
	Merged    Status = "merged" // absorbé par un tour en cours (steering)
	Held      Status = "held"   // reçu pendant une reprise en main : enregistré, sans réponse de l'agent
)

// MetaTools est la clé de Meta qui retire les outils d'un tour ("none").
const MetaTools = "tools"

// MetaSilence est la clé de Meta qui permet à un tour de ne rien dire : si
// la réponse contient sa valeur (par exemple "[SILENCE]"), elle n'est pas
// livrée. Sert aux tours proactifs (planification, observation) qui
// décident eux-mêmes s'il y a lieu de prévenir.
const MetaSilence = "silence"

// MetaReply est la clé de Meta d'un tour à réponse fixe : son texte est livré
// tel quel et enregistré dans la session comme réponse de l'agent, sans appel
// au modèle (annonce d'une décision rédigée par l'application…). Le tour garde
// l'ordre de la session : il passe après le tour en cours.
const MetaReply = "reply"

// MetaNoFailureReply ("true") : un échec de ce tour ne renvoie pas le message
// d'indisponibilité (runner.Config.FailureReply) ; sert au nouvel essai d'un
// tour dont l'interlocuteur l'a déjà reçu.
const MetaNoFailureReply = "no_failure_reply"

// Silent indique si reply est un silence pour t (voir MetaSilence).
func Silent(t *Turn, reply string) bool {
	tok := t.Meta[MetaSilence]
	return tok != "" && strings.Contains(reply, tok)
}

// Final indique si le statut est terminal.
func (s Status) Final() bool { return s != Queued && s != Running }

// Turn est un tour.
type Turn struct {
	ID      string
	Session string
	Source  Source
	// Channel et ChatID disent où répondre ; vides : pas de réponse à livrer
	// (tâche interne).
	Channel string
	ChatID  string
	Sender  string
	Content string
	// Note accompagne le message pour ce seul tour (voir agent.Input.Note).
	Note string
	// Meta porte des informations libres de l'application (identifiant du
	// message d'origine, référence d'une approbation…). MetaTools = "none"
	// exécute le tour sans outils.
	Meta map[string]string

	Status Status
	Reply  string
	Error  string
	// Undelivered : pourquoi la réponse n'a pas pu être livrée (canal en
	// panne…) ; vide si elle l'a été ou s'il n'y avait rien à livrer. Rempli
	// par le runner à la fin du tour, pour le crochet OnFinish ; non conservé.
	Undelivered string
	// MergedInto : tour qui a absorbé celui-ci (Status Merged).
	MergedInto string

	CreatedAt  time.Time
	NotBefore  time.Time // ne pas démarrer avant (heures calmes, report)
	StartedAt  time.Time
	FinishedAt time.Time
}

// Filter sélectionne des tours.
type Filter struct {
	Session string
	Status  []Status
	Limit   int // 0 : 100
}

// Queue est la file durable des tours.
type Queue interface {
	// Enqueue ajoute un tour (ID et CreatedAt remplis s'ils sont vides).
	Enqueue(ctx context.Context, t *Turn) error
	// Claim passe en Running, au plus, max tours prêts (Queued, NotBefore
	// atteint) : le plus ancien de chaque session sans tour en cours. Les
	// sessions retenues sont toutes différentes.
	Claim(ctx context.Context, now time.Time, max int) ([]*Turn, error)
	// TakeQueued retire les tours user en attente d'une session sur un canal,
	// absorbés par le tour into, et les renvoie dans l'ordre d'arrivée.
	TakeQueued(ctx context.Context, session, channel, into string) ([]*Turn, error)
	// Finish termine un tour en cours.
	Finish(ctx context.Context, id string, status Status, reply, errMsg string) error
	// Cancel annule un tour encore en attente ; false s'il n'était plus en attente.
	Cancel(ctx context.Context, id string) (bool, error)
	// Get renvoie un tour ; nil s'il n'existe pas.
	Get(ctx context.Context, id string) (*Turn, error)
	// List renvoie des tours, du plus récent au plus ancien.
	List(ctx context.Context, f Filter) ([]*Turn, error)
	// Recover passe en Failed les tours restés Running après un arrêt
	// brutal : ils ne sont jamais rejoués, car leurs effets sont inconnus.
	Recover(ctx context.Context) (int, error)
}

// NewID renvoie un identifiant aléatoire.
func NewID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func prepare(t *Turn, now time.Time) {
	if t.ID == "" {
		t.ID = NewID()
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	if t.Source == "" {
		t.Source = SourceUser
	}
	if t.Session == "" && (t.Channel != "" || t.ChatID != "") {
		t.Session = t.Channel + ":" + t.ChatID
	}
	t.Status = Queued
}
