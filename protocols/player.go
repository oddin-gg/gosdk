package protocols

type Player interface {
	ID() string
	LocalizedName() (string, error)
	FullName() (string, error)
	SportID() (string, error)
}
