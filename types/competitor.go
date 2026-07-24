package types

// Competitor is a pure-data snapshot of a competitor profile, populated
// across one or more locales.
//
// Phase 6 reshape: replaces the previous Competitor interface (with
// (value, error) lazy accessors) with a value struct populated at
// construction. Names and Abbreviations cover every locale that was
// loaded; Players is keyed by locale.
//
// v2.28 reshape: IconPath migrated from *string to Optional[string].
//
// v2.x reshape: locale-keyed Name / Abbreviation accessors migrated
// from `string` (silent "" on miss) to `Optional[string]` so callers
// can distinguish "loaded but empty" from "locale not loaded". Use
// `.ValueOr("")` for the previous always-string semantics, or
// `.Get()` to detect the not-loaded case explicitly.
type Competitor struct {
	ID            URN
	Names         map[Locale]string
	Abbreviations map[Locale]string
	IconPath      Optional[string]
	Underage      UnderageStatus
	Players       map[Locale][]Player
}

// Name returns the localized name for the locale, or None if the
// competitor wasn't loaded for that locale.
func (c Competitor) Name(locale Locale) Optional[string] {
	if v, ok := c.Names[locale]; ok {
		return Some(v)
	}
	return None[string]()
}

// Abbreviation returns the localized abbreviation, or None if not loaded.
func (c Competitor) Abbreviation(locale Locale) Optional[string] {
	if v, ok := c.Abbreviations[locale]; ok {
		return Some(v)
	}
	return None[string]()
}

// PlayersFor returns the player list in the given locale, or nil.
func (c Competitor) PlayersFor(locale Locale) []Player { return c.Players[locale] }

// TeamCompetitor extends Competitor with a side qualifier ("home"/"away")
// for matches.
//
// v2.28 reshape: Qualifier migrated from *string to Optional[string].
type TeamCompetitor struct {
	Competitor
	Qualifier Optional[string]
}
