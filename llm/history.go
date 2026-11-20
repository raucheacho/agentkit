package llm

// Règles de forme d'un historique de conversation (format OpenAI), vérifiées
// par les fournisseurs : un message "tool" répond toujours à un appel d'outil
// d'un message "assistant" qui le précède, et chaque appel reçoit sa réponse.
// Un historique qui viole ces règles est rejeté (400) : il faut donc le couper
// et le relire sans jamais les casser.

// MissingToolResult est la réponse posée à la place d'un résultat d'outil
// perdu (arrêt pendant l'exécution, ancien historique incomplet).
const MissingToolResult = "Erreur : résultat de l'outil indisponible (exécution interrompue)."

// TrimHistory garde au plus max messages, en coupant à une frontière de tour :
// l'historique conservé commence toujours par un message "user". Un tour n'est
// jamais coupé au milieu de ses appels d'outils.
func TrimHistory(messages []Message, max int) []Message {
	if max <= 0 || len(messages) <= max {
		return messages
	}
	start := len(messages) - max
	for start < len(messages) && messages[start].Role != "user" {
		start++
	}
	return messages[start:]
}

// SanitizeHistory rend un historique valide pour l'envoi au modèle :
//   - il commence par un message "user" (les messages qui précèdent sont retirés) ;
//   - un message "tool" sans appel correspondant est retiré ;
//   - un appel d'outil sans réponse reçoit MissingToolResult.
//
// L'historique d'origine n'est pas modifié.
func SanitizeHistory(messages []Message) []Message {
	start := 0
	for start < len(messages) && messages[start].Role != "user" {
		start++
	}
	out := make([]Message, 0, len(messages)-start)
	pending := map[string]bool{} // appels du dernier assistant encore sans réponse
	var order []string

	closePending := func() {
		for _, id := range order {
			if pending[id] {
				out = append(out, Message{Role: "tool", Content: MissingToolResult, ToolCallID: id})
			}
		}
		pending, order = map[string]bool{}, nil
	}

	for _, m := range messages[start:] {
		if m.Role == "tool" {
			if !pending[m.ToolCallID] {
				continue // réponse orpheline
			}
			pending[m.ToolCallID] = false
			out = append(out, m)
			continue
		}
		closePending()
		out = append(out, m)
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				pending[tc.ID] = true
				order = append(order, tc.ID)
			}
		}
	}
	closePending()
	return out
}
