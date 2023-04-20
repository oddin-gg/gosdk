package xml

// PlayerProfile represents sport/{language}/player/{id}/profiles response
type PlayerProfile struct {
	Player Player `xml:"player"`
}
