package protocols

import (
	"net/url"
)

// Response ...
type Response struct {
	Data   interface{}
	URL    *url.URL
	Locale *Locale
}

// ResponseCode ...
type ResponseCode string

// ResponseCodes
const (
	OkResponseCode                 ResponseCode = "OK"
	CreatedResponseCode            ResponseCode = "CREATED"
	AcceptedResponseCode           ResponseCode = "ACCEPTED"
	ForbiddenResponseCode          ResponseCode = "FORBIDDEN"
	NotFoundResponseCode           ResponseCode = "NOT_FOUND"
	ConflictResponseCode           ResponseCode = "CONFLICT"
	ServiceUnavailableResponseCode ResponseCode = "SERVICE_UNAVAILABLE"
	NotImplementedResponseCode     ResponseCode = "NOT_IMPLEMENTED"
	MovedPermanentlyResponseCode   ResponseCode = "MOVED_PERMANENTLY"
	BadRequestResponseCode         ResponseCode = "BAD_REQUEST"
)

// ResponseWithCode ...
type ResponseWithCode interface {
	Code() ResponseCode
}
