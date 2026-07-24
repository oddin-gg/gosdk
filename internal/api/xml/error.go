package xml

import "encoding/xml"

// Error is the <response> envelope. XMLName is pinned so a decode of a
// non-<response> root ERRORS — classifyMutationBody depends on that to
// tell a genuine 2xx <response> envelope from some other success payload.
type Error struct {
	XMLName xml.Name `xml:"response"`
	Code    string   `xml:"response_code,attr"`
	Action  string   `xml:"action"`
	Message string   `xml:"message"`
}

// ErrorBody leniently decodes a non-2xx error body from EITHER supported
// wire shape:
//
//	<response response_code="NOT_FOUND"><message>…</message></response>
//	<error><message>…</message></error>
//
// Unlike Error it does NOT pin XMLName, so both roots decode — the pinned
// Error silently failed to decode the <error> shape, dropping the server
// message. Code stays empty for the <error> shape (it carries no
// response_code attribute).
type ErrorBody struct {
	Code    string `xml:"response_code,attr"`
	Message string `xml:"message"`
}
