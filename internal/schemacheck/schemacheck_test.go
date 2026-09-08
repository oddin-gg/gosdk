package schemacheck

import (
	"path/filepath"
	"reflect"
	"testing"

	apiXML "github.com/oddin-gg/gosdk/internal/api/xml"
	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
)

// wire is one side of the protocol: the schema directories (relative to
// the oddsfeedschema root) that define it, the Go type behind every root
// element, and the acknowledged drift.
type wire struct {
	name   string
	dirs   []string
	roots  map[string]reflect.Type
	ledger []ledgerEntry
}

// feedRoots maps every AMQP root element the schema declares to the Go
// type feedXML.Decode produces for it.
var feedRoots = map[string]reflect.Type{
	"alive":                   reflect.TypeFor[feedXML.Alive](),
	"bet_cancel":              reflect.TypeFor[feedXML.BetCancel](),
	"bet_settlement":          reflect.TypeFor[feedXML.BetSettlement](),
	"bet_stop":                reflect.TypeFor[feedXML.BetStop](),
	"fixture_change":          reflect.TypeFor[feedXML.FixtureChange](),
	"odds_change":             reflect.TypeFor[feedXML.OddsChange](),
	"rollback_bet_cancel":     reflect.TypeFor[feedXML.RollbackBetCancel](),
	"rollback_bet_settlement": reflect.TypeFor[feedXML.RollbackBetSettlement](),
	"snapshot_complete":       reflect.TypeFor[feedXML.SnapshotComplete](),
}

// restRoots maps every REST root element the schema declares to the Go
// type the API client decodes it into. A nil entry records an endpoint
// the SDK does not call, so there is nothing to compare.
var restRoots = map[string]reflect.Type{
	"bookmaker_details":         reflect.TypeFor[apiXML.WhoAMI](),
	"competitor_profile":        reflect.TypeFor[apiXML.CompetitorResponse](),
	"fixture_changes":           reflect.TypeFor[apiXML.FixtureChangesResponse](),
	"fixtures_fixture":          reflect.TypeFor[apiXML.FixtureResponse](),
	"market_descriptions":       reflect.TypeFor[apiXML.MarketDescriptionResponse](),
	"match_status_descriptions": reflect.TypeFor[apiXML.MatchStatusDescriptionResponse](),
	"match_summary":             reflect.TypeFor[apiXML.MatchSummaryResponse](),
	"player_profile":            reflect.TypeFor[apiXML.PlayerProfile](),
	"player_status":             reflect.TypeFor[apiXML.ReplayStatusResponse](),
	"producers":                 reflect.TypeFor[apiXML.ProducersResponse](),
	"replay_set_content":        reflect.TypeFor[apiXML.ReplayResponse](),
	"response":                  reflect.TypeFor[apiXML.Error](),
	"schedule":                  reflect.TypeFor[apiXML.ScheduleResponse](),
	"sport_tournaments":         reflect.TypeFor[apiXML.SportTournamentsResponse](),
	"sports":                    reflect.TypeFor[apiXML.SportsResponse](),
	"tournament_info":           reflect.TypeFor[apiXML.SportTournamentInfoResponse](),
	"tournament_schedule":       nil, // endpoint not called by the SDK
	"tournaments":               reflect.TypeFor[apiXML.TournamentsResponse](),
	"void_reasons":              reflect.TypeFor[apiXML.MarketVoidReasonsResponse](),
}

// The drift ledgers. Every entry is a deviation between the schema and
// the Go models that is known and tolerated, with the reason. The test
// fails on a deviation that is NOT listed here (new drift) and on an
// entry that no longer matches anything (fixed drift — delete the line),
// so this list is always exactly the current state.
//
// Path syntax: "/root/child@attr" for attributes, "/root/child" for
// elements; a leading "**" matches by suffix wherever the element is
// nested (one line for a shared Go type used in many places).
//
// Two reasons recur:
//
//   - "legacy wire field": ref_id / event_ref_id / sport_event_ref_id /
//     extended_specifiers. Betradar-heritage attributes the Go models
//     still decode but no Oddin producer sends (oddsfeedschema declares
//     what the producers actually emit). Candidates for removal from
//     the models; harmless until then.
//   - "shared Go type": one Go struct serves several schema types
//     (feedXML.Outcome for odds AND settlement outcomes, MarketWithOutcome
//     for odds AND settlement markets, apiXML.Sport for sport AND
//     sportExtended), so each context sees the union of fields.
var feedLedger = []ledgerEntry{
	{"/bet_cancel/market@void_reason", "schema declares the pre-void_reason_id spelling; the SDK reads void_reason_id only"},
	{"**@event_ref_id", "legacy wire field"},
	{"**/market@ref_id", "legacy wire field"},
	{"**/market@extended_specifiers", "legacy wire field"},
	{"**/outcome@ref_id", "legacy wire field"},
	{"/bet_settlement/outcomes/market/outcome@active", "shared Go type feedXML.Outcome (odds attributes on a settlement outcome)"},
	{"/bet_settlement/outcomes/market/outcome@odds", "shared Go type feedXML.Outcome"},
	{"/bet_settlement/outcomes/market/outcome@probabilities", "shared Go type feedXML.Outcome"},
	{"/odds_change/odds/market/outcome@result", "shared Go type feedXML.Outcome (settlement attributes on an odds outcome)"},
	{"/odds_change/odds/market/outcome@void_factor", "shared Go type feedXML.Outcome"},
	{"/bet_settlement/outcomes/market@favourite", "shared Go type feedXML.MarketWithOutcome (odds attributes on a settlement market)"},
	{"/bet_settlement/outcomes/market@status", "shared Go type feedXML.MarketWithOutcome"},
	{"**/outcomes/market@void_reason_id", "shared Go type feedXML.MarketWithOutcome (cancel attributes on a settlement market)"},
	{"**/outcomes/market@void_reason_params", "shared Go type feedXML.MarketWithOutcome"},
	{"/odds_change/odds/market@void_reason_id", "shared Go type feedXML.MarketWithOutcome (cancel attributes on an odds market)"},
	{"/odds_change/odds/market@void_reason_params", "shared Go type feedXML.MarketWithOutcome"},
	{"/odds_change/sport_event_status/statistics", "SDK decodes <statistics> (yellow/red cards, corners) into types.Statistics; the schema does not declare it — schema owners to confirm whether any producer emits it"},
}

