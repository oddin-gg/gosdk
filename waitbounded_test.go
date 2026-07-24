package gosdk

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestWaitBounded pins the primitive that makes WithShutdownTimeout a
// TOTAL bound: every shutdown wait (subscription drains, broker-open
// fence, internal goroutines) goes through waitBounded, which must return
// when the deadline fires even if the WaitGroup never completes — and
// return promptly when it does.
func TestWaitBounded(t *testing.T) {
	// Wedged wait: wg never reaches zero → waitBounded returns FALSE on
	// the deadline (runShutdown folds that into the terminal close error).
	var wedged sync.WaitGroup
	wedged.Add(1)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if waitBounded(ctx, &wedged) {
		t.Fatal("waitBounded reported completion on a wedged WaitGroup")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("waitBounded blocked %v on a wedged WaitGroup — deadline not honoured", el)
	}

	// Completes before the deadline → returns TRUE promptly, not at ctx expiry.
	var quick sync.WaitGroup
	quick.Add(1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		quick.Done()
	}()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	start2 := time.Now()
	if !waitBounded(ctx2, &quick) {
		t.Fatal("waitBounded reported timeout on a completed WaitGroup")
	}
	if el := time.Since(start2); el > time.Second {
		t.Fatalf("waitBounded took %v to observe a completed WaitGroup — should be prompt", el)
	}
}
