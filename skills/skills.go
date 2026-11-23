// Package skills charge des skills au format SKILL.md (agentskills.io) :
// un dossier par skill, avec un fichier SKILL.md qui commence par un
// en-tête YAML (name, description) suivi des instructions, et
// éventuellement des fichiers annexes (modèles, scripts, références).
//
// Seul l'index (nom et description) entre dans le prompt système ; l'agent
// lit le contenu d'un skill à la demande avec skill_view. Il peut aussi en
// écrire (skill_save) pour retenir une méthode qui a marché.
package skills

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/raucheacho/agentkit/tool"
)

// Skill est un skill chargé.
type Skill struct {
	Name        string
	Description string
	// Dir est le dossier du skill ; Body les instructions (sans l'en-tête).
	Dir  string
	Body string
	// Meta contient les autres champs simples de l'en-tête.
	Meta map[string]string
}

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// Library est un ensemble de skills chargés depuis des dossiers. Pour un même
// nom, le premier dossier l'emporte.
type Library struct {
	dirs []string
	mu   sync.RWMutex
	byID map[string]Skill
	errs []error
}

// Load charge les skills de dirs (dossiers absents ignorés). Un skill
// illisible est ignoré et signalé par Errors.
func Load(dirs ...string) (*Library, error) {
	l := &Library{dirs: dirs}
	return l, l.Reload()
}

// Reload relit les dossiers. Les sessions en cours gardent l'index de leur
// création (prompt figé) ; skill_view voit tout de suite les changements.
func (l *Library) Reload() error {
	byID := map[string]Skill{}
	var errs []error
	for _, dir := range l.dirs {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("skills : lecture de %s : %w", dir, err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			sk, err := parseFile(filepath.Join(dir, e.Name()))
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if _, dup := byID[sk.Name]; !dup {
				byID[sk.Name] = sk
			}
		}
	}
	l.mu.Lock()
	l.byID, l.errs = byID, errs
	l.mu.Unlock()
	return nil
}

// Errors renvoie les skills ignorés au dernier chargement.
func (l *Library) Errors() []error {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return slices.Clone(l.errs)
}

// List renvoie les skills, triés par nom.
func (l *Library) List() []Skill {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]Skill, 0, len(l.byID))
	for _, s := range l.byID {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b Skill) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Get renvoie un skill.
func (l *Library) Get(name string) (Skill, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	s, ok := l.byID[strings.ToLower(strings.TrimSpace(name))]
	return s, ok
}

func parseFile(dir string) (Skill, error) {
	data, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return Skill{}, err
	}
	sk, err := Parse(string(data))
	if err != nil {
		return Skill{}, fmt.Errorf("skills : %s : %w", dir, err)
	}
	if sk.Name == "" {
		sk.Name = strings.ToLower(filepath.Base(dir))
	}
	if !validName.MatchString(sk.Name) {
		return Skill{}, fmt.Errorf("skills : %s : nom invalide %q (minuscules, chiffres et tirets)", dir, sk.Name)
	}
	if sk.Description == "" {
		return Skill{}, fmt.Errorf("skills : %s : description manquante", dir)
	}
	sk.Dir = dir
	return sk, nil
}

// Parse lit un SKILL.md : en-tête entre deux lignes « --- » (champs simples
// « clé: valeur », guillemets retirés ; les blocs imbriqués sont ignorés),
// puis le corps.
func Parse(text string) (Skill, error) {
	text = strings.TrimPrefix(strings.ReplaceAll(text, "\r\n", "\n"), "\uFEFF")
	sk := Skill{Meta: map[string]string{}}
	if !strings.HasPrefix(text, "---\n") {
		return sk, errors.New("en-tête manquant (le fichier doit commencer par ---)")
	}
	head, body, ok := strings.Cut(text[4:], "\n---")
	if !ok {
		return sk, errors.New("en-tête non fermé")
	}
	body = strings.TrimPrefix(body, "-") // tolère « ---- »
	_, body, _ = strings.Cut(body, "\n")
	for _, line := range strings.Split(head, "\n") {
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		switch k {
		case "name":
			sk.Name = strings.ToLower(v)
		case "description":
			sk.Description = v
		default:
			if v != "" && v != "|" && v != ">" {
				sk.Meta[k] = v
			}
		}
	}
	sk.Body = strings.TrimSpace(body)
	return sk, nil
}

// Section renvoie l'index des skills pour le prompt système (signature de
// prompt.Section) ; rien s'il n'y a aucun skill.
func (l *Library) Section(ctx context.Context, _ tool.Conversation) (string, error) {
	list := l.List()
	if len(list) == 0 {
		return "", nil
	}
	var sb strings.Builder
	sb.WriteString("## Skills\n\nAvant une tâche qui correspond à l'un de ces skills, lis-le avec skill_view et suis-le.")
	for _, s := range list {
		fmt.Fprintf(&sb, "\n- %s : %s", s.Name, s.Description)
	}
	return sb.String(), nil
}