var restLedger = []ledgerEntry{
	// Declared by the schema, ignored by the SDK.
	{"**/sport_event@type", "producer sends it; the SDK has no API surface for it yet"},
	{"**/sport_event@start_time_tbd", "producer sends it; the SDK has no API surface for it yet"},
	{"**/fixture@type", "producer sends it; the SDK has no API surface for it yet"},
	{"**/fixture@start_time_tbd", "producer sends it; the SDK has no API surface for it yet"},
	{"/match_summary/sport_event_status@status_code", "declared Betradar heritage, never emitted (schema comment); the SDK ignores it"},
	{"/match_summary/sport_event_status@aggregate_home_score", "declared Betradar heritage, never emitted; the SDK ignores it"},
	{"/match_summary/sport_event_status@aggregate_away_score", "declared Betradar heritage, never emitted; the SDK ignores it"},
	{"/match_summary/sport_event_status@aggregate_winner_id", "declared Betradar heritage, never emitted; the SDK ignores it"},
	{"/player_profile@generated_at", "the SDK does not read the generation time of a player profile"},
	// Decoded by the SDK, not declared by the schema.
	{"**@ref_id", "legacy wire field"},
	{"/fixture_changes/fixture_change@sport_event_ref_id", "legacy wire field"},
	{"**/sport@icon_path", "shared Go type apiXML.Sport (sportExtended's icon_path on every plain <sport>)"},
	{"**/tournament/category", "SDK decodes <category> under <tournament> and exposes it via the tournament cache; the schema does not declare it — schema owners to confirm"},
	{"**/competitor/category", "SDK decodes <category> under a competitor profile; the schema does not declare it — schema owners to confirm"},
	{"**/reference_ids", "SDK decodes <reference_ids> on sport events and tournaments (match cache keys off them); the schema does not declare it — schema owners to confirm"},
}

var wires = []wire{
	{"feed", []string{"schema/feed", "schema/common"}, feedRoots, feedLedger},
	{"rest", []string{"schema/rest", "schema/common"}, restRoots, restLedger},
}

// TestSchemaCoverage compares every root element of each wire against
// its Go model and fails on drift the ledger does not acknowledge, and
// on ledger entries the code has outgrown.
func TestSchemaCoverage(t *testing.T) {
	for _, w := range wires {
		t.Run(w.name, func(t *testing.T) {
			root := schemaRoot(t)
			dirs := make([]string, len(w.dirs))
			for i, d := range w.dirs {
				dirs[i] = filepath.Join(root, d)
			}
			set, err := loadXSDs(dirs...)
			if err != nil {
				t.Fatal(err)
			}
			var all []finding
			for _, root := range set.rootNames() {
				gt, mapped := w.roots[root]
				if !mapped {
					t.Errorf("schema root %q has no Go mapping — add it to %sRoots (nil if the SDK does not use the endpoint)", root, w.name)
					continue
				}
				if gt == nil {
					continue
				}
				xs, err := set.rootShape(root)
				if err != nil {
					t.Fatalf("%s: %v", root, err)
				}
				gs, err := goShape(gt)
				if err != nil {
					t.Fatalf("%s: %v", root, err)
				}
				all = append(all, compare("/"+root, xs, gs)...)
			}
			for root := range w.roots {
				if _, ok := set.roots[root]; !ok {
					t.Errorf("Go mapping for %q but the schema declares no such root", root)
				}
			}
			unexpected, stale := reconcile(all, w.ledger)
			if len(unexpected) > 0 {
				t.Errorf("%d deviation(s) between the schema and the Go models not in the ledger:%s\n\nA schema-only slot means the SDK drops data the producer sends: add the field, or add a ledger entry with the reason. An sdk-only slot means the model decodes something the schema does not declare: fix the schema (oddsfeedschema), remove the field, or add a ledger entry.", len(unexpected), describe(unexpected))
			}
			for _, s := range stale {
				t.Errorf("ledger entry %q no longer matches any deviation (%s) — delete it", s.path, s.reason)
			}
		})
	}
}
