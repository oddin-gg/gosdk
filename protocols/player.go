package protocols

type Player interface {
	ID() string
	LocalizedName() string
	FullName() string
	SportID() string
}
