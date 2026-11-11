package apipolicy

import (
	"context"
	"testing"
)

type otherKey struct{}

type stubGate struct{ open bool }

func (g stubGate) Open() bool            { return g.open }
func (stubGate) Opened() <-chan struct{} { return nil }

func TestGateMark(t *testing.T) {
	ctx := context.Background()
	if GateOf(ctx) != nil {
		t.Fatal("a plain context carries a gate")
	}
	marked := WithMessage(ctx, stubGate{open: true})
	g := GateOf(marked)
	if g == nil || !g.Open() {
		t.Fatalf("the mark was lost or changed: %v", g)
	}
	if GateOf(WithoutGate(marked)) != nil {
		t.Fatal("WithoutGate left the mark in place")
	}
}

// The mark follows the context into everything derived from it, and a
// context derived from a stripped one does not get it back.
func TestGateMarkIsInherited(t *testing.T) {
	marked := WithMessage(context.Background(), stubGate{})
	child, cancel := context.WithCancel(marked)
	defer cancel()
	if GateOf(child) == nil {
		t.Fatal("a derived context lost the mark")
	}
	if GateOf(context.WithoutCancel(marked)) == nil {
		t.Fatal("a context detached from the caller's cancellation lost the mark; WithoutGate is the way to drop it")
	}
	if GateOf(context.WithValue(WithoutGate(marked), otherKey{}, 1)) != nil {
		t.Fatal("a context derived from a stripped one has the mark again")
	}
}
