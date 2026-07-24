package types

// StaticData is a primary-locale static-catalog entry (one row of a
// catalog like match-status descriptions or void reasons).
//
// Phase 6 reshape: replaces the previous StaticData interface with a
// value struct.
//
// v2.29 reshape: Description migrated from *string to Optional[string].
type StaticData struct {
	ID          int
	Description Optional[string]
}

// GetID returns the entry id.
func (s StaticData) GetID() int { return s.ID }

// GetDescription returns the primary-locale description Optional.
func (s StaticData) GetDescription() Optional[string] { return s.Description }

// LocalizedStaticData is a multi-locale static-catalog entry. Description
// carries the entry's primary locale; Descriptions covers every locale
// that was loaded.
//
// v2.29 reshape: Description migrated from *string to Optional[string].
type LocalizedStaticData struct {
	ID           int
	Description  Optional[string]
	Descriptions map[Locale]string
}

// GetID returns the entry id.
func (l LocalizedStaticData) GetID() int { return l.ID }

// GetDescription returns the primary-locale description Optional.
func (l LocalizedStaticData) GetDescription() Optional[string] { return l.Description }

// LocalizedDescription returns the description for a locale, or
// None if the locale wasn't loaded.
func (l LocalizedStaticData) LocalizedDescription(locale Locale) Optional[string] {
	if v, ok := l.Descriptions[locale]; ok {
		return Some(v)
	}
	return None[string]()
}
