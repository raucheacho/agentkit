package computer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/raucheacho/agentkit/tool"
)

// Record décrit une commande exécutée, pour l'historique des commandes et
// les traces. Command et Output sont passés par Redact.
type Record struct {
	Conversation tool.Conversation
	Command      string
	ExitCode     int
	// Output : fin de la sortie (au plus RecordChars caractères).
	Output   string
	TimedOut bool
	Duration time.Duration
	Err      string
}

// ToolsConfig règle les outils.
type ToolsConfig struct {
	// BashEffect : effet de l'outil bash pour la porte de confiance (défaut
	// Write : l'espace de travail ne contient ni clé ni session ; Sensitive
	// fait valider chaque commande).
	BashEffect tool.Effect
	// DefaultTimeout (défaut 10 min) et MaxTimeout (défaut 60 min) d'une commande.
	DefaultTimeout time.Duration
	MaxTimeout     time.Duration
	// MaxOutputChars borne la sortie montrée au modèle (défaut 30 000).
	MaxOutputChars int
	// Redact retire les secrets connus d'un texte enregistré (nil : rien).
	Redact func(string) string
	// RecordChars borne la sortie d'un Record (défaut 16 000).
	RecordChars int
	// OnExec reçoit chaque commande (historique, événements).
	OnExec func(ctx context.Context, r Record)
}

type bashIn struct {
	Command        string `json:"command" desc:"commande bash, exécutée dans ton dossier de travail"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" desc:"délai en secondes (défaut 600)"`
}

type readIn struct {
	Path   string `json:"path" desc:"chemin relatif à ton dossier de travail"`
	Offset int    `json:"offset,omitempty" desc:"première ligne à lire (à partir de 1)"`
	Limit  int    `json:"limit,omitempty" desc:"nombre de lignes (défaut 2000)"`
}

type writeIn struct {
	Path    string `json:"path" desc:"chemin relatif à ton dossier de travail"`
	Content string `json:"content" desc:"contenu complet du fichier"`
}

type editIn struct {
	Path       string `json:"path" desc:"chemin relatif à ton dossier de travail"`
	OldString  string `json:"old_string" desc:"texte exact à remplacer"`
	NewString  string `json:"new_string" desc:"texte de remplacement"`
	ReplaceAll bool   `json:"replace_all,omitempty" desc:"remplacer toutes les occurrences"`
}

const maxReadBytes = 4 << 20

