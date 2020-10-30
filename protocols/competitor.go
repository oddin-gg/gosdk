package protocols

// Player ...
type Player interface {
	ID() URN
	RefID() (*URN, error)
	Names() (map[Locale]string, error)
	LocalizedName(locale Locale) (*string, error)
	IconPath() (*string, error)
}

// Competitor ...
type Competitor interface {
	Player
	Abbreviations() (map[Locale]string, error)
	LocalizedAbbreviation(locale Locale) (*string, error)
}

// TeamCompetitor ...
type TeamCompetitor interface {
	Competitor
	Qualifier() *string
}
