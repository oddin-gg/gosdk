package types

// ReplayPlayParams carries the optional knobs for the replay
// manager's Play call (surfaced as gosdk.Replay's Start options).
//
// v2.29 reshape: every field migrated from *T to Optional[T] for
// value semantics. Construct via the WithReplay* options in the
// gosdk package (e.g. gosdk.WithReplaySpeed(5)) or directly:
//
//	p := types.ReplayPlayParams{Speed: types.Some(5)}
//
// The behavioural surface (formerly types.ReplayManager) lives in
// the gosdk root package as of v2.25 (unexported since v1.0.0);
// types/ is data-shape-only.
type ReplayPlayParams struct {
	Speed             Optional[int]
	MaxDelayInMs      Optional[int]
	RunParallel       Optional[bool]
	RewriteTimestamps Optional[bool]
	Producer          Optional[string]
}
