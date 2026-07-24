package gosdk

import (
	"bytes"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// TestSession_RawEnvelope_BufferDecoupled is the regression for the
// aliased-payload finding: the channel consumer builds the raw envelope
// and the parsed message from the SAME d.Body slice, and
// types.RawFeedMessage exposes it as a public FIELD. Pre-fix, a
// consumer mutating raw.RawMessage tampered with the backing buffer the
// parsed message's RawMessage() accessor clones FROM (and could race
// its readers). The session must hand consumers a decoupled copy.
func TestSession_RawEnvelope_BufferDecoupled(t *testing.T) {
	backing := []byte(`<odds_change/>`) // stands in for d.Body, retained by the parsed message
	rawMsg := &types.RawFeedMessage{
		BasicFeedMessage: types.BasicFeedMessage{RawMessage: backing},
	}

	o := &oddsFeedSessionImpl{
		logger:    log.New(slog.New(slog.NewTextHandler(io.Discard, nil))),
		msgCh:     make(chan sessionEnvelope, 2),
		sessionID: uuid.New(),
	}

	// Path 1: raw side-channel (parsed envelope follows separately).
	o.sendRawSideChannel(t.Context(), rawMsg)
	env := <-o.msgCh
	got := env.msg.RawFeedMessage
	if got == nil {
		t.Fatal("raw envelope not emitted")
	}
	if !bytes.Equal(got.RawMessage, backing) {
		t.Fatalf("raw payload = %q, want %q", got.RawMessage, backing)
	}
	if &got.RawMessage[0] == &backing[0] {
		t.Fatal("raw envelope aliases the SDK backing buffer (side-channel path)")
	}
	// Consumer mutation must not reach the backing buffer.
	got.RawMessage[1] = 'X'
	if backing[1] == 'X' {
		t.Fatal("consumer mutation of raw envelope tampered with the SDK buffer")
	}

	// Path 2: dropDelivery (raw is the delivery's only output).
	acked := false
	o.dropDelivery(t.Context(), true, rawMsg, func() { acked = true })
	env = <-o.msgCh
	got = env.msg.RawFeedMessage
	if got == nil {
		t.Fatal("raw envelope not emitted on drop path")
	}
	if &got.RawMessage[0] == &backing[0] {
		t.Fatal("raw envelope aliases the SDK backing buffer (drop path)")
	}
	if env.ack == nil {
		t.Fatal("drop-path raw envelope must carry the delivery ack")
	}
	env.ack()
	if !acked {
		t.Fatal("ack closure did not fire")
	}
}
