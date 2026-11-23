package schedule

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/raucheacho/agentkit/tool"
	"github.com/raucheacho/agentkit/turn"
)

type createIn struct {
	When string `json:"when" desc:"quand : « every 2h », « every 1d », cron « 0 9 * * 1-5 » (lun-ven 9 h), date « 2026-12-01 09:00 » ou délai « in 20m »"`
	Task string `json:"task" desc:"consigne complète à exécuter à chaque échéance, comprise sans le contexte de la conversation"`
	Name string `json:"name,omitempty" desc:"nom court"`
	// OnlyIfRelevant : surveillance (« préviens-moi si… ») plutôt que rappel.
	OnlyIfRelevant bool `json:"only_if_relevant,omitempty" desc:"vrai pour une surveillance : ne prévenir que s'il y a quelque chose d'utile"`
}

type idIn struct {
	ID string `json:"id" desc:"identifiant de la tâche planifiée"`
}

// Tools renvoie schedule_create, schedule_list et schedule_cancel, réservés à
// l'agent principal et refusés pendant un tour planifié.
func (s *Scheduler) Tools() []tool.Tool {
	return []tool.Tool{
		tool.New("schedule_create", "Planifie une tâche récurrente ou ponctuelle pour cette conversation (rappel, "+
			"résumé régulier, surveillance). À chaque échéance tu recevras la consigne et ta réponse sera envoyée à ton interlocuteur.",
			func(ctx context.Context, in createIn) (tool.Result, error) {
				conv, _ := tool.ConversationFrom(ctx)
				if conv.Source == string(turn.SourceSchedule) {
					return tool.Text("Refusé : une tâche planifiée ne peut pas en planifier d'autres."), nil
				}
				list, err := s.cfg.Store.List(ctx, Filter{Session: conv.Session, Limit: s.cfg.MaxPerSession + 1})
				if err != nil {
					return tool.Result{}, err
				}
				if len(list) >= s.cfg.MaxPerSession {
					return tool.Text(fmt.Sprintf("Refusé : déjà %d tâches planifiées dans cette conversation ; annules-en une d'abord.", len(list))), nil
				}
				j, err := s.add(ctx, conv, in.Name, in.When, in.Task, in.OnlyIfRelevant, s.cfg.MinInterval)
				if err != nil {
					return tool.Text("Erreur : " + err.Error()), nil
				}
				return tool.Text(fmt.Sprintf("Tâche %s planifiée (%s). Prochaine exécution : %s.", j.ID, j.Spec, s.when(j.NextRun))), nil
			}, tool.MainOnly(), tool.WithEffect(tool.Write)),
		tool.Func("schedule_list", "Liste les tâches planifiées de cette conversation.", nil,
			func(ctx context.Context, _ tool.Call) (tool.Result, error) {
				conv, _ := tool.ConversationFrom(ctx)
				list, err := s.cfg.Store.List(ctx, Filter{Session: conv.Session})
				if err != nil {
					return tool.Result{}, err
				}
				if len(list) == 0 {
					return tool.Text("Aucune tâche planifiée."), nil
				}
				var sb strings.Builder
				for _, j := range list {
					state := "prochaine : " + s.when(j.NextRun)
					if !j.Enabled {
						state = "inactive"
					}
					fmt.Fprintf(&sb, "- %s « %s » (%s, %s) : %s\n", j.ID, j.Name, j.Spec, state, clip(j.Task, 120))
				}
				return tool.Text(strings.TrimSpace(sb.String())), nil
			}, tool.MainOnly()),
		tool.New("schedule_cancel", "Supprime une tâche planifiée de cette conversation.",
			func(ctx context.Context, in idIn) (tool.Result, error) {
				conv, _ := tool.ConversationFrom(ctx)
				j, err := s.cfg.Store.Get(ctx, strings.TrimSpace(in.ID))
				if err != nil {
					return tool.Result{}, err
				}
				if j == nil || j.Conversation.Session != conv.Session {
					return tool.Text("Erreur : tâche inconnue dans cette conversation."), nil
				}
				if _, err := s.cfg.Store.Delete(ctx, j.ID); err != nil {
					return tool.Result{}, err
				}
				return tool.Text("Tâche " + j.ID + " supprimée."), nil
			}, tool.MainOnly(), tool.WithEffect(tool.Write)),
	}
}

func (s *Scheduler) when(t time.Time) string {
	if t.IsZero() {
		return "aucune"
	}
	return t.In(s.cfg.Location).Format("2006-01-02 15:04 MST")
}
