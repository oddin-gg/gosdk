package protocols

type Player interface {
	ID() (*string, error)
	LocalizedName() (*string, error)
	FullName() (*string, error)
	SportID() (*string, error)
}
