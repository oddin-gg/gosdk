package types

// Competitor is a pure-data snapshot of a competitor profile, populated
// across one or more locales.
//
// Phase 6 reshape: replaces the previous Competitor interface (with
// (value, error) lazy accessors) with a value struct populated at
// construction. Names and Abbreviations cover every locale that was
// loaded; Players is keyed by locale.
type Competitor struct {
	ID            URN
	Names         map[Locale]string
	Abbreviations map[Locale]string
	IconPath      *string
	Underage      UnderageStatus
	Players       map[Locale][]Player
}

// Name returns the localized name for the locale, or "" if the
// competitor wasn't loaded for that locale.
func (c Competitor) Name(locale Locale) string { return c.Names[locale] }

// Abbreviation returns the localized abbreviation, or "" if not loaded.
func (c Competitor) Abbreviation(locale Locale) string { return c.Abbreviations[locale] }

// PlayersFor returns the player list in the given locale, or nil.
func (c Competitor) PlayersFor(locale Locale) []Player { return c.Players[locale] }

// TeamCompetitor extends Competitor with a side qualifier ("home"/"away")
// for matches.
type TeamCompetitor struct {
	Competitor
	Qualifier *string
}
