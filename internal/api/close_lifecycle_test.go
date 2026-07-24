package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestClient_Close_RejectsNewRequests pins that after Close, request
// methods return ErrClosed instead of issuing new HTTP calls.
func TestClient_Close_RejectsNewRequests(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0"?><producers response_code="OK"/>`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	c.Close()

	if _, err := c.FetchProducers(t.Context()); !errors.Is(err, ErrClosed) {
		t.Fatalf("FetchProducers after Close = %v, want ErrClosed", err)
	}
	if hits != 0 {
		t.Fatalf("server received %d requests after Close, want 0", hits)
	}
}

// blockingTransport ignores ctx cancellation entirely — RoundTrip parks
// on release no matter what — modelling a misbehaving custom transport.
type blockingTransport struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	close(b.entered)
	<-b.release
	return nil, errors.New("released")
}

// TestClient_CloseCtx_BoundedByCtxWithStuckTransport pins the M3 fix:
// CloseCtx must return (reporting false) when the in-flight join can't
// complete because a custom transport ignores cancellation — instead of
// hanging the caller's shutdown budget on inflight.Wait().
func TestClient_CloseCtx_BoundedByCtxWithStuckTransport(t *testing.T) {
	bt := &blockingTransport{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(bt.release)

	c := New(&testConfig{apiURL: "example.invalid", token: "tok"})
	c.SetHTTPClient(&http.Client{Transport: bt, Timeout: time.Hour})

	go func() {
		_, _ = c.FetchProducers(context.Background())
	}()
	select {
	case <-bt.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("request never reached the transport")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if c.CloseCtx(ctx) {
		t.Fatal("CloseCtx reported completion while the transport is stuck")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("CloseCtx blocked %v despite the ctx bound", el)
	}
}

// TestClient_Close_CancelsInflightAndJoins pins that Close cancels an
// in-flight request via the client lifetime — even one launched with a
// context that never fires on its own (context.Background()) — and joins
// it before returning.
func TestClient_Close_CancelsInflightAndJoins(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release // block until released (or the client aborts the request)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0"?><producers response_code="OK"/>`))
	}))
	defer srv.Close()
	defer close(release)

	c := newTestClient(t, srv)

	reqErr := make(chan error, 1)
	go func() {
		// context.Background(): only the client lifetime can abort this.
		_, err := c.FetchProducers(context.Background())
		reqErr <- err
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("request never reached the server")
	}

	closed := make(chan struct{})
	go func() { c.Close(); close(closed) }()

	select {
	case err := <-reqErr:
		if err == nil {
			t.Fatal("in-flight request returned nil; expected cancellation via client lifetime")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight request was not cancelled by Close")
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return (join hung)")
	}
}
