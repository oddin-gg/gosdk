package xml

import (
	"encoding/xml"
	"github.com/oddin-gg/gosdk/internal/utils"
)

// SportsResponse create default response
type SportsResponse struct {
	XMLName     xml.Name       `xml:"sports"`
	GeneratedAt utils.DateTime `xml:"generated_at,attr"`
	Sports      []Sport        `xml:"sport"`
}
