package cache

import (
	"context"

	"github.com/oddin-gg/gosdk/types"
)

// entityFactory is the consumer-side view of *factory.EntityFactory:
// only the build methods that the per-entity cache layers call to
// resolve cross-entity references during BuildX. Declared here at the
// point of use so the cache package doesn't depend on internal/factory
// (which itself depends on cache, building entities from cache data).
//
// *factory.EntityFactory satisfies this interface implicitly via
// structural typing. Pre-v2.32 the same contract lived in
// types.EntityFactory — relocated to internal because the contract is
// purely internal-facing (consumers never implement it).
type entityFactory interface {
	BuildTournament(ctx context.Context, id types.URN, sportID types.URN, locales []types.Locale) (*types.Tournament, error)
	BuildSport(ctx context.Context, id types.URN, locales []types.Locale) (*types.Sport, error)
	BuildCompetitor(ctx context.Context, id types.URN, locales []types.Locale) (*types.Competitor, error)
	BuildTeamCompetitor(ctx context.Context, id types.URN, qualifier *string, locales []types.Locale) (*types.TeamCompetitor, error)
	BuildPlayer(ctx context.Context, id types.URN, locale types.Locale) (*types.Player, error)
	BuildFixture(ctx context.Context, id types.URN, locale types.Locale) (*types.Fixture, error)
	BuildMatchStatus(ctx context.Context, id types.URN, locales []types.Locale) (*types.MatchStatus, error)
}

// idMessage is the consumer-side view of "anything carrying an event
// id" — used by Manager.OnFeedMessageReceived to route incoming feed
// messages into the per-event cache invalidation hooks. Pre-v2.32 the
// same contract lived in types.IDMessage; relocated to internal
// because the interface is satisfied by `internal/feed/xml` decoded
// types and consumed only inside the cache, never crossing the
// public surface.
type idMessage interface {
	GetEventID() string
}
