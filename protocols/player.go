package protocols

// Player is a pure-data snapshot of a player profile in one locale.
//
// Phase 6 reshape: replaces the previous Player interface (with lazy-load
// accessors that returned (value, error)) with a value struct populated
// at construction time. Field access is allocation-free and never errors
// — the data is already in memory by the time you hold a Player.
//
// Every Player carries the Locale it was loaded for (Locale field). To
// observe a different locale, request the player from the SDK with that
// locale.
type Player struct {
	ID       string
	Name     string
	FullName string
	SportID  string
	Locale   Locale
}
