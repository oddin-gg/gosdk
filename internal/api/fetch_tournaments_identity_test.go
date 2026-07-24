package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/oddin-gg/gosdk/types"
)

// TestFetchTournaments_TopLevelSportIdentity is the regression for the P2
// finding: FetchTournaments validated only nested tournament sport ids,
// which the loop never reaches for an EMPTY list — so a 2xx response for
// the WRONG sport (or one omitting the top-level <sport>) satisfied the
// request and was cached as this sport's authoritative empty list. The
// top-level <sport> identity is now required and validated. Covers
// wrong-id-empty-list, missing-id, and correct-empty-list.
func TestFetchTournaments_TopLevelSportIdentity(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{
			name:    "wrong top-level sport, empty list",
			body:    `<?xml version="1.0"?><sport_tournaments><sport id="od:sport:2" name="Other"/></sport_tournaments>`,
			wantErr: true,
		},
		{
			name:    "missing top-level sport",
			body:    `<?xml version="1.0"?><sport_tournaments/>`,
			wantErr: true,
		},
		{
			name:    "correct top-level sport, empty list",
			body:    `<?xml version="1.0"?><sport_tournaments><sport id="od:sport:1" name="Football"/></sport_tournaments>`,
			wantErr: false,
		},
		{
			name:    "correct top-level sport, populated list",
			body:    `<?xml version="1.0"?><sport_tournaments><sport id="od:sport:1" name="Football"/><tournaments><tournament id="od:tournament:9" name="T"><sport id="od:sport:1" name="Football"/></tournament></tournaments></sport_tournaments>`,
			wantErr: false,
		},
	}
	sportID := types.URN{Prefix: "od", Type: "sport", ID: 1}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/xml")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			c := newTestClient(t, srv)
			_, err := c.FetchTournaments(t.Context(), sportID, types.Locale("en"))
			if tc.wantErr != (err != nil) {
				t.Fatalf("FetchTournaments err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}