// Tools renvoie bash, read_file, write_file et edit_file, qui agissent dans
// l'espace de travail de la conversation appelante.
func Tools(ws Workspace, cfg ToolsConfig) []tool.Tool {
	if cfg.BashEffect == tool.Read {
		cfg.BashEffect = tool.Write // une commande peut toujours écrire
	}
	if cfg.DefaultTimeout <= 0 {
		cfg.DefaultTimeout = 10 * time.Minute
	}
	if cfg.MaxTimeout <= 0 {
		cfg.MaxTimeout = time.Hour
	}
	if cfg.MaxOutputChars <= 0 {
		cfg.MaxOutputChars = 30000
	}
	if cfg.RecordChars <= 0 {
		cfg.RecordChars = 16000
	}
	if cfg.Redact == nil {
		cfg.Redact = func(s string) string { return s }
	}
	open := func(ctx context.Context) (Session, tool.Conversation, error) {
		conv, _ := tool.ConversationFrom(ctx)
		if Key(conv) == "" {
			return nil, conv, errors.New("conversation inconnue")
		}
		s, err := ws.Open(ctx, Key(conv))
		return s, conv, err
	}
	return []tool.Tool{
		tool.New("bash", "Lance une commande bash sur ton ordinateur (Linux, séparé de celui de ton interlocuteur), "+
			"dans ton dossier de travail. Tu peux y installer des paquets Python (pip install --user), convertir des "+
			"fichiers, exécuter du code. Les fichiers restent d'un message à l'autre.",
			func(ctx context.Context, in bashIn) (tool.Result, error) {
				s, conv, err := open(ctx)
				if err != nil {
					return tool.Text("Erreur : " + err.Error()), nil
				}
				timeout := cfg.DefaultTimeout
				if in.TimeoutSeconds > 0 {
					timeout = min(time.Duration(in.TimeoutSeconds)*time.Second, cfg.MaxTimeout)
				}
				res, err := s.Exec(ctx, in.Command, ExecOptions{Timeout: timeout})
				rec := Record{Conversation: conv, Command: cfg.Redact(in.Command), ExitCode: res.ExitCode, TimedOut: res.TimedOut, Duration: res.Duration}
				out := string(res.Output)
				rec.Output = tail(cfg.Redact(out), cfg.RecordChars)
				if err != nil {
					rec.Err = err.Error()
				}
				if cfg.OnExec != nil {
					cfg.OnExec(ctx, rec)
				}
				switch {
				case errors.Is(err, ErrUncertain):
					return tool.Text("Issue inconnue : la liaison avec l'ordinateur a été perdue pendant la commande. " +
						"Vérifie son effet avant toute nouvelle tentative ; ne la relance pas à l'aveugle."), nil
				case err != nil && ctx.Err() != nil:
					return tool.Result{}, ctx.Err()
				case err != nil:
					return tool.Text("Erreur : " + err.Error()), nil
				}
				var sb strings.Builder
				if res.TimedOut {
					fmt.Fprintf(&sb, "Commande arrêtée : délai de %s dépassé.\n", timeout)
				}
				fmt.Fprintf(&sb, "Code de sortie : %d\n", res.ExitCode)
				if res.Truncated {
					sb.WriteString("[sortie tronquée]\n")
				}
				sb.WriteString(clipChars(out, cfg.MaxOutputChars))
				return tool.External(strings.TrimRight(sb.String(), "\n")), nil
			}, tool.WithEffect(cfg.BashEffect)),

		tool.New("read_file", "Lit un fichier de ton dossier de travail, avec les numéros de ligne.",
			func(ctx context.Context, in readIn) (tool.Result, error) {
				s, _, err := open(ctx)
				if err != nil {
					return tool.Text("Erreur : " + err.Error()), nil
				}
				data, err := s.ReadFile(ctx, in.Path, maxReadBytes)
				if err != nil {
					return tool.Text("Erreur : " + err.Error()), nil
				}
				if bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
					return tool.Text(fmt.Sprintf("Fichier binaire (%d octets) : utilise bash pour l'examiner.", len(data))), nil
				}
				lines := strings.Split(string(data), "\n")
				start := max(in.Offset, 1)
				limit := in.Limit
				if limit <= 0 {
					limit = 2000
				}
				if start > len(lines) {
					return tool.Text(fmt.Sprintf("Le fichier n'a que %d lignes.", len(lines))), nil
				}
				end := min(start-1+limit, len(lines))
				var sb strings.Builder
				for i := start - 1; i < end; i++ {
					fmt.Fprintf(&sb, "%6d\t%s\n", i+1, clipChars(lines[i], 2000))
				}
				if end < len(lines) {
					fmt.Fprintf(&sb, "[… %d lignes de plus : offset=%d]", len(lines)-end, end+1)
				}
				return tool.External(clipChars(strings.TrimRight(sb.String(), "\n"), cfg.MaxOutputChars)), nil
			}),

		tool.New("write_file", "Crée ou remplace un fichier dans ton dossier de travail.",
			func(ctx context.Context, in writeIn) (tool.Result, error) {
				s, _, err := open(ctx)
				if err != nil {
					return tool.Text("Erreur : " + err.Error()), nil
				}
				if err := s.WriteFile(ctx, in.Path, []byte(in.Content)); err != nil {
					return tool.Text("Erreur : " + err.Error()), nil
				}
				return tool.Text(fmt.Sprintf("Fichier %s écrit (%d octets).", in.Path, len(in.Content))), nil
			}, tool.WithEffect(tool.Write)),

		tool.New("edit_file", "Remplace un texte exact dans un fichier de ton dossier de travail.",
			func(ctx context.Context, in editIn) (tool.Result, error) {
				s, _, err := open(ctx)
				if err != nil {
					return tool.Text("Erreur : " + err.Error()), nil
				}
				if in.OldString == "" || in.OldString == in.NewString {
					return tool.Text("Erreur : old_string doit être non vide et différent de new_string."), nil
				}
				data, err := s.ReadFile(ctx, in.Path, maxReadBytes)
				if err != nil {
					return tool.Text("Erreur : " + err.Error()), nil
				}
				text := string(data)
				n := strings.Count(text, in.OldString)
				switch {
				case n == 0:
					return tool.Text("Erreur : old_string introuvable dans " + in.Path + ". Relis le fichier (read_file)."), nil
				case n > 1 && !in.ReplaceAll:
					return tool.Text(fmt.Sprintf("Erreur : old_string apparaît %d fois ; donne plus de contexte ou replace_all.", n)), nil
				}
				if in.ReplaceAll {
					text = strings.ReplaceAll(text, in.OldString, in.NewString)
				} else {
					text = strings.Replace(text, in.OldString, in.NewString, 1)
				}
				if err := s.WriteFile(ctx, in.Path, []byte(text)); err != nil {
					return tool.Text("Erreur : " + err.Error()), nil
				}
				return tool.Text(fmt.Sprintf("%s modifié (%d remplacement(s)).", in.Path, map[bool]int{true: n, false: 1}[in.ReplaceAll])), nil
			}, tool.WithEffect(tool.Write)),
	}
}

// Redactor renvoie un Redact qui masque des secrets connus.
func Redactor(secrets ...string) func(string) string {
	var pairs []string
	for _, s := range secrets {
		if len(s) >= 4 {
			pairs = append(pairs, s, "[secret masqué]")
		}
	}
	r := strings.NewReplacer(pairs...)
	return r.Replace
}

func clipChars(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	head, tl := max/2, max-max/2
	return string(r[:head]) + fmt.Sprintf("\n[… %d caractères omis …]\n", len(r)-head-tl) + string(r[len(r)-tl:])
}

func tail(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return "…" + string(r[len(r)-max:])
}
