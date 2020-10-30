package xml

import (
	"encoding/xml"
	"github.com/oddin-gg/gosdk/protocols"
)

// MQSubscriptionTypeName ...
type MQSubscriptionTypeName string

// List of MQSubscriptionTypeName
const (
	MQSubscriptionTypeNamePre  MQSubscriptionTypeName = "pre"
	MQSubscriptionTypeNameLive MQSubscriptionTypeName = "live"
)

// Scope ...
type Scope string

// List of Scope
const (
	ScopeLive     Scope = "live"
	ScopePrematch Scope = "prematch"
)

// ProducersResponse ...
type ProducersResponse struct {
	XMLName      xml.Name   `xml:"producers"`
	ResponseCode string     `xml:"response_code,attr"`
	Producers    []Producer `xml:"producer,omitempty"`
}

// Code ...
func (p ProducersResponse) Code() protocols.ResponseCode {
	return protocols.ResponseCode(p.ResponseCode)
}

// Producer ...
type Producer struct {
	ID             uint                   `xml:"id,attr"`
	Name           MQSubscriptionTypeName `xml:"name,attr"`
	Description    string                 `xml:"description,attr"`
	APIEndpoint    string                 `xml:"api_url,attr"`
	Active         bool                   `xml:"active,attr"`
	Scope          Scope                  `xml:"scope,attr"`
	RecoveryWindow uint                   `xml:"stateful_recovery_window_in_minutes,attr"`
}
