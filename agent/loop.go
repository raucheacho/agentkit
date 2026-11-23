package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/raucheacho/agentkit/llm"
	"github.com/raucheacho/agentkit/session"
	"github.com/raucheacho/agentkit/tool"
)

// notExecuted remplace le résultat des appels qui suivent, dans la même
// réponse du modèle, un outil qui a suspendu le tour.
const notExecuted = "Non exécuté : le tour est suspendu en attente d'une réponse."

// turn : les messages d'un tour. api part au modèle (prompt système,
// historique, note éphémère) ; saved sera ajouté à l'historique.
type turn struct {
	in    Input
	conv  tool.Conversation
	api   []llm.Message
	saved []llm.Message
	usage llm.Usage
	steps int
}

func (t *turn) add(m llm.Message) {
	t.api = append(t.api, m)
	t.saved = append(t.saved, m)
}

// Run exécute un tour. Les tours d'une même session sont sérialisés ; ceux de
// sessions différentes s'exécutent en parallèle.
//
// Même en cas d'erreur ou d'annulation (Stop), ce que le tour a réellement
// fait (appels d'outils et résultats) est enregistré, pour que le tour
// suivant le sache. Un tour n'est jamais rejoué automatiquement.
func (a *Agent) Run(ctx context.Context, in Input) (*Outcome, error) {
	conv := in.Conversation
	if conv.Session == "" {
		if conv.Channel == "" && conv.ChatID == "" {
			return nil, errors.New("agent : Input.Conversation.Session est obligatoire")
		}
		conv.Session = conv.Channel + ":" + conv.ChatID
	}
	if conv.Source == "" {
		conv.Source = "user"
	}
	unlock := a.locks.Lock(conv.Session)
	defer unlock()
	ctx = tool.WithConversation(ctx, conv)

	t, err := a.open(ctx, conv, in)
	if err != nil {
		return nil, err
	}
	out, runErr := a.loop(ctx, t)

	if len(t.saved) > 0 {
		if err := a.cfg.Sessions.Append(context.WithoutCancel(ctx), conv.Session, t.saved...); err != nil {
			a.log.Warn("agent : enregistrement du tour impossible", "session", conv.Session, "err", err)
			if runErr == nil {
				runErr = fmt.Errorf("agent : enregistrement du tour : %w", err)
			}
		}
	}
	if out != nil {
		out.Saved, out.Usage, out.Steps = t.saved, t.usage, t.steps
	}
	if runErr == nil && out != nil && a.cfg.AfterTurn != nil {
		a.cfg.AfterTurn(context.WithoutCancel(ctx), conv, out)
	}
	a.emit(ctx, Event{Kind: EventTurn, Conversation: conv, Step: t.steps, Usage: t.usage, Outcome: out, Err: runErr})
	if runErr != nil {
		return nil, runErr
	}
	return out, nil
}

// open charge la session, fige son prompt système si elle est neuve, et
// prépare le message d'ouverture avec la note éphémère.
func (a *Agent) open(ctx context.Context, conv tool.Conversation, in Input) (*turn, error) {
	limit := a.cfg.HistoryLimit
	if a.cfg.Compaction != nil {
		limit = 0 // la compression borne l'historique actif
	}
	sess, err := a.cfg.Sessions.Load(ctx, conv.Session, limit)
	if err != nil {
		return nil, fmt.Errorf("agent : lecture de la session %s : %w", conv.Session, err)
	}
	system := sess.System
	if system == "" {
		if system, err = a.buildSystem(ctx, conv); err != nil {
			return nil, err
		}
		if err := a.cfg.Sessions.SetSystem(ctx, conv.Session, system); err != nil {
			return nil, fmt.Errorf("agent : enregistrement du prompt système : %w", err)
		}
	}

	t := &turn{in: in, conv: conv}
	history, summary := sess.Messages, sess.Summary
	if a.cfg.Compaction != nil {
		history, summary = a.compact(ctx, t, history, summary)
	}
	if summary != "" {
		system += "\n\n" + a.cfg.Texts.SummaryHeading + "\n\n" + summary
	}
	t.api = append(t.api, llm.Message{Role: llm.RoleSystem, Content: system})
	t.api = append(t.api, llm.SanitizeHistory(llm.TrimHistory(history, max(a.cfg.HistoryLimit, len(history))))...)

	opening := llm.Message{Role: llm.RoleUser, Content: in.Content}
	t.saved = append(t.saved, opening)
	extra := in.Note
	if a.cfg.Recall != nil {
		rec, err := a.cfg.Recall(ctx, conv, in.Content)
		if err != nil {
			a.log.Warn("agent : rappel de mémoire impossible", "session", conv.Session, "err", err)
		} else if rec = strings.TrimSpace(rec); rec != "" {
			extra = strings.TrimSpace(extra + "\n\n" + rec)
		}
	}
	if note := a.note(extra); note != "" {
		opening.Content = note + "\n\n" + in.Content
	}
	t.api = append(t.api, opening)
	return t, nil
}

