package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	data "github.com/oddin-gg/gosdk/internal/api/xml"
)

// TestInstallCapture_TruncatesEventButPreservesParserBody verifies the
// streaming-capture contract from NEXT.md §9.3:
//   - the parser sees the full body via the TeeReader
//   - the captured event payload caps at BodyLimit
//   - no full-body materialization (peak memory bounded by parser
//     working set + BodyLimit)
func TestInstallCapture_TruncatesEventButPreservesParserBody(t *testing.T) {
	full := strings.Repeat("ABCDEFGH", 256) // 2048 bytes
	c := &Client{}
	c.capture = EventCapture{
		Emit:         func(APIEvent) {},
		ResponseBody: true,
		BodyLimit:    32,
	}
	r := &http.Response{Body: io.NopCloser(strings.NewReader(full))}

	pc := c.installCapture(r, &http.Request{}, http.StatusOK, 0, 1, nil, nil, false)
	if pc == nil || pc.buf == nil {
		t.Fatal("installCapture returned nil pendingCapture/buf")
	}

	// Drain the body through the tee — simulates the decoder reading.
	parserBytes, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(parserBytes) != full {
		t.Errorf("parser saw %d bytes, want %d (full)", len(parserBytes), len(full))
	}

	if !pc.buf.truncated {
		t.Error("buf.truncated=false; expected true (body > limit)")
	}
	if len(pc.buf.buf) != 32 {
		t.Errorf("captured len = %d, want 32", len(pc.buf.buf))
	}
	if got := string(pc.buf.buf); got != full[:32] {
		t.Errorf("captured prefix mismatch:\n got=%q\nwant=%q", got, full[:32])
	}
}

// TestInstallCapture_BelowLimit confirms the cheap path: body under the
// limit lands fully in the capture buf, truncated stays false.
func TestInstallCapture_BelowLimit(t *testing.T) {
	body := "small"
	c := &Client{}
	c.capture = EventCapture{
		Emit:         func(APIEvent) {},
		ResponseBody: true,
		BodyLimit:    64,
	}
	r := &http.Response{Body: io.NopCloser(strings.NewReader(body))}

	pc := c.installCapture(r, &http.Request{}, http.StatusOK, 0, 1, nil, nil, false)
	if pc == nil {
		t.Fatal("installCapture returned nil")
	}
	parser, _ := io.ReadAll(r.Body)
	if string(parser) != body {
		t.Errorf("parser body = %q, want %q", parser, body)
	}
	if pc.buf.truncated {
		t.Error("truncated=true on body within limit")
	}
	if string(pc.buf.buf) != body {
		t.Errorf("captured = %q, want %q", pc.buf.buf, body)
	}
}

// TestInstallCapture_DisabledReturnsNil — capture not configured.
func TestInstallCapture_DisabledReturnsNil(t *testing.T) {
	c := &Client{} // zero capture (no Emit)
	r := &http.Response{Body: io.NopCloser(strings.NewReader("x"))}
	pc := c.installCapture(r, &http.Request{}, http.StatusOK, 0, 1, nil, nil, false)
	if pc != nil {
		t.Error("pc != nil with capture disabled")
	}
}

// TestInstallCapture_NoResponseBody returns a metadata-only pc when
// the emitter is set but ResponseBody capture is off.
func TestInstallCapture_NoResponseBody(t *testing.T) {
	c := &Client{}
	c.capture = EventCapture{
		Emit:         func(APIEvent) {},
		ResponseBody: false,
		BodyLimit:    64,
	}
	r := &http.Response{Body: io.NopCloser(strings.NewReader("ignored"))}
	pc := c.installCapture(r, &http.Request{}, http.StatusOK, 0, 1, nil, nil, false)
	if pc == nil {
		t.Fatal("expected metadata-only pc")
	}
	if pc.buf != nil {
		t.Error("buf must be nil when ResponseBody is false")
	}
}

