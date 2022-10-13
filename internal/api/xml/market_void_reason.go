package xml

import (
	"encoding/xml"

	"github.com/oddin-gg/gosdk/protocols"
)

// MarketVoidReasonsResponse ...
type MarketVoidReasonsResponse struct {
	XMLName      xml.Name            `xml:"void_reasons"`
	ResponseCode string              `xml:"response_code,attr"`
	VoidReasons  []MarketVoidReasons `xml:"void_reason,omitempty"`
}

// Code ...
func (m MarketVoidReasonsResponse) Code() protocols.ResponseCode {
	return protocols.ResponseCode(m.ResponseCode)
}

// MarketVoidReasons represents market void reason type
type MarketVoidReasons struct {
	ID               uint                    `xml:"id,attr"`
	Name             string                  `xml:"name,attr"`
	Description      *string                 `xml:"description,attr,omitempty"`
	Template         *string                 `xml:"template,attr,omitempty"`
	VoidReasonParams []MarketVoidReasonParam `xml:"param,omitempty"`
}

// MarketVoidReasonParam ...
type MarketVoidReasonParam struct {
	Name string `xml:"name,attr"`
}
