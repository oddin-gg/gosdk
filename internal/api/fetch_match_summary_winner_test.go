package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oddin-gg/gosdk/types"
)

// TestFetchMatchSummary_MalformedWinnerIsValidationError is the regression
// for the P2 finding: a present-but-malformed winner_id used to fail only
// inside the match-status observer, which dropped the response and left
// the MatchStatus loader reporting ErrItemNotFound (definitive, non-
// retryable absence). Validating it at the response boundary surfaces a
// descriptive fetch error instead. A valid (or absent) winner still
// succeeds. (The api layer never wraps the cache's not-found sentinel — a
// descriptive fetch error here is exactly what keeps the classification
// honest one layer up.)
func TestFetchMatchSummary_MalformedWinnerIsValidationError(t *testing.T) {
	matchID := types.URN{Prefix: "od", Type: "match", ID: 1}

	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{
			name:    "malformed winner_id",
			body:    `<match_summary><sport_event id="od:match:1"/><sport_event_status status="live" winner_id="not-a-urn"/></match_summary>`,
			wantErr: true,
		},
		{
			name:    "valid winner_id",
			body:    `<match_summary><sport_event id="od:match:1"/><sport_event_status status="ended" winner_id="od:competitor:5"/></match_summary>`,
			wantErr: false,
		},
		{
			name:    "absent winner_id",
			body:    `<match_summary><sport_event id="od:match:1"/><sport_event_status status="live"/></match_summary>`,
			wantErr: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/xml")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			c := newTestClient(t, srv)
			_, err := c.FetchMatchSummary(t.Context(), matchID, types.Locale("en"))
			if tc.wantErr != (err != nil) {
				t.Fatalf("FetchMatchSummary err = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "winner_id") {
				t.Fatalf("malformed-winner error should name winner_id, got %v", err)
			}
		})
	}
}
