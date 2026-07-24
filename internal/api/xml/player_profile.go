package xml

import "encoding/xml"

// PlayerProfile represents sport/{language}/player/{id}/profiles response.
//
// XMLName constrains the document ROOT: without it, ANY well-formed 2xx
// XML document decoded "successfully" into a zero-valued profile —
// which the cache then stored as a real (empty) player under the
// requested key.
type PlayerProfile struct {
	XMLName xml.Name `xml:"player_profile"`
	Player  Player   `xml:"player"`
}
