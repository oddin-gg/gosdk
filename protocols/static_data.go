package protocols

// StaticData is a primary-locale static-catalog entry (one row of a
// catalog like match-status descriptions or void reasons).
//
// Phase 6 reshape: replaces the previous StaticData interface with a
// value struct.
type StaticData struct {
	ID          uint
	Description *string
}

// GetID returns the entry id.
func (s StaticData) GetID() uint { return s.ID }

// GetDescription returns the primary-locale description, or nil if unset.
func (s StaticData) GetDescription() *string { return s.Description }

// LocalizedStaticData is a multi-locale static-catalog entry. Description
// carries the entry's primary locale; Descriptions covers every locale
// that was loaded.
type LocalizedStaticData struct {
	ID           uint
	Description  *string
	Descriptions map[Locale]string
}

// GetID returns the entry id.
func (l LocalizedStaticData) GetID() uint { return l.ID }

// GetDescription returns the primary-locale description, or nil if unset.
func (l LocalizedStaticData) GetDescription() *string { return l.Description }

// LocalizedDescription returns the description for a locale, or nil if
// the locale wasn't loaded.
func (l LocalizedStaticData) LocalizedDescription(locale Locale) *string {
	if v, ok := l.Descriptions[locale]; ok {
		return &v
	}
	return nil
}