// TestEmit_RedactsAccessToken verifies item 6 — captured bytes that
// match the configured access token are scrubbed before emission.
// Uses a realistic long token; short tokens are covered by
// TestRedactSensitive_ShortToken.
// (production tokens are always long enough).
func TestEmit_RedactsAccessToken(t *testing.T) {
	tok := "super-secret-token-abcdef123456"
	cfg := &fakeCfgWithToken{tok: tok}
	c := &Client{cfg: cfg}
	emitted := make(chan APIEvent, 1)
	c.capture = EventCapture{
		Emit:         func(ev APIEvent) { emitted <- ev },
		ResponseBody: true,
		BodyLimit:    1024,
	}

	body := "<resp token=\"" + tok + "\"><payload/></resp>"
	r := &http.Response{Body: io.NopCloser(strings.NewReader(body))}
	pc := c.installCapture(r, &http.Request{Method: http.MethodGet}, http.StatusOK, 0, 1, nil, nil, false)
	_, _ = io.ReadAll(r.Body)
	c.emit(pc, nil)

	ev := <-emitted
	if got := string(ev.Response); got == body {
		t.Errorf("token leaked into event payload: %q", got)
	}
	if got := string(ev.Response); !strings.Contains(got, "[REDACTED]") {
		t.Errorf("[REDACTED] marker missing: %q", got)
	}
	if strings.Contains(string(ev.Response), tok) {
		t.Errorf("raw token still present in event: %q", ev.Response)
	}
}

// fakeCfgWithToken is a minimal config.Config that only
// implements AccessToken — the rest of the methods aren't exercised by
// these tests so we delegate to a zero-value testConfig.
type fakeCfgWithToken struct {
	testConfig
	tok string
}

func (f *fakeCfgWithToken) AccessToken() *string { return &f.tok }

// TestErrorBody_StructuredErrorPreservedWithCapture verifies the v2.6
// review fix: when APILogResponses is enabled, a 4xx response with a
// structured error envelope must still surface the server's error
// message — the body must NOT be double-consumed by readErrorBody.
func TestErrorBody_StructuredErrorPreservedWithCapture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`<response response_code="BAD_REQUEST"><action>reject</action><message>token expired</message></response>`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	emitted := make(chan APIEvent, 1)
	c.SetEventCapture(EventCapture{
		Emit:         func(ev APIEvent) { emitted <- ev },
		ResponseBody: true,
		BodyLimit:    1024,
	})

	var resp data.WhoAMI
	err := c.fetchData(t.Context(), "/users/whoami", &resp, nil)
	if err == nil {
		t.Fatal("expected 4xx error, got nil")
	}
	// The structured server message must be preserved in the wrapped error.
	if !strings.Contains(err.Error(), "token expired") {
		t.Errorf("structured error lost: %v", err)
	}

	// And the APIEvent must still carry the captured body bytes.
	select {
	case ev := <-emitted:
		if !strings.Contains(string(ev.Response), "token expired") {
			t.Errorf("APIEvent body missing structured error: %q", ev.Response)
		}
		if ev.Status != http.StatusBadRequest {
			t.Errorf("APIEvent.Status = %d, want %d", ev.Status, http.StatusBadRequest)
		}
	case <-time.After(time.Second):
		t.Fatal("no APIEvent emitted within 1s")
	}
}

