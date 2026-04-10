package protocols

type UnderageStatus int

const (
	UnderageUnknown UnderageStatus = -1
	UnderageNo      UnderageStatus = 0
	UnderageYes     UnderageStatus = 1
)
