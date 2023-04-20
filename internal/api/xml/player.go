package xml

// Player represents player info
type Player struct {
	ID       string `xml:"id,attr"`
	Name     string `xml:"name,attr"`
	FullName string `xml:"full_name,attr"`
}