// TestCaptureRequestBytes_FillsAPIEventRequest verifies the v2.19 fix
// to finding F4: APILogFull (RequestBody=true) now actually populates
// APIEvent.Request from the request body. captureRequestBytes drains
// req.Body, restores it via io.NopCloser + GetBody so the http
// transport (and retries) can still send the body, then returns the
// redacted, length-bounded snapshot for the event.
//
// All current SDK API paths use bodyless requests, so this is wired
// through for future endpoints. The test exercises the mechanism
// directly: construct a request with a body, run captureRequestBytes,
// and assert the returned bytes + req.Body still readable for the
// "transport" + req.GetBody works for retries.
func TestCaptureRequestBytes_FillsAPIEventRequest(t *testing.T) {
	full := []byte("<request>payload-for-future-post</request>")
	c := &Client{cfg: &fakeCfgWithToken{tok: ""}}
	c.capture = EventCapture{
		Emit:        func(APIEvent) {},
		RequestBody: true,
		BodyLimit:   1024,
	}

	req := &http.Request{
		Method: http.MethodPost,
		Body:   io.NopCloser(strings.NewReader(string(full))),
	}

	captured, truncated := c.captureRequestBytes(req)
	if string(captured) != string(full) {
		t.Errorf("captured = %q, want %q", captured, full)
	}
	if truncated {
		t.Error("truncated = true with body well under BodyLimit")
	}

	// req.Body must be re-readable so the http transport can send it.
	got, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("ReadAll(req.Body): %v", err)
	}
	if string(got) != string(full) {
		t.Errorf("req.Body after capture = %q, want %q", got, full)
	}

	// req.GetBody must replay for retries.
	if req.GetBody == nil {
		t.Fatal("req.GetBody = nil; retries cannot replay body")
	}
	rc, err := req.GetBody()
	if err != nil {
		t.Fatalf("req.GetBody(): %v", err)
	}
	got2, _ := io.ReadAll(rc)
	if string(got2) != string(full) {
		t.Errorf("req.GetBody() = %q, want %q", got2, full)
	}
}

// TestCaptureRequestBytes_TruncatesAtBodyLimit verifies the body-cap
// behaviour of the F4 capture machinery — bodies larger than
// BodyLimit are truncated and the truncated flag is set, matching
// response-side capture semantics.
func TestCaptureRequestBytes_TruncatesAtBodyLimit(t *testing.T) {
	full := strings.Repeat("X", 200)
	c := &Client{cfg: &fakeCfgWithToken{tok: ""}}
	c.capture = EventCapture{
		Emit:        func(APIEvent) {},
		RequestBody: true,
		BodyLimit:   32,
	}
	req := &http.Request{
		Method: http.MethodPost,
		Body:   io.NopCloser(strings.NewReader(full)),
	}

	captured, truncated := c.captureRequestBytes(req)
	if !truncated {
		t.Error("truncated = false; expected truncation past BodyLimit=32")
	}
	if len(captured) != 32 {
		t.Errorf("len(captured) = %d, want 32", len(captured))
	}

	// Transport still sees the FULL body (truncation only affects the
	// captured snapshot, not the bytes sent over the wire).
	got, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("ReadAll(req.Body): %v", err)
	}
	if len(got) != 200 {
		t.Errorf("req.Body length = %d, want 200 (untruncated for transport)", len(got))
	}
}

// TestCaptureRequestBytes_NoOpWhenDisabled verifies the F4 capture
// is a no-op when RequestBody capture is not enabled — preserves the
// existing zero-cost behaviour for APILogResponses and below.
func TestCaptureRequestBytes_NoOpWhenDisabled(t *testing.T) {
	full := []byte("<request>body</request>")
	c := &Client{}
	c.capture = EventCapture{
		Emit:         func(APIEvent) {},
		ResponseBody: true,
		BodyLimit:    1024,
		// RequestBody intentionally false — APILogResponses level.
	}
	req := &http.Request{
		Method: http.MethodPost,
		Body:   io.NopCloser(strings.NewReader(string(full))),
	}

	captured, truncated := c.captureRequestBytes(req)
	if captured != nil || truncated {
		t.Errorf("capture with RequestBody=false: got (%q, %v), want (nil, false)", captured, truncated)
	}
}

// TestCaptureRequestBytes_NilBodyIsSafe verifies the no-body path
// (every current SDK API call) doesn't panic when capture is enabled.
func TestCaptureRequestBytes_NilBodyIsSafe(t *testing.T) {
	c := &Client{cfg: &fakeCfgWithToken{tok: ""}}
	c.capture = EventCapture{
		Emit:        func(APIEvent) {},
		RequestBody: true,
		BodyLimit:   1024,
	}
	req := &http.Request{Method: http.MethodGet, Body: nil}

	captured, truncated := c.captureRequestBytes(req)
	if captured != nil || truncated {
		t.Errorf("nil-body capture: got (%q, %v), want (nil, false)", captured, truncated)
	}
}

