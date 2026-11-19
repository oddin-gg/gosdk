package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/oddin-gg/gosdk/types"
)

// Which failures say that the API answered for the key and failed.
func TestKeyFailure_Classification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"404", &Error{Status: http.StatusNotFound}, false},
		{"200 with NOT_FOUND", &Error{Status: http.StatusOK, Code: types.NotFoundResponseCode}, false},
		{"500", &Error{Status: http.StatusInternalServerError}, true},
		{"429", &Error{Status: http.StatusTooManyRequests}, true},
		{"wrapped 500", fmt.Errorf("gave up: %w", &Error{Status: http.StatusInternalServerError}), true},
		{"bad response", badResponse(fmt.Errorf("truncated")), true},
		{"wrapped bad response", fmt.Errorf("fetch: %w", badResponse(fmt.Errorf("truncated"))), true},
		{"canceled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"plain", fmt.Errorf("connection refused"), false},
	}
	for _, tc := range cases {
		if got := IsKeyFailure(tc.err); got != tc.want {
			t.Errorf("%s: IsKeyFailure = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A response that arrived complete is an answer about one resource, whatever
// its status or envelope says: it opens no breaker.
func TestBreaker_ACompleteAnswerOpensNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"service unavailable envelope on 500", http.StatusInternalServerError, `<response response_code="SERVICE_UNAVAILABLE"/>`},
		{"plain 500", http.StatusInternalServerError, `<error/>`},
		{"502", http.StatusBadGateway, `<error/>`},
		{"503", http.StatusServiceUnavailable, `<error/>`},
		{"504", http.StatusGatewayTimeout, `<error/>`},
		{"400 with service unavailable envelope", http.StatusBadRequest, `<response response_code="SERVICE_UNAVAILABLE"/>`},
		{"403 with service unavailable envelope", http.StatusForbidden, `<response response_code="SERVICE_UNAVAILABLE"/>`},
		{"400 with another code", http.StatusBadRequest, `<response response_code="BAD_REQUEST"/>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newBodyServer(tc.body, tc.status)
			defer srv.Close()
			c := newTestClient(t, srv)
			c.maxAttempts = 1
			if _, err := c.FetchSports(t.Context(), types.EnLocale); err == nil {
				t.Fatal("expected an error")
			}
			if anyOpen(c.Breaker()) {
				t.Fatal("a complete answer opened a breaker")
			}
		})
	}
}

func newBodyServer(body string, status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
}
