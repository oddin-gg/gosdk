package protocols

// StaticData ...
type StaticData interface {
	GetID() uint
	GetDescription() *string
}

// LocalizedStaticData ...
type LocalizedStaticData interface {
	StaticData
	LocalizedDescription(locale Locale) *string
}
