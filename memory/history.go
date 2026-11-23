package memory

import (
	"context"
	"fmt"
	"strings"

	"github.com/raucheacho/agentkit/session"
	"github.com/raucheacho/agentkit/tool"
)

// HistoryConfig configure l'outil de recherche dans l'historique.
type HistoryConfig struct {
	Searcher session.Searcher
	// Keys renvoie les sessions consultables depuis une conversation (nil :
	// la sienne seulement, via Scope). Renvoyer nil autorise toutes les
	// sessions : à réserver à un assistant personnel, jamais à un agent qui
	// parle à plusieurs personnes.
	Keys func(conv tool.Conversation) []string
	// Limit : résultats renvoyés (défaut 8) ; MaxChars par résultat (défaut 400).
	Limit    int
	MaxChars int
}

type historyIn struct {
	Query string `json:"query" desc:"mots-clés à retrouver dans les échanges passés"`
}

// HistoryTool renvoie l'outil history_search : retrouver ce qui a été dit
// dans les échanges passés, y compris ceux résumés par la compression. Les
// extraits sont des données non fiables.
func HistoryTool(cfg HistoryConfig) tool.Tool {
	if cfg.Keys == nil {
		cfg.Keys = func(conv tool.Conversation) []string { return []string{Scope(conv)} }
	}
	if cfg.Limit <= 0 {
		cfg.Limit = 8
	}
	if cfg.MaxChars <= 0 {
		cfg.MaxChars = 400
	}
	return tool.New("history_search", "Cherche dans les échanges passés de cette conversation (y compris les plus anciens, "+
		"qui ne sont plus dans ton contexte). Utilise-le avant de dire que tu ne sais pas ce qui a été dit.",
		func(ctx context.Context, in historyIn) (tool.Result, error) {
			q := strings.TrimSpace(in.Query)
			if len(session.Terms(q)) == 0 {
				return tool.Text("Erreur : donne au moins un mot-clé de trois lettres ou plus."), nil
			}
			conv, _ := tool.ConversationFrom(ctx)
			hits, err := cfg.Searcher.Search(ctx, session.Query{Text: q, Keys: cfg.Keys(conv), Limit: cfg.Limit})
			if err != nil {
				return tool.Result{}, err
			}
			if len(hits) == 0 {
				return tool.Text("Aucun échange passé ne correspond."), nil
			}
			var sb strings.Builder
			for _, h := range hits {
				who := "interlocuteur"
				if h.Role == "assistant" {
					who = "toi"
				}
				c := []rune(strings.TrimSpace(h.Content))
				if len(c) > cfg.MaxChars {
					c = append(c[:cfg.MaxChars], '…')
				}
				fmt.Fprintf(&sb, "- [%s, %s] %s\n", h.At.Format("2006-01-02 15:04"), who, string(c))
			}
			return tool.External(strings.TrimSpace(sb.String())), nil
		})
}
