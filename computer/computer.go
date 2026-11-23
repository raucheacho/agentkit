// Package computer donne à un agent son propre ordinateur : un espace de
// travail où il lance des commandes et garde des fichiers, séparé de la
// machine de l'application.
//
// Le package définit le contrat (Workspace, Session) et les outils bash,
// read_file, write_file et edit_file. Les implémentations vivent à côté :
// computer/docker (conteneur verrouillé, recommandé) et computer/local (un
// dossier de la machine, sans isolation : développement et tests).
//
// Invariants :
//   - chaque conversation a son dossier ; un sous-agent travaille dans celui
//     de son parent ;
//   - aucun chemin ne sort du dossier de la conversation ;
//   - aucune variable d'environnement de l'application (clés, jetons)
//     n'atteint les commandes ;
//   - une commande dont l'issue est inconnue (flux perdu) n'est jamais
//     relancée (ErrUncertain) ;
//   - la sortie d'une commande est une donnée non fiable.
package computer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/raucheacho/agentkit/tool"
)

// ExecOptions règle une commande.
type ExecOptions struct {
	// Stdin est passé sur l'entrée standard.
	Stdin []byte
	// Timeout borne la commande (0 : défaut de l'implémentation).
	Timeout time.Duration
	// MaxOutput borne la sortie gardée (0 : défaut de l'implémentation) :
	// au-delà, le début et la fin sont conservés.
	MaxOutput int
}

// ExecResult est l'issue d'une commande.
type ExecResult struct {
	ExitCode int
	// Output mêle sortie standard et erreurs, dans l'ordre d'écriture.
	Output    []byte
	Truncated bool
	// TimedOut : la commande a été arrêtée par son délai.
	TimedOut bool
	Duration time.Duration
}

// ErrUncertain : on ne sait pas si la commande s'est exécutée ni comment
// (connexion perdue). Elle ne doit pas être relancée automatiquement.
var ErrUncertain = errors.New("computer : issue de la commande inconnue")

// ErrPath : chemin hors du dossier de la conversation.
var ErrPath = errors.New("computer : chemin hors de l'espace de travail")

// Session est l'espace de travail d'une conversation.
type Session interface {
	// Dir est le dossier de travail tel que le voient les commandes.
	Dir() string
	// Exec lance une commande shell (bash -c) dans Dir.
	Exec(ctx context.Context, command string, opts ExecOptions) (ExecResult, error)
	// ReadFile lit au plus max octets d'un fichier (chemin relatif à Dir).
	ReadFile(ctx context.Context, name string, max int64) ([]byte, error)
	// WriteFile crée ou remplace un fichier (dossiers créés au besoin).
	WriteFile(ctx context.Context, name string, data []byte) error
}

// Workspace fournit les espaces de travail.
type Workspace interface {
	// Open renvoie l'espace de travail d'une clé (créé au besoin).
	Open(ctx context.Context, key string) (Session, error)
	Close() error
}

var unsafeChars = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

// Folder transforme une clé de conversation en nom de dossier sûr et
// stable : lisible, suivi d'une empreinte qui évite les collisions.
func Folder(key string) string {
	sum := sha256.Sum256([]byte(key))
	name := strings.Trim(unsafeChars.ReplaceAllString(key, "_"), "._-")
	if len(name) > 40 {
		name = name[:40]
	}
	if name == "" {
		name = "s"
	}
	return name + "-" + hex.EncodeToString(sum[:4])
}

// Clean valide un chemin donné par le modèle et le rend relatif au dossier
// de la conversation. Un chemin absolu est accepté s'il est dans dir.
func Clean(dir, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", ErrPath
	}
	if strings.HasPrefix(name, "/") {
		rel, ok := strings.CutPrefix(path.Clean(name), strings.TrimSuffix(dir, "/")+"/")
		if !ok {
			return "", ErrPath
		}
		name = rel
	}
	p := path.Clean(name)
	if p == "." || p == ".." || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) {
		return "", ErrPath
	}
	return p, nil
}

// Clip garde le début et la fin d'une sortie trop longue.
func Clip(out []byte, max int) ([]byte, bool) {
	if max <= 0 || len(out) <= max {
		return out, false
	}
	head, tail := max/2, max-max/2
	msg := fmt.Sprintf("\n[… %d octets omis …]\n", len(out)-head-tail)
	return append(append(append([]byte{}, out[:head]...), msg...), out[len(out)-tail:]...), true
}

// Key renvoie la clé d'espace de travail d'une conversation : sa session,
// ou celle de l'agent parent pour un sous-agent.
func Key(conv tool.Conversation) string {
	if conv.Parent != "" {
		return conv.Parent
	}
	return conv.Session
}
