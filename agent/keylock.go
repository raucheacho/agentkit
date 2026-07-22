package agent

import "sync"

// keyedMutex serializes work for a given key while letting different keys
// run fully in parallel. Used so that two messages for the same WhatsApp
// conversation are always processed in order, while different conversations
// keep running concurrently.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func newKeyedMutex() *keyedMutex {
	return &keyedMutex{locks: make(map[string]*sync.Mutex)}
}

// Lock acquires the lock for key and returns a function to release it.
func (k *keyedMutex) Lock(key string) func() {
	k.mu.Lock()
	l, ok := k.locks[key]
	if !ok {
		l = &sync.Mutex{}
		k.locks[key] = l
	}
	k.mu.Unlock()

	l.Lock()
	return l.Unlock
}
