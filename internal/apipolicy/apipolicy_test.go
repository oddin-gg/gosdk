package apipolicy

import (
	"context"
	"testing"
)

func TestModeOfDefaultsAndCarry(t *testing.T) {
	ctx := context.Background()
	if ModeOf(ctx) != Default || Suffix(ctx) != "" {
		t.Fatal("a plain context must carry the default policy and an empty suffix")
	}
	msg := With(ctx, Message)
	if ModeOf(msg) != Message || Suffix(msg) != "|msg" {
		t.Fatalf("Message: mode=%v suffix=%q", ModeOf(msg), Suffix(msg))
	}
	skip := With(ctx, MessageSkip)
	if ModeOf(skip) != MessageSkip || Suffix(skip) != "|skip" {
		t.Fatalf("MessageSkip: mode=%v suffix=%q", ModeOf(skip), Suffix(skip))
	}

	// Carry moves the policy onto an unrelated (detached) context, and
	// leaves a default caller's destination untouched.
	detached := context.Background()
	if got := ModeOf(Carry(detached, msg)); got != Message {
		t.Fatalf("Carry did not move the policy: %v", got)
	}
	if got := Carry(detached, ctx); got != detached {
		t.Fatal("Carry from a default context must return dst unchanged")
	}

	// Every suffix is listed for invalidation.
	seen := map[string]bool{}
	for _, s := range Suffixes {
		seen[s] = true
	}
	for _, c := range []context.Context{ctx, msg, skip} {
		if !seen[Suffix(c)] {
			t.Fatalf("suffix %q missing from Suffixes", Suffix(c))
		}
	}
}

type stubGate struct{}

func (stubGate) Open() bool              { return true }
func (stubGate) Opened() <-chan struct{} { return nil }

func TestGateMark(t *testing.T) {
	ctx := context.Background()
	if GateOf(ctx) != nil {
		t.Fatal("a plain context carries a gate")
	}
	marked := WithMessage(ctx, stubGate{})
	if GateOf(marked) == nil {
		t.Fatal("the mark was lost")
	}
	if GateOf(WithoutGate(marked)) != nil {
		t.Fatal("WithoutGate left the mark in place")
	}
	// The mark does not touch the call policy of the same context.
	if ModeOf(marked) != Default || Suffix(marked) != "" {
		t.Fatal("the gate mark changed the call policy")
	}
}
