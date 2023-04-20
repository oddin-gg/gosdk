package protocols

// Competitor ...
type Competitor interface {
	ID() URN
	// Deprecated: do not use this method, it will be removed in future
	RefID() (*URN, error)
	Names() (map[Locale]string, error)
	LocalizedName(locale Locale) (*string, error)
	IconPath() (*string, error)
	Abbreviations() (map[Locale]string, error)
	LocalizedAbbreviation(locale Locale) (*string, error)
}

// TeamCompetitor ...
type TeamCompetitor interface {
	Competitor
	Qualifier() *string
}
