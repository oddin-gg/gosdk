package recovery

import (
	"math/rand"
	"sync"
	"time"
)

type generator struct {
	value     uint32
	increment uint32
	mux       sync.Mutex
}

func (g *generator) next() uint {
	g.mux.Lock()
	defer g.mux.Unlock()

	g.value = g.value + g.increment
	return uint(g.value)
}

func newGenerator(increment uint32) *generator {
	return &generator{
		value:     rand.New(rand.NewSource(time.Now().UnixNano())).Uint32(),
		increment: increment,
	}
}
