package factory

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/apipolicy"
	"github.com/oddin-gg/gosdk/internal/cache"
	"github.com/oddin-gg/gosdk/internal/sdkerr"
	"github.com/oddin-gg/gosdk/types"
)

// reportNameFailure records only what says the API is unavailable: a
// server-side failure or a rate limit. An answer from a healthy API (4xx),
// a skipped request, shutdown and an absent entity say nothing about it.
func TestReportNameFailure_Classification(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		record bool
	}{
		{"429 rate limited", &api.Error{Status: 429}, true},
		{"503 unavailable", &api.Error{Status: 503}, true},
		{"500, wrapped", fmt.Errorf("fetch market description: %w", &api.Error{Status: 500}), true},
		{"transport failure", errors.New("connection refused"), true},
		{"404 not found", &api.Error{Status: 404}, false},
		{"200 envelope not found", &api.Error{Status: 200, Code: types.NotFoundResponseCode}, false},
		{"200 envelope forbidden", &api.Error{Status: 200, Code: types.ForbiddenResponseCode}, false},
		{"200 envelope service unavailable", &api.Error{Status: 200, Code: types.ServiceUnavailableResponseCode}, true},
		{"404, wrapped", fmt.Errorf("fetch market description: %w", &api.Error{Status: 404}), false},
		{"context canceled, wrapped", fmt.Errorf("lookup: %w", context.Canceled), false},
		{"client closed", sdkerr.ErrClosed, false},
		{"request skipped", apipolicy.ErrSkipped, false},
		{"budget spent before sending, wrapped", fmt.Errorf("fetch: %w: %w", apipolicy.ErrBudgetSpent, context.DeadlineExceeded), false},
		{"timed out after sending", fmt.Errorf("fetch: %w", context.DeadlineExceeded), true},
		{"entity not found in cache", cache.ErrItemNotFoundInCache, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, sink := withNameFailures(t.Context())
			reportNameFailure(ctx, tc.err)
			if got := sink.first() != nil; got != tc.record {
				t.Fatalf("recorded = %v, want %v", got, tc.record)
			}
		})
	}

	// Without a sink on the context reporting is a no-op, and the first
	// recorded failure is the one kept.
	reportNameFailure(t.Context(), &api.Error{Status: 503})
	ctx, sink := withNameFailures(t.Context())
	first, second := &api.Error{Status: 500}, &api.Error{Status: 503}
	reportNameFailure(ctx, first)
	reportNameFailure(ctx, second)
	if !errors.Is(sink.first(), first) {
		t.Fatalf("sink kept %v, want the first failure", sink.first())
	}
}
