// Package trust est la porte de confiance d'un agent : une seule vérification,
// placée devant chaque outil, décide si l'appel s'exécute, est refusé ou
// attend la validation d'un humain.
//
// La décision appartient au code, pas au modèle : les consignes du prompt
// s'ajoutent à la porte, elles ne la remplacent pas. Une validation est liée à
// l'action exacte (outil et arguments) ; une action validée s'exécute une
// seule fois, hors du tour qui l'a demandée, puis un tour de reprise informe
// l'interlocuteur.
package trust

import (
	"context"
	"slices"

	"github.com/raucheacho/agentkit/tool"
)

// Decision est le verdict de la porte.
type Decision int

const (
	Allow Decision = iota // l'outil s'exécute
	Deny                  // l'outil est refusé ; le modèle reçoit la raison
	Ask                   // l'outil attend la validation d'un humain
)

func (d Decision) String() string {
	switch d {
	case Allow:
		return "allow"
	case Deny:
		return "deny"
	case Ask:
		return "ask"
	}
	return "unknown"
}

// Verdict est une décision motivée.
type Verdict struct {
	Decision Decision
	Reason   string
}

// Action est ce que la porte examine : un appel d'outil résolu et validé, et
// la conversation pour laquelle il agit.
type Action struct {
	Call         tool.Call
	Conversation tool.Conversation
}

// Policy décide d'une action.
type Policy interface {
	Check(ctx context.Context, a Action) Verdict
}

// PolicyFunc adapte une fonction en Policy.
type PolicyFunc func(ctx context.Context, a Action) Verdict

func (f PolicyFunc) Check(ctx context.Context, a Action) Verdict { return f(ctx, a) }

// ByEffect décide selon l'effet de l'outil ; un effet absent de la table est autorisé.
func ByEffect(table map[tool.Effect]Decision) Policy {
	return PolicyFunc(func(_ context.Context, a Action) Verdict {
		d, ok := table[a.Call.Spec.Effect]
		if !ok {
			return Verdict{Decision: Allow}
		}
		return Verdict{Decision: d, Reason: "action " + a.Call.Spec.Effect.String()}
	})
}

// Default autorise la lecture et l'écriture, et demande une validation pour
// toute action sensible.
func Default() Policy {
	return ByEffect(map[tool.Effect]Decision{tool.Read: Allow, tool.Write: Allow, tool.Sensitive: Ask})
}

// Rule est une règle : elle s'applique quand tous ses critères renseignés
// correspondent (un critère vide correspond à tout).
type Rule struct {
	Tools    []string      // noms d'outils
	Effects  []tool.Effect // effets
	Sources  []string      // sources du tour : user, schedule, event…
	Channels []string      // canaux : telegram, web…
	Decision Decision
	Reason   string
}

func (r Rule) matches(a Action) bool {
	return (len(r.Tools) == 0 || slices.Contains(r.Tools, a.Call.Name)) &&
		(len(r.Effects) == 0 || slices.Contains(r.Effects, a.Call.Spec.Effect)) &&
		(len(r.Sources) == 0 || slices.Contains(r.Sources, a.Conversation.Source)) &&
		(len(r.Channels) == 0 || slices.Contains(r.Channels, a.Conversation.Channel))
}

// Rules applique la première règle qui correspond, sinon fallback (nil : Default).
//
//	trust.Rules(trust.Default(),
//		trust.Rule{Tools: []string{"shell"}, Channels: []string{"whatsapp"}, Decision: trust.Deny, Reason: "interdit aux clients"},
//		trust.Rule{Effects: []tool.Effect{tool.Sensitive}, Sources: []string{"schedule"}, Decision: trust.Deny, Reason: "personne pour valider"},
//	)
func Rules(fallback Policy, rules ...Rule) Policy {
	if fallback == nil {
		fallback = Default()
	}
	return PolicyFunc(func(ctx context.Context, a Action) Verdict {
		for _, r := range rules {
			if r.matches(a) {
				return Verdict{Decision: r.Decision, Reason: r.Reason}
			}
		}
		return fallback.Check(ctx, a)
	})
}
