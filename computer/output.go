package computer

import (
	"fmt"
	"sync"
)

// Output est un io.Writer qui garde le début et la fin d'une sortie, sans
// jamais dépasser sa taille : une commande bavarde ne remplit pas la mémoire.
type Output struct {
	mu    sync.Mutex
	head  []byte
	tail  []byte // tampon circulaire
	pos   int
	total int64
	h, t  int
}

// NewOutput crée un Output qui garde au plus max octets.
func NewOutput(max int) *Output {
	if max < 2 {
		max = 2
	}
	return &Output{h: max / 2, t: max - max/2}
}

func (o *Output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := len(p)
	o.total += int64(n)
	if room := o.h - len(o.head); room > 0 {
		k := min(room, len(p))
		o.head = append(o.head, p[:k]...)
		p = p[k:]
	}
	for len(p) > 0 {
		if len(o.tail) < o.t {
			k := min(o.t-len(o.tail), len(p))
			o.tail = append(o.tail, p[:k]...)
			p = p[k:]
			continue
		}
		k := min(o.t-o.pos, len(p))
		copy(o.tail[o.pos:], p[:k])
		o.pos = (o.pos + k) % o.t
		p = p[k:]
	}
	return n, nil
}

// Bytes renvoie la sortie gardée et indique si une partie a été omise.
func (o *Output) Bytes() ([]byte, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := append([]byte{}, o.head...)
	ordered := append(append([]byte{}, o.tail[o.pos:]...), o.tail[:o.pos]...)
	kept := int64(len(o.head) + len(ordered))
	if o.total > kept {
		out = append(out, fmt.Sprintf("\n[… %d octets omis …]\n", o.total-kept)...)
	}
	return append(out, ordered...), o.total > kept
}
