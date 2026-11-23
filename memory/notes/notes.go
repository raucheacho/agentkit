package notes

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/raucheacho/agentkit/llm"
	"github.com/raucheacho/agentkit/memory"
	"github.com/raucheacho/agentkit/tool"
)

// Texts regroupe les textes adressés au modèle. Les valeurs par défaut sont en français.
type Texts struct {
	// Intro ouvre le bloc du prompt système.
	Intro string
	// RecallHeading ouvre les souvenirs rappelés dans la note éphémère.
	RecallHeading string
}

// DefaultTexts renvoie les textes par défaut.
func DefaultTexts() Texts {
	return Texts{
		Intro: "## Mémoire\n\nTu disposes d'une mémoire durable en notes « zone/nom » (par exemple clients/ana-martin, " +
			"preferences/horaires). Note avec memory_save ce qui servira plus tard (faits, préférences, décisions, " +
			"engagements) ; mets à jour une note plutôt que d'en créer une nouvelle ; relis avec memory_read avant " +
			"d'affirmer un souvenir. N'y mets jamais de mot de passe ni de donnée bancaire.",
		RecallHeading: "[Notes de ta mémoire peut-être utiles pour ce message]",
	}
}

// Config configure le fournisseur de notes.
type Config struct {
	Store Store
	// Embedder active la recherche hybride (le Store doit implémenter VectorStore).
	Embedder llm.Embedder
	// Scope renvoie le propriétaire des notes d'une conversation (défaut
	// memory.Scope : la session). Renvoyer une constante (« global ») partage
	// la mémoire entre toutes les conversations : à réserver à un assistant
	// personnel.
	Scope func(conv tool.Conversation) string
	// IndexLimit borne l'index du prompt système (défaut 50, < 0 : pas d'index).
	IndexLimit int
	// RecallLimit borne les notes rappelées à chaque tour (défaut 3, < 0 : aucun rappel).
	RecallLimit int
	// MinSimilarity écarte les notes trop éloignées dans la recherche par
	// vecteurs (cosinus, défaut 0,3 ; dépend du modèle d'embeddings).
	MinSimilarity float64
	// MaxNoteChars borne une note (défaut 4000).
	MaxNoteChars int
	Texts        Texts
	Logger       *slog.Logger
}

// Notes est un fournisseur de mémoire (memory.Provider) : prompt, rappel et outils.
type Notes struct {
	cfg Config
	log *slog.Logger
}

// New crée le fournisseur.
func New(cfg Config) *Notes {
	if cfg.Scope == nil {
		cfg.Scope = memory.Scope
	}
	if cfg.IndexLimit == 0 {
		cfg.IndexLimit = 50
	}
	if cfg.RecallLimit == 0 {
		cfg.RecallLimit = 3
	}
	if cfg.MinSimilarity == 0 {
		cfg.MinSimilarity = 0.3
	}
	if cfg.MaxNoteChars <= 0 {
		cfg.MaxNoteChars = 4000
	}
	def := DefaultTexts()
	if cfg.Texts.Intro == "" {
		cfg.Texts.Intro = def.Intro
	}
	if cfg.Texts.RecallHeading == "" {
		cfg.Texts.RecallHeading = def.RecallHeading
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Notes{cfg: cfg, log: log}
}

func (n *Notes) Name() string { return "notes" }

// Prompt donne les consignes et l'index des notes, figés pour la session.
func (n *Notes) Prompt(ctx context.Context, conv tool.Conversation) (string, error) {
	var sb strings.Builder
	sb.WriteString(n.cfg.Texts.Intro)
	if n.cfg.IndexLimit < 0 {
		return sb.String(), nil
	}
	list, err := n.cfg.Store.List(ctx, n.cfg.Scope(conv), "")
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		sb.WriteString("\n\nAucune note pour l'instant.")
		return sb.String(), nil
	}
	sb.WriteString("\n\nNotes existantes :")
	for i, note := range list {
		if i == n.cfg.IndexLimit {
			fmt.Fprintf(&sb, "\n- … et %d autres (memory_search)", len(list)-i)
			break
		}
		fmt.Fprintf(&sb, "\n- %s — %s", note.Path, firstLine(note.Content, 80))
	}
	return sb.String(), nil
}

// Recall renvoie les notes pertinentes pour le message reçu.
func (n *Notes) Recall(ctx context.Context, conv tool.Conversation, query string) (string, error) {
	if n.cfg.RecallLimit < 0 {
		return "", nil
	}
	hits, err := n.search(ctx, n.cfg.Scope(conv), query, n.cfg.RecallLimit)
	if err != nil || len(hits) == 0 {
		return "", err
	}
	var sb strings.Builder
	sb.WriteString(n.cfg.Texts.RecallHeading)
	for _, h := range hits {
		fmt.Fprintf(&sb, "\n- %s : %s", h.Path, clip(h.Content, 300))
	}
	return sb.String(), nil
}

// Search cherche dans les notes d'un scope : plein texte, et vecteurs si un
// Embedder est configuré (fusion par rangs réciproques).
func (n *Notes) Search(ctx context.Context, scope, query string, limit int) ([]Hit, error) {
	return n.search(ctx, scope, query, limit)
}