// compact résume puis archive le début de l'historique s'il dépasse les
// seuils. Un échec laisse l'historique intact (il sera retenté au tour suivant).
func (a *Agent) compact(ctx context.Context, t *turn, msgs []llm.Message, summary string) ([]llm.Message, string) {
	c := a.cfg.Compaction
	chars := 0
	for _, m := range msgs {
		chars += len(m.Content) + len(m.Reasoning)
		for _, tc := range m.ToolCalls {
			chars += len(tc.Arguments)
		}
	}
	if len(msgs) <= c.MaxMessages && chars <= c.MaxChars {
		return msgs, summary
	}
	store, ok := a.cfg.Sessions.(session.Compactor)
	if !ok {
		a.log.Warn("agent : compression demandée mais le Store n'implémente pas session.Compactor", "session", t.conv.Session)
		return llm.TrimHistory(msgs, a.cfg.HistoryLimit), summary
	}
	// Coupe au début d'un tour (message user), en gardant au moins Keep messages.
	cut := len(msgs) - c.Keep
	for cut > 0 && msgs[cut].Role != llm.RoleUser {
		cut--
	}
	if cut <= 0 {
		return msgs, summary
	}
	old := msgs[:cut]
	if c.OnCompact != nil {
		c.OnCompact(ctx, t.conv, old)
	}
	var next string
	var err error
	if c.Summarize != nil {
		next, err = c.Summarize(ctx, t.conv, summary, old)
	} else {
		next, err = a.summarize(ctx, t, summary, old)
	}
	if err == nil && strings.TrimSpace(next) == "" {
		err = errors.New("résumé vide")
	}
	if err == nil {
		err = store.Compact(ctx, t.conv.Session, strings.TrimSpace(next), len(msgs)-cut)
	}
	if err != nil {
		a.log.Warn("agent : compression impossible", "session", t.conv.Session, "err", err)
		return msgs, summary
	}
	a.log.Info("agent : historique compressé", "session", t.conv.Session, "archived", cut, "kept", len(msgs)-cut)
	return msgs[cut:], strings.TrimSpace(next)
}

// summarize résume avec le modèle de l'agent.
func (a *Agent) summarize(ctx context.Context, t *turn, previous string, msgs []llm.Message) (string, error) {
	// La consigne est répétée dans le message user : les petits modèles la
	// suivent mieux qu'en prompt système seul.
	var sb strings.Builder
	sb.WriteString(a.cfg.Texts.Summarize + "\n\n")
	if previous != "" {
		sb.WriteString("Résumé précédent :\n" + previous + "\n\n")
	}
	sb.WriteString("Conversation à résumer :\n<conversation>\n")
	for _, m := range msgs {
		switch {
		case m.Role == llm.RoleTool:
			sb.WriteString("[résultat d'outil] " + truncate(m.Content, 600) + "\n")
		case len(m.ToolCalls) > 0:
			for _, tc := range m.ToolCalls {
				sb.WriteString("[appel d'outil] " + tc.Name + " " + truncate(tc.Arguments, 300) + "\n")
			}
			if m.Content != "" {
				sb.WriteString(string(m.Role) + " : " + m.Content + "\n")
			}
		case m.Content != "":
			sb.WriteString(string(m.Role) + " : " + m.Content + "\n")
		}
	}
	sb.WriteString("</conversation>\n\nÉcris maintenant le résumé, rien d'autre.")
	req := &llm.Request{Model: a.cfg.Model, MaxTokens: a.cfg.MaxTokens, Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: a.cfg.Texts.Summarize},
		{Role: llm.RoleUser, Content: sb.String()},
	}}
	resp, err := a.call(ctx, t, req)
	if err != nil {
		return "", err
	}
	return cleanReply(resp.Message.Content), nil
}

