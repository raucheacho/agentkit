// Package tool définit les outils qu'un agent peut appeler.
//
// Un outil décrit ce qu'il fait (Spec : nom, description, JSON Schema, effet)
// et l'exécute (Run). Son effet dit à la porte de confiance ce qu'il risque :
// lire, modifier, ou engager l'interlocuteur. Son résultat dit si son contenu
// vient de l'extérieur (non fiable) ou si le tour doit attendre quelqu'un.
package tool

import (
	"context"
	"encoding/json"
)

// Effect classe ce qu'un outil fait au monde.
type Effect int

const (
	// Read : aucun effet de bord (recherche, lecture, calcul).
	Read Effect = iota
	// Write : modifie quelque chose que la demande couvre (brouillon, fichier de travail).
	Write
	// Sensitive : engage l'interlocuteur ou un tiers (réserver, payer, envoyer,
	// supprimer). Par défaut, la porte de confiance demande une validation.
	Sensitive
)

func (e Effect) String() string {
	switch e {
	case Read:
		return "read"
	case Write:
		return "write"
	case Sensitive:
		return "sensitive"
	}
	return "unknown"
}

// Spec décrit un outil.
type Spec struct {
	Name        string
	Description string
	// Parameters est un JSON Schema d'objet, transmis tel quel au modèle.
	Parameters json.RawMessage
	Effect     Effect
	// MainOnly réserve l'outil à l'agent principal : il n'est jamais montré à
	// un sous-agent (déléguer, poser une question à un humain…).
	MainOnly bool
}

// Call est un appel d'outil prêt à exécuter : nom résolu, arguments décodés.
type Call struct {
	ID   string
	Name string
	Args map[string]any
	// Raw garde les arguments JSON d'origine (pour lier une approbation aux
	// arguments exacts, ou décoder dans une struct).
	Raw json.RawMessage
	// Spec de l'outil appelé.
	Spec Spec
}

// Decode décode les arguments dans v.
func (c Call) Decode(v any) error {
	raw := c.Raw
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	return json.Unmarshal(raw, v)
}

// Wait suspend le tour : l'outil a besoin de quelqu'un (validation, réponse à
// une question, identifiants). Le tour se termine en attente ; il reprendra
// dans un nouveau tour quand la réponse arrivera.
type Wait struct {
	// Kind : "approval", "question", "credential"… libre pour l'application.
	Kind string
	// Ref identifie ce qui est attendu (identifiant de la demande).
	Ref string
	// Reply est le message à envoyer tout de suite à l'interlocuteur
	// (« Je transmets votre demande, je reviens vers vous. »). Vide : le
	// modèle formule lui-même la réponse.
	Reply string
}

// Result est le résultat d'un outil.
type Result struct {
	// Content est ce que le modèle lit.
	Content string
	// Untrusted : le contenu vient de l'extérieur (page web, e-mail, fichier,
	// message d'un tiers). Il est présenté au modèle comme une donnée, jamais
	// comme une instruction de l'interlocuteur.
	Untrusted bool
	// Wait suspend le tour après cet outil.
	Wait *Wait
}

// Text renvoie un résultat de confiance.
func Text(s string) Result { return Result{Content: s} }

// External renvoie un résultat au contenu non fiable.
func External(s string) Result { return Result{Content: s, Untrusted: true} }

// Pause renvoie un résultat qui suspend le tour.
func Pause(content string, w Wait) Result { return Result{Content: content, Wait: &w} }

// Tool est un outil.
type Tool interface {
	Spec() Spec
	Run(ctx context.Context, call Call) (Result, error)
}