// TestRedactSensitive_ShortToken is the regression for the redaction
// floor: tokens shorter than 16 bytes were not scrubbed at all, so a
// response or error echoing such a token reached opt-in APIEvent
// observers unsanitized — despite config validation only requiring a
// non-empty token. Every non-empty token must be redacted; the
// over-scrub of coincidental substrings is an accepted fidelity cost.
func TestRedactSensitive_ShortToken(t *testing.T) {
	cfg := &testConfig{apiURL: "api.example.com", token: "tok"}
	c := New(cfg)

	got := string(c.redactSensitive([]byte(`<error>bad token "tok" rejected</error>`)))
	if strings.Contains(got, `"tok"`) {
		t.Fatalf("short token not redacted: %q", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("no redaction marker in %q", got)
	}

	// Empty token: nothing to redact, buffer unchanged (and no
	// pathological empty-pattern ReplaceAll).
	cfg2 := &testConfig{apiURL: "api.example.com", token: ""}
	c2 := New(cfg2)
	in := `<response>ok</response>`
	if got := string(c2.redactSensitive([]byte(in))); got != in {
		t.Fatalf("empty token mutated capture: %q", got)
	}
}

// TestRedactSensitive_WireEncodedForms is the regression for encoded
// credential echoes (Codex P2): an XML response echoing a token that
// contains XML-sensitive characters carries the CHARACTER-ESCAPED form
// (&amp;, &lt;, …), and a URL rendered into a body or error carries the
// QUERY-ESCAPED form — neither matches an exact raw-token replacement,
// so the credential reached APIEvent observers despite the "always
// sanitized" guarantee.
func TestRedactSensitive_WireEncodedForms(t *testing.T) {
	cfg := &testConfig{apiURL: "api.example.com", token: `se<c&re>t"tok`}
	c := New(cfg)

	xmlEcho := `<error>key se&lt;c&amp;re&gt;t&#34;tok was rejected</error>`
	got := string(c.redactSensitive([]byte(xmlEcho)))
	if strings.Contains(got, "re&gt;t") || strings.Contains(got, "se&lt;c") {
		t.Fatalf("XML-escaped token survived redaction: %q", got)
	}

	urlEcho := `Get "https://api.example.com/v1/users/whoami?key=se%3Cc%26re%3Et%22tok": EOF`
	got = string(c.redactSensitive([]byte(urlEcho)))
	if strings.Contains(got, "se%3C") {
		t.Fatalf("URL-escaped token survived redaction: %q", got)
	}
}

// TestRedactCapture_BoundarySplitToken is the regression for the
// truncation-bisected token (Codex P2): capture buffers are capped at
// BodyLimit BEFORE redaction, so a token straddling the limit leaves a
// prefix fragment that exact-match replacement cannot find — with the
// boundary in the token's last byte, the fragment is nearly the whole
// credential. A truncated capture whose tail is a partial prefix of any
// token wire form must have that tail scrubbed.
func TestRedactCapture_BoundarySplitToken(t *testing.T) {
	const token = "secret-token-abcdef123456"
	cfg := &testConfig{apiURL: "api.example.com", token: token}
	c := New(cfg)

	// The capture cut the echoed token one byte short of complete.
	almostWhole := "<error>bad key " + token[:len(token)-1]
	got := string(c.redactCapture([]byte(almostWhole), true))
	if strings.Contains(got, token[:8]) {
		t.Fatalf("boundary-split token fragment survived: %q", got)
	}
	if !strings.HasSuffix(got, "[REDACTED]") {
		t.Fatalf("boundary tail not replaced with marker: %q", got)
	}

	// Non-truncated captures must NOT have coincidental tails scrubbed.
	benign := "<response>fine s</response>"
	if got := string(c.redactCapture([]byte(benign), false)); got != benign {
		t.Fatalf("untruncated capture mutated: %q", got)
	}

	// Complete tokens inside a truncated capture still use exact-match
	// replacement; the boundary pass only touches the tail.
	both := "<error>key " + token + " rejected; retry key " + token[:10]
	got = string(c.redactCapture([]byte(both), true))
	if strings.Contains(got, token) || strings.Contains(got, token[:8]) {
		t.Fatalf("token content survived combined redaction: %q", got)
	}
}
