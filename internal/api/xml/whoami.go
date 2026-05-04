package xml

import (
	"encoding/xml"
	"github.com/oddin-gg/gosdk/internal/utils"
	"github.com/oddin-gg/gosdk/types"
)

// WhoAMI ...
type WhoAMI struct {
	XMLName      xml.Name       `xml:"bookmaker_details"`
	ResponseCode string         `xml:"response_code,attr"`
	ExpireAt     utils.DateTime `xml:"expire_at,attr"`
	BookmakerID  uint           `xml:"bookmaker_id,attr"`
	VirtualHost  string         `xml:"virtual_host,attr"`
}

// Code ...
func (w WhoAMI) Code() types.ResponseCode {
	return types.ResponseCode(w.ResponseCode)
}
