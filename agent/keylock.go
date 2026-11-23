package agent

import "sync"

// keyedMutex sérialise le travail d'une même clé et laisse les clés
// différentes en parallèle. Un verrou est libéré de la table quand plus
// personne ne l'attend : la table ne grossit pas avec le nombre de sessions.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*refLock
}

type refLock struct {
	sync.Mutex
	refs int
}

func newKeyedMutex() *keyedMutex {
	return &keyedMutex{locks: make(map[string]*refLock)}
}

// Lock prend le verrou de key et renvoie la fonction qui le relâche.
func (k *keyedMutex) Lock(key string) func() {
	k.mu.Lock()
	l, ok := k.locks[key]
	if !ok {
		l = &refLock{}
		k.locks[key] = l
	}
	l.refs++
	k.mu.Unlock()

	l.Lock()
	return func() {
		l.Unlock()
		k.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}
