package llm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
)

// StatusError est une erreur HTTP d'un fournisseur. Elle garde le statut et la
// raison donnée par le service pour qu'un appelant puisse décider quoi faire
// (réessayer un 429, signaler une clé invalide…).
type StatusError struct {
	Provider string
	Status   int
	Message  string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s: statut %d: %s", e.Provider, e.Status, e.Message)
}

// Transient indique si l'erreur mérite une nouvelle tentative : limite de
// débit, service indisponible, coupure réseau, délai dépassé côté réseau.
// Une annulation du contexte n'est jamais transitoire.
func Transient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) {
		switch se.Status {
		case 408, 409, 425, 429, 500, 502, 503, 504, 529:
			return true
		}
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, m := range []string{"connection reset", "connection refused", "broken pipe", "unexpected eof", "no such host"} {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}