type viewIn struct {
	Name string `json:"name" desc:"nom du skill"`
	File string `json:"file,omitempty" desc:"fichier annexe du skill à lire (chemin relatif), au lieu de SKILL.md"`
}

// maxFile borne la lecture d'un fichier de skill.
const maxFile = 64 << 10

// ViewTool renvoie skill_view : lire un skill ou l'un de ses fichiers, sans
// jamais sortir de son dossier.
func (l *Library) ViewTool() tool.Tool {
	return tool.New("skill_view", "Lit les instructions d'un skill (ou l'un de ses fichiers annexes).",
		func(_ context.Context, in viewIn) (tool.Result, error) {
			sk, ok := l.Get(in.Name)
			if !ok {
				names := make([]string, 0)
				for _, s := range l.List() {
					names = append(names, s.Name)
				}
				return tool.Text(fmt.Sprintf("Erreur : skill %q inconnu. Skills : %s.", in.Name, strings.Join(names, ", "))), nil
			}
			if strings.TrimSpace(in.File) == "" {
				return tool.Text(fmt.Sprintf("# %s\n\n%s%s", sk.Name, sk.Body, filesOf(sk.Dir))), nil
			}
			content, err := readInside(sk.Dir, in.File)
			if err != nil {
				return tool.Text("Erreur : " + err.Error()), nil
			}
			return tool.Text(content), nil
		})
}

// readInside lit un fichier de dir sans pouvoir en sortir (chemins
// absolus, « .. » et liens symboliques sortants refusés par os.Root).
func readInside(dir, name string) (string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	f, err := root.Open(filepath.Clean(name))
	if err != nil {
		return "", fmt.Errorf("fichier %q introuvable dans le skill", name)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return "", fmt.Errorf("fichier %q illisible", name)
	}
	if len(data) > maxFile {
		return string(data[:maxFile]) + "\n[… fichier tronqué …]", nil
	}
	return string(data), nil
}

func filesOf(dir string) string {
	var files []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || len(files) >= 30 {
			return nil
		}
		if rel, _ := filepath.Rel(dir, p); rel != "SKILL.md" {
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if len(files) == 0 {
		return ""
	}
	return "\n\nFichiers annexes (skill_view avec file) : " + strings.Join(files, ", ")
}

type saveIn struct {
	Name        string `json:"name" desc:"nom du skill : minuscules, chiffres et tirets"`
	Description string `json:"description" desc:"quand utiliser ce skill, en une phrase"`
	Body        string `json:"body" desc:"instructions complètes, en Markdown"`
}

// SaveTool renvoie skill_save : l'agent écrit ou remplace un skill dans dir
// (qui doit faire partie des dossiers de la bibliothèque pour être relu).
func (l *Library) SaveTool(dir string) tool.Tool {
	return tool.New("skill_save", "Enregistre une méthode réutilisable sous forme de skill (crée ou remplace), "+
		"pour la suivre la prochaine fois qu'une tâche semblable se présente.",
		func(_ context.Context, in saveIn) (tool.Result, error) {
			name := strings.ToLower(strings.TrimSpace(in.Name))
			desc := strings.Join(strings.Fields(in.Description), " ")
			if !validName.MatchString(name) {
				return tool.Text("Erreur : nom invalide (minuscules, chiffres et tirets, 64 caractères au plus)."), nil
			}
			if desc == "" || strings.TrimSpace(in.Body) == "" {
				return tool.Text("Erreur : description et body sont obligatoires."), nil
			}
			if sk, ok := l.Get(name); ok && filepath.Dir(sk.Dir) != filepath.Clean(dir) {
				return tool.Text("Erreur : le skill " + name + " est fourni par l'application et ne peut pas être remplacé."), nil
			}
			if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
				return tool.Result{}, err
			}
			desc = strings.ReplaceAll(desc, `"`, "'")
			content := fmt.Sprintf("---\nname: %s\ndescription: \"%s\"\n---\n\n%s\n", name, desc, strings.TrimSpace(in.Body))
			if err := os.WriteFile(filepath.Join(dir, name, "SKILL.md"), []byte(content), 0o644); err != nil {
				return tool.Result{}, err
			}
			if err := l.Reload(); err != nil {
				return tool.Result{}, err
			}
			return tool.Text("Skill " + name + " enregistré. Il apparaîtra dans l'index des prochaines conversations."), nil
		}, tool.WithEffect(tool.Write))
}