func (a *Agent) buildSystem(ctx context.Context, conv tool.Conversation) (string, error) {
	system := a.cfg.Texts.Identity
	if a.cfg.System != nil {
		s, err := a.cfg.System(ctx, conv)
		if err != nil {
			return "", fmt.Errorf("agent : construction du prompt système : %w", err)
		}
		system = s
	}
	return strings.TrimSpace(system) + "\n\n" + a.cfg.Texts.UntrustedNotice, nil
}

func (a *Agent) note(extra string) string {
	var parts []string
	if !a.cfg.NoTimeNote {
		parts = append(parts, a.cfg.Texts.Now(a.cfg.Clock().In(a.cfg.Location)))
	}
	if s := strings.TrimSpace(extra); s != "" {
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n")
}

func (a *Agent) loop(ctx context.Context, t *turn) (*Outcome, error) {
	var specs []tool.Spec
	if !t.in.NoTools {
		specs = a.tools.Specs(ctx, func(s tool.Spec) bool {
			return a.cfg.Expose == nil || a.cfg.Expose(ctx, s)
		})
	}
	llmTools := make([]llm.ToolSpec, 0, len(specs))
	for _, s := range specs {
		llmTools = append(llmTools, llm.ToolSpec{Name: s.Name, Description: s.Description, Parameters: s.Parameters})
	}

	emptyRetried := false
	for step := 0; step < a.cfg.MaxSteps; step++ {
		resp, err := a.chat(ctx, t, llmTools)
		if err != nil {
			return nil, err
		}
		msg := resp.Message
		calls := msg.ToolCalls
		if len(calls) == 0 && a.cfg.ParseInlineTools && len(specs) > 0 {
			if tc, ok := parseInlineToolCall(msg.Content, specs); ok {
				calls, msg.Content = []llm.ToolCall{tc}, ""
			}
		}

		if len(calls) == 0 {
			if text := cleanReply(msg.Content); text != "" {
				t.add(llm.Message{Role: llm.RoleAssistant, Content: text, Reasoning: msg.Reasoning})
				return &Outcome{Status: Done, Reply: text}, nil
			}
			// Réponse vide (fréquent avec les modèles qui « réfléchissent ») :
			// une nouvelle tentative, puis la réponse finale forcée.
			if emptyRetried {
				break
			}
			emptyRetried = true
			a.log.Warn("agent : réponse vide du modèle, nouvelle tentative", "session", t.conv.Session)
			continue
		}

		assignCallIDs(calls, step)
		t.add(llm.Message{Role: llm.RoleAssistant, Content: msg.Content, ToolCalls: calls, Reasoning: msg.Reasoning})

		var wait *tool.Wait
		for _, tc := range calls {
			if wait != nil {
				t.add(llm.Message{Role: llm.RoleTool, Content: notExecuted, ToolCallID: tc.ID})
				continue
			}
			res := a.runTool(ctx, t, specs, tc)
			t.add(llm.Message{Role: llm.RoleTool, Content: forModel(tc.Name, res), ToolCallID: tc.ID})
			if res.Wait != nil {
				wait = res.Wait
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if wait != nil {
			reply := strings.TrimSpace(wait.Reply)
			if reply == "" {
				reply = a.finalAnswer(ctx, t)
			}
			t.add(llm.Message{Role: llm.RoleAssistant, Content: reply})
			return &Outcome{Status: Waiting, Reply: reply, Wait: wait}, nil
		}
		if t.in.Steer != nil {
			if extra := nonEmpty(t.in.Steer()); len(extra) > 0 {
				// Plusieurs messages arrivés pendant le tour forment un seul
				// message user : jamais deux messages du même rôle d'affilée.
				t.add(llm.Message{Role: llm.RoleUser, Content: strings.Join(extra, "\n\n")})
			}
		}
	}
	a.log.Warn("agent : réponse finale forcée", "session", t.conv.Session, "max_steps", a.cfg.MaxSteps)
	reply := a.finalAnswer(ctx, t)
	t.add(llm.Message{Role: llm.RoleAssistant, Content: reply})
	return &Outcome{Status: Done, Reply: reply}, nil
}

// chat appelle le modèle, en flux si l'appelant le demande et que le
// fournisseur le permet.
func (a *Agent) chat(ctx context.Context, t *turn, tools []llm.ToolSpec) (*llm.Response, error) {
	req := &llm.Request{Model: a.cfg.Model, Messages: t.api, Tools: tools, Temperature: a.cfg.Temperature, MaxTokens: a.cfg.MaxTokens}
	if len(tools) == 0 {
		req.Tools = nil
	}
	return a.call(ctx, t, req)
}

func (a *Agent) call(ctx context.Context, t *turn, req *llm.Request) (*llm.Response, error) {
	start := time.Now()
	t.steps++
	var resp *llm.Response
	var err error
	if st, ok := a.cfg.Provider.(llm.Streamer); ok && t.in.Stream != nil {
		resp, err = streamChat(ctx, st, req, t.in.Stream)
	} else {
		resp, err = a.cfg.Provider.Chat(ctx, req)
	}
	if err == nil && resp == nil {
		err = errors.New("le fournisseur n'a renvoyé aucune réponse")
	}
	ev := Event{Kind: EventModel, Conversation: t.conv, Step: t.steps, Duration: time.Since(start), Err: err, Model: req.Model}
	if resp != nil {
		ev.Usage = resp.Usage
		if resp.Model != "" {
			ev.Model = resp.Model
		}
		t.usage = t.usage.Add(resp.Usage)
	}
	a.emit(ctx, ev)
	if err != nil {
		return nil, fmt.Errorf("agent : appel au modèle : %w", err)
	}
	return resp, nil
}

func streamChat(ctx context.Context, st llm.Streamer, req *llm.Request, onDelta func(string)) (*llm.Response, error) {
	ch, err := st.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	var final *llm.Response
	for ev := range ch {
		if ev.Err != nil {
			return nil, ev.Err
		}
		if ev.Delta != "" {
			onDelta(ev.Delta)
		}
		if ev.Response != nil {
			final = ev.Response
		}
	}
	if final == nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("flux interrompu sans réponse finale")
	}
	return final, nil
}

// runTool résout, valide puis exécute un appel à travers les middlewares.
// Toute erreur devient un résultat lisible par le modèle, pour qu'il se corrige.
func (a *Agent) runTool(ctx context.Context, t *turn, specs []tool.Spec, tc llm.ToolCall) tool.Result {
	start := time.Now()
	call := tool.Call{ID: tc.ID, Name: tc.Name}
	res := a.execute(ctx, specs, tc, &call)
	res.Content = truncate(res.Content, a.cfg.MaxToolResultChars)
	a.emit(ctx, Event{Kind: EventTool, Conversation: t.conv, Step: t.steps, Duration: time.Since(start), Call: &call, Result: &res})
	return res
}

func (a *Agent) execute(ctx context.Context, specs []tool.Spec, tc llm.ToolCall, call *tool.Call) tool.Result {
	name := tool.Resolve(tc.Name, specs)
	if name == "" {
		names := make([]string, 0, len(specs))
		for _, s := range specs {
			names = append(names, s.Name)
		}
		return tool.Text(fmt.Sprintf("Erreur : l'outil %q n'existe pas ou n'est pas disponible ici. Outils disponibles : %s.",
			tc.Name, strings.Join(names, ", ")))
	}
	impl, spec, ok := a.tools.Get(name)
	if !ok {
		return tool.Text(fmt.Sprintf("Erreur : l'outil %q n'est plus disponible.", name))
	}
	call.Name, call.Spec = name, spec

	args, err := parseArguments(tc.Arguments)
	if err != nil {
		return tool.Text(fmt.Sprintf("Erreur : arguments invalides pour %s (%s). Renvoie un objet JSON conforme au schéma de l'outil.", name, err))
	}
	if err := a.tools.Validate(name, args); err != nil {
		return tool.Text(fmt.Sprintf("Erreur : %s. Corrige les arguments de %s selon son schéma.", err, name))
	}
	raw, _ := json.Marshal(args)
	call.Args, call.Raw = args, raw

	h := ToolHandler(impl.Run)
	for i := len(a.cfg.Middleware) - 1; i >= 0; i-- {
		h = a.cfg.Middleware[i](h)
	}
	res, err := h(ctx, *call)
	if err != nil {
		return tool.Text("Erreur : " + err.Error())
	}
	return res
}

// forModel met en forme un résultat pour le modèle : un contenu non fiable est
// délimité comme une donnée.
func forModel(name string, r tool.Result) string {
	if !r.Untrusted {
		return r.Content
	}
	c := strings.ReplaceAll(r.Content, "</untrusted_data>", "</untrusted-data>")
	return fmt.Sprintf("<untrusted_data source=%q>\n%s\n</untrusted_data>", name, c)
}

// finalAnswer demande au modèle, sans outils, une réponse à l'interlocuteur.
// La consigne n'est pas enregistrée.
func (a *Agent) finalAnswer(ctx context.Context, t *turn) string {
	msgs := append([]llm.Message(nil), t.api...)
	if last := len(msgs) - 1; msgs[last].Role == llm.RoleUser {
		msgs[last].Content += "\n\n" + a.cfg.Texts.FinalAnswer
	} else {
		msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: a.cfg.Texts.FinalAnswer})
	}
	req := &llm.Request{Model: a.cfg.Model, Messages: msgs, Temperature: a.cfg.Temperature, MaxTokens: a.cfg.MaxTokens}
	resp, err := a.call(ctx, t, req)
	if err != nil {
		a.log.Warn("agent : réponse finale impossible", "session", t.conv.Session, "err", err)
		return a.cfg.Texts.Fallback
	}
	if text := cleanReply(resp.Message.Content); text != "" {
		return text
	}
	return a.cfg.Texts.Fallback
}

