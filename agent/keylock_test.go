package agent

import (
	"sync"
	"testing"
)

// TestKeyedMutexSerializesSameKey verifies two goroutines locking the same
// key never run their critical section concurrently.
func TestKeyedMutexSerializesSameKey(t *testing.T) {
	k := newKeyedMutex()
	counter := 0
	var wg sync.WaitGroup

	const n = 200
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := k.Lock("same-conversation")
			defer unlock()
			// Non-atomic read-modify-write: would race if the lock didn't hold.
			current := counter
			counter = current + 1
		}()
	}
	wg.Wait()

	if counter != n {
		t.Fatalf("expected counter=%d, got %d (lost updates -> lock did not serialize)", n, counter)
	}
}

// TestKeyedMutexAllowsDifferentKeysInParallel verifies different keys don't
// block each other.
func TestKeyedMutexAllowsDifferentKeysInParallel(t *testing.T) {
	k := newKeyedMutex()
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		key := string(rune('a' + i%26))
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			unlock := k.Lock(key)
			defer unlock()
		}(key)
	}
	wg.Wait()
}
