package xml

import "encoding/xml"

// Error ...
type Error struct {
	XMLName xml.Name `xml:"response"`
	Code    string   `xml:"response_code,attr"`
	Action  string   `xml:"action"`
	Message string   `xml:"message"`
}
