package delegate

import (
	"context"
	"fmt"
	"strings"

	"github.com/raucheacho/agentkit/tool"
)

type delegateIn struct {
	Tasks []Brief `json:"tasks,omitempty" desc:"tâches indépendantes, exécutées en parallèle"`
	// Une seule tâche peut aussi être passée directement.
	Goal    string `json:"goal,omitempty" desc:"une seule tâche (à la place de tasks)"`
	Context string `json:"context,omitempty" desc:"contexte de cette tâche unique"`
}

type startIn struct {
	Goal    string `json:"goal" desc:"ce que le sous-agent doit accomplir, formulé comme une consigne autonome"`
	Context string `json:"context,omitempty" desc:"tout ce qu'il doit savoir : il ne voit pas la conversation"`
}

type cancelIn struct {
	ID string `json:"id" desc:"identifiant de la tâche de fond"`
}

// Tools renvoie les outils de délégation, tous réservés à l'agent principal :
//   - delegate : confie des tâches à des sous-agents et attend leurs comptes rendus ;
//   - start_task, list_tasks, cancel_task : tâches de fond (si un Store est configuré).
func (d *Delegator) Tools() []tool.Tool {
	tools := []tool.Tool{d.delegateTool()}
	if d.cfg.Store != nil {
		tools = append(tools, d.startTool(), d.listTool(), d.cancelTool())
	}
	return tools
}

func (d *Delegator) delegateTool() tool.Tool {
	desc := fmt.Sprintf("Confie une ou plusieurs tâches indépendantes à des sous-agents qui travaillent en parallèle "+
		"(au plus %d) et renvoie leurs comptes rendus. Un sous-agent ne voit pas la conversation : donne-lui un "+
		"objectif clair et tout le contexte utile. Il ne peut ni parler à l'interlocuteur, ni faire d'action sensible.",
		d.cfg.MaxTasks)
	return tool.New("delegate", desc, func(ctx context.Context, in delegateIn) (tool.Result, error) {
		briefs := in.Tasks
		if strings.TrimSpace(in.Goal) != "" {
			briefs = append(briefs, Brief{Goal: in.Goal, Context: in.Context})
		}
		var clean []Brief
		for _, b := range briefs {
			if b.Goal = strings.TrimSpace(b.Goal); b.Goal != "" {
				clean = append(clean, b)
			}
		}
		if len(clean) == 0 {
			return tool.Text("Erreur : aucune tâche. Passe tasks (liste de {goal, context}) ou goal."), nil
		}
		if len(clean) > d.cfg.MaxTasks {
			return tool.Text(fmt.Sprintf("Erreur : %d tâches, au plus %d par appel.", len(clean), d.cfg.MaxTasks)), nil
		}
		parent, _ := tool.ConversationFrom(ctx)
		outs := d.DelegateAll(ctx, parent, clean)
		var sb strings.Builder
		for i, o := range outs {
			if len(outs) > 1 {
				fmt.Fprintf(&sb, "### Tâche %d : %s\n", i+1, o.Brief.Goal)
			}
			switch {
			case o.Err != nil:
				fmt.Fprintf(&sb, "Échec : %v\n", o.Err)
			case o.Reply == "":
				sb.WriteString("(compte rendu vide)\n")
			default:
				sb.WriteString(o.Reply + "\n")
			}
			if i < len(outs)-1 {
				sb.WriteString("\n")
			}
		}
		return tool.External(strings.TrimSpace(sb.String())), nil
	}, tool.MainOnly())
}

func (d *Delegator) startTool() tool.Tool {
	return tool.New("start_task", "Lance une tâche longue en arrière-plan, confiée à un sous-agent. Tu réponds tout de "+
		"suite ; son compte rendu te reviendra plus tard dans la conversation. Le sous-agent ne voit pas la conversation : "+
		"donne-lui un objectif clair et tout le contexte utile.",
		func(ctx context.Context, in startIn) (tool.Result, error) {
			goal := strings.TrimSpace(in.Goal)
			if goal == "" {
				return tool.Text("Erreur : goal est vide."), nil
			}
			parent, _ := tool.ConversationFrom(ctx)
			t, err := d.Start(ctx, parent, goal, in.Context)
			if err != nil {
				return tool.Text("Erreur : " + err.Error()), nil
			}
			return tool.Text(d.cfg.Texts.Started(t)), nil
		}, tool.MainOnly(), tool.WithEffect(tool.Write))
}

func (d *Delegator) listTool() tool.Tool {
	return tool.Func("list_tasks", "Liste les tâches de fond récentes de cette conversation et leur état.", nil,
		func(ctx context.Context, _ tool.Call) (tool.Result, error) {
			conv, _ := tool.ConversationFrom(ctx)
			list, err := d.cfg.Store.List(ctx, TaskFilter{Session: conv.Session, Limit: 10})
			if err != nil {
				return tool.Result{}, err
			}
			if len(list) == 0 {
				return tool.Text("Aucune tâche de fond."), nil
			}
			var sb strings.Builder
			for _, t := range list {
				fmt.Fprintf(&sb, "- %s [%s] %s", t.ID, t.Status, t.Goal)
				if t.Error != "" {
					fmt.Fprintf(&sb, " (%s)", t.Error)
				}
				sb.WriteString("\n")
			}
			return tool.Text(strings.TrimSpace(sb.String())), nil
		}, tool.MainOnly())
}

func (d *Delegator) cancelTool() tool.Tool {
	return tool.New("cancel_task", "Annule une tâche de fond en cours de cette conversation.",
		func(ctx context.Context, in cancelIn) (tool.Result, error) {
			conv, _ := tool.ConversationFrom(ctx)
			t, err := d.cfg.Store.Get(ctx, strings.TrimSpace(in.ID))
			if err != nil {
				return tool.Result{}, err
			}
			if t == nil || t.Conversation.Session != conv.Session {
				return tool.Text("Erreur : tâche inconnue dans cette conversation."), nil
			}
			ok, err := d.Cancel(ctx, t.ID)
			if err != nil {
				return tool.Result{}, err
			}
			if !ok {
				return tool.Text(fmt.Sprintf("La tâche %s était déjà terminée (%s).", t.ID, t.Status)), nil
			}
			return tool.Text("Tâche " + t.ID + " annulée."), nil
		}, tool.MainOnly(), tool.WithEffect(tool.Write))
}