func (a *Agent) emit(ctx context.Context, ev Event) {
	if a.cfg.OnEvent != nil {
		a.cfg.OnEvent(ctx, ev)
	}
}

func truncate(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	head, tail := limit*2/3, limit/3
	for head > 0 && !utf8Start(s[head]) {
		head--
	}
	start := len(s) - tail
	for start < len(s) && !utf8Start(s[start]) {
		start++
	}
	return s[:head] + fmt.Sprintf("\n[... %d caractères omis ...]\n", start-head) + s[start:]
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// assignCallIDs donne un identifiant aux appels qui n'en ont pas (Ollama,
// appels écrits en texte) : sans lui, un résultat ne peut pas être rattaché à
// son appel dans l'historique.
func assignCallIDs(calls []llm.ToolCall, step int) {
	for i := range calls {
		if calls[i].ID == "" {
			calls[i].ID = fmt.Sprintf("call_%d_%d", step, i)
		}
	}
}

var trailingComma = regexp.MustCompile(`,\s*([}\]])`)

// parseArguments lit les arguments JSON d'un appel, en réparant les défauts
// fréquents des modèles (vide, "null", virgule finale).
func parseArguments(raw string) (map[string]any, error) {
	s := strings.TrimSpace(raw)
	if s == "" || s == "null" || s == "None" {
		return map[string]any{}, nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(s), &args); err != nil {
		if json.Unmarshal([]byte(trailingComma.ReplaceAllString(s, "$1")), &args) != nil {
			return nil, err
		}
	}
	if args == nil {
		args = map[string]any{}
	}
	return args, nil
}

// pythonLiterals remplace True, False et None hors des chaînes JSON.
func pythonLiterals(s string) string {
	var b strings.Builder
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			b.WriteByte(c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			b.WriteByte(c)
			continue
		}
		replaced := false
		for lit, js := range map[string]string{"True": "true", "False": "false", "None": "null"} {
			if strings.HasPrefix(s[i:], lit) && (i == 0 || !isIdent(s[i-1])) && (i+len(lit) == len(s) || !isIdent(s[i+len(lit)])) {
				b.WriteString(js)
				i += len(lit) - 1
				replaced = true
				break
			}
		}
		if !replaced {
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isIdent(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

var thinkBlock = regexp.MustCompile(`(?s)<think>.*?</think>`)

// cleanReply retire le raisonnement exposé par certains modèles.
func cleanReply(s string) string {
	return strings.TrimSpace(thinkBlock.ReplaceAllString(s, ""))
}

func nonEmpty(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// parseInlineToolCall lit un appel d'outil écrit en texte : un objet JSON
// {"name": …, "parameters"|"arguments": {…}}, éventuellement dans un bloc de
// code. Seul un outil montré au modèle est accepté.
func parseInlineToolCall(content string, specs []tool.Spec) (llm.ToolCall, bool) {
	s := strings.TrimSpace(content)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	i, j := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if i < 0 || j <= i {
		return llm.ToolCall{}, false
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal([]byte(s[i:j+1]), &raw) != nil {
		// Littéraux Python (True, False, None) fréquents chez les petits modèles.
		if json.Unmarshal([]byte(pythonLiterals(s[i:j+1])), &raw) != nil {
			return llm.ToolCall{}, false
		}
	}
	var name string
	if json.Unmarshal(raw["name"], &name) != nil || tool.Resolve(name, specs) == "" {
		return llm.ToolCall{}, false
	}
	args := json.RawMessage("{}")
	if v, ok := raw["parameters"]; ok {
		args = v
	} else if v, ok := raw["arguments"]; ok {
		args = v
	}
	return llm.ToolCall{Name: name, Arguments: string(args)}, true
}
