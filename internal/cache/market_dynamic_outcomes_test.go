package cache

import (
	"errors"
	"testing"

	"github.com/oddin-gg/gosdk/types"
)

// A dynamic-outcome market (outcome_type set) lists no static outcomes: its
// outcomes are player or competitor URNs carried by the odds. Such a row is
// complete and must be served, by id and in the bulk view.
func TestMarketDescriptionCache_DynamicOutcomeMarketWithoutOutcomesIsServed(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "empty outcomes container",
			body: `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="341" name="Anytime Goalscorer" includes_outcomes_of_type="cw:player" outcome_type="player"><outcomes/></market>
</market_descriptions>`,
		},
		{
			name: "no outcomes block at all",
			body: `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="341" name="Anytime Goalscorer" includes_outcomes_of_type="cw:player" outcome_type="player"/>
</market_descriptions>`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newMarketSrv(t, tt.body)
			mc, ctx := newMarketCacheForTest(t, srv)

			entry, err := mc.MarketDescriptionByID(ctx, 341, types.None[string](), []types.Locale{types.EnLocale})
			if err != nil {
				t.Fatalf("by-id lookup: %v", err)
			}
			snap := entry.Snapshot()
			if got, ok := snap.LocalizedName(types.EnLocale).Get(); !ok || got != "Anytime Goalscorer" {
				t.Errorf("name = %q (%v), want Anytime Goalscorer", got, ok)
			}
			if got, ok := snap.OutcomeType.Get(); !ok || got != "player" {
				t.Errorf("OutcomeType = %q (%v), want player", got, ok)
			}
			if got, ok := snap.IncludesOutcomesOfType.Get(); !ok || got != "cw:player" {
				t.Errorf("IncludesOutcomesOfType = %q (%v), want cw:player", got, ok)
			}
			if len(snap.Outcomes) != 0 {
				t.Errorf("outcomes = %d, want none", len(snap.Outcomes))
			}

			all, err := mc.LocalizedMarketDescriptions(ctx, types.EnLocale)
			if err != nil {
				t.Fatalf("bulk read: %v", err)
			}
			if _, ok := all[CompositeKey{MarketID: 341}]; !ok {
				t.Error("bulk view omits the dynamic-outcome market")
			}
		})
	}
}

// Without outcome_type the zero-outcome row stays what it was: incomplete.
func TestMarketDescriptionCache_ZeroOutcomesWithoutOutcomeTypeStaysIncomplete(t *testing.T) {
	srv := newMarketSrv(t, `<?xml version="1.0"?>
<market_descriptions response_code="OK">
  <market id="9" name="Plain"><outcomes/></market>
</market_descriptions>`)
	mc, ctx := newMarketCacheForTest(t, srv)

	if _, err := mc.MarketDescriptionByID(ctx, 9, types.None[string](), []types.Locale{types.EnLocale}); !errors.Is(err, ErrMarketLocaleIncomplete) {
		t.Fatalf("err = %v, want ErrMarketLocaleIncomplete", err)
	}
}
