package xml

import (
	"encoding/xml"
	"github.com/oddin-gg/gosdk/internal/utils"
)

// ScheduleResponse ...
type ScheduleResponse struct {
	XMLName     xml.Name       `xml:"schedule"`
	GeneratedAt utils.DateTime `xml:"generated_at,attr"`
	SportEvents []SportEvent   `xml:"sport_event"`
}
