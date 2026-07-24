package types

// ResponseCode is the consumer-visible enum mirroring the
// `response_code` attribute on the legacy `<response>` envelope. The
// internal Response / ResponseWithCode types that wrap it now live
// in internal/api (relocated v2.32) — this enum stays in types/
// because consumer code can compare against the constants when
// inspecting wrapped errors.
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