func (n *Notes) search(ctx context.Context, scope, query string, limit int) ([]Hit, error) {
	text, err := n.cfg.Store.Search(ctx, scope, query, limit*3)
	if err != nil {
		return nil, err
	}
	vs, ok := n.cfg.Store.(VectorStore)
	if n.cfg.Embedder == nil || !ok {
		return top(text, limit), nil
	}
	vecs, err := n.cfg.Embedder.Embed(ctx, []string{query})
	if err != nil || len(vecs) != 1 {
		n.log.Warn("notes : embedding de la recherche impossible, plein texte seul", "err", err)
		return top(text, limit), nil
	}
	near, err := vs.Nearest(ctx, scope, vecs[0], limit*3)
	if err != nil {
		return nil, err
	}
	kept := near[:0]
	for _, h := range near {
		if h.Score >= n.cfg.MinSimilarity {
			kept = append(kept, h)
		}
	}
	return fuse(limit, text, kept), nil
}

// fuse combine des classements par rangs réciproques (RRF, k = 60).
func fuse(limit int, lists ...[]Hit) []Hit {
	score := map[string]float64{}
	notes := map[string]Note{}
	for _, l := range lists {
		for rank, h := range l {
			score[h.Path] += 1 / float64(60+rank+1)
			notes[h.Path] = h.Note
		}
	}
	out := make([]Hit, 0, len(score))
	for p, s := range score {
		out = append(out, Hit{Note: notes[p], Score: s})
	}
	return top(out, limit)
}

// Save enregistre une note (et son vecteur si un Embedder est configuré).
func (n *Notes) Save(ctx context.Context, scope, path, content string) (string, error) {
	p, err := CleanPath(path)
	if err != nil {
		return "", err
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return "", fmt.Errorf("contenu vide (utilise memory_delete pour supprimer)")
	}
	if r := []rune(content); len(r) > n.cfg.MaxNoteChars {
		return "", fmt.Errorf("note trop longue (%d caractères, au plus %d) : découpe-la en plusieurs notes", len(r), n.cfg.MaxNoteChars)
	}
	if err := n.cfg.Store.Put(ctx, Note{Scope: scope, Path: p, Content: content}); err != nil {
		return "", err
	}
	if vs, ok := n.cfg.Store.(VectorStore); ok && n.cfg.Embedder != nil {
		vecs, err := n.cfg.Embedder.Embed(ctx, []string{p + "\n" + content})
		if err == nil && len(vecs) == 1 {
			err = vs.SetVector(ctx, scope, p, vecs[0])
		}
		if err != nil {
			n.log.Warn("notes : vecteur non enregistré", "path", p, "err", err)
		}
	}
	return p, nil
}

type saveIn struct {
	Path    string `json:"path" desc:"zone/nom, par exemple clients/ana-martin"`
	Content string `json:"content" desc:"contenu complet de la note (remplace l'ancien)"`
}

type pathIn struct {
	Path string `json:"path" desc:"zone/nom de la note"`
}

type searchIn struct {
	Query string `json:"query" desc:"mots-clés ou question"`
}

// Tools renvoie memory_save, memory_read, memory_search et memory_delete.
func (n *Notes) Tools() []tool.Tool {
	scope := func(ctx context.Context) string {
		conv, _ := tool.ConversationFrom(ctx)
		return n.cfg.Scope(conv)
	}
	return []tool.Tool{
		tool.New("memory_save", "Crée ou remplace une note de ta mémoire durable. Pour compléter une note, relis-la puis réécris-la en entier.",
			func(ctx context.Context, in saveIn) (tool.Result, error) {
				p, err := n.Save(ctx, scope(ctx), in.Path, in.Content)
				if err != nil {
					return tool.Text("Erreur : " + err.Error()), nil
				}
				return tool.Text("Note " + p + " enregistrée."), nil
			}, tool.WithEffect(tool.Write)),
		tool.New("memory_read", "Lit une note de ta mémoire.",
			func(ctx context.Context, in pathIn) (tool.Result, error) {
				p, err := CleanPath(in.Path)
				if err != nil {
					return tool.Text("Erreur : " + err.Error()), nil
				}
				note, err := n.cfg.Store.Get(ctx, scope(ctx), p)
				if err != nil {
					return tool.Result{}, err
				}
				if note == nil {
					return tool.Text("Aucune note " + p + ". Cherche avec memory_search."), nil
				}
				return tool.Text(fmt.Sprintf("%s (mise à jour %s)\n\n%s", p, note.UpdatedAt.Format("2006-01-02"), note.Content)), nil
			}),
		tool.New("memory_search", "Cherche dans ta mémoire les notes liées à un sujet.",
			func(ctx context.Context, in searchIn) (tool.Result, error) {
				hits, err := n.search(ctx, scope(ctx), in.Query, 8)
				if err != nil {
					return tool.Result{}, err
				}
				if len(hits) == 0 {
					return tool.Text("Aucune note ne correspond."), nil
				}
				var sb strings.Builder
				for _, h := range hits {
					fmt.Fprintf(&sb, "- %s : %s\n", h.Path, clip(h.Content, 200))
				}
				return tool.Text(strings.TrimSpace(sb.String())), nil
			}),
		tool.New("memory_delete", "Supprime une note devenue fausse ou inutile.",
			func(ctx context.Context, in pathIn) (tool.Result, error) {
				p, err := CleanPath(in.Path)
				if err != nil {
					return tool.Text("Erreur : " + err.Error()), nil
				}
				ok, err := n.cfg.Store.Delete(ctx, scope(ctx), p)
				if err != nil {
					return tool.Result{}, err
				}
				if !ok {
					return tool.Text("Aucune note " + p + "."), nil
				}
				return tool.Text("Note " + p + " supprimée."), nil
			}, tool.WithEffect(tool.Write)),
	}
}

func firstLine(s string, max int) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return clip(s, max)
}

func clip(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

var (
	_ memory.Provider     = (*Notes)(nil)
	_ memory.Prompter     = (*Notes)(nil)
	_ memory.Recaller     = (*Notes)(nil)
	_ memory.ToolProvider = (*Notes)(nil)
)
