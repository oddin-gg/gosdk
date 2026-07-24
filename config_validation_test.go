package gosdk

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/types"
)

// TestValidateConfig_BoundedOptions pins the up-front bounds validation:
// out-of-range numeric / enum / required-string options must surface as
// ErrInvalidConfig from New (via validateConfig) rather than as runtime
// failures later. Forced hosts are supplied so endpoint resolution
// always succeeds and the bounded check under test is the only thing
// that can fail.
func TestValidateConfig_BoundedOptions(t *testing.T) {
	base := []Option{WithAPIHost("api.example.com"), WithMQHost("mq.example.com")}

	// Baseline: valid config passes.
	valid := NewConfig("token", types.IntegrationEnvironment, base...)
	if err := validateConfig(&valid); err != nil {
		t.Fatalf("baseline valid config rejected: %v", err)
	}
	// Boundary: 65535 is the largest prefetch a uint16 can carry — valid.
	maxPrefetch := NewConfig("token", types.IntegrationEnvironment, append(append([]Option{}, base...), WithAMQPPrefetch(65535))...)
	if err := validateConfig(&maxPrefetch); err != nil {
		t.Fatalf("prefetch 65535 rejected: %v", err)
	}

	bad := []struct {
		name string
		opt  Option
	}{
		{"maxInactivity zero", WithMaxInactivity(0)},
		{"maxInactivity negative", WithMaxInactivity(-time.Second)},
		{"maxRecoveryExecution zero", WithMaxRecoveryExecution(0)},
		{"httpClientTimeout zero", WithHTTPClientTimeout(0)},
		{"shutdownTimeout zero", WithShutdownTimeout(0)},
		{"shutdownTimeout negative", WithShutdownTimeout(-time.Second)},
		{"initialSnapshotTime negative", WithInitialSnapshotTime(-time.Second)},
		{"port zero", WithMessagingPort(0)},
		{"port too high", WithMessagingPort(70000)},
		{"port negative", WithMessagingPort(-1)},
		{"prefetch negative", WithAMQPPrefetch(-1)},
		// uint16 wrap (Codex P2): AMQP basic.qos carries prefetch as
		// uint16 — 65536 wrapped to protocol 0 = UNLIMITED prefetch
		// (backpressure silently disabled), 65537 wrapped to 1.
		{"prefetch wraps to unlimited", WithAMQPPrefetch(65536)},
		{"prefetch wraps to one", WithAMQPPrefetch(65537)},
		// Extreme accepted buffer values previously panicked/OOMed only
		// AFTER broker setup (Codex P3) — bound them up front.
		{"subscription buffer beyond practical max", WithSubscriptionBuffer(1<<20 + 1)},
		{"subscription buffer negative", WithSubscriptionBuffer(-1)},
		{"api body limit negative", WithAPICallBodyLimit(-1)},
		{"unknown exception strategy", WithExceptionStrategy(ExceptionStrategy(99))},
		{"unknown api log level", WithAPICallLogging(APILogLevel(99))},
		{"empty default locale", WithDefaultLocale(types.Locale(""))},
		{"empty exchange name", WithExchangeName("")},
		{"empty replay exchange name", WithReplayExchangeName("")},
		{"empty sport id prefix", WithSportIDPrefix("")},
		// Malformed sport-id prefixes (Codex P2): the prefix is
		// concatenated with a numeric segment and parsed as a URN on
		// every feed delivery — a shape that can't yield a valid URN
		// makes ordinary deliveries silently unparsable (ACKed under
		// StrategyCatch), so New must reject it up front.
		{"sport prefix single component", WithSportIDPrefix("sport:")},
		{"sport prefix missing trailing colon", WithSportIDPrefix("od:sport")},
		{"sport prefix trailing digit", WithSportIDPrefix("od:sport:0")},
		{"sport prefix empty component", WithSportIDPrefix("od::")},
		{"sport prefix too many components", WithSportIDPrefix("od:sport:x:")},
		{"sport prefix url-reserved chars", WithSportIDPrefix("od/x:sport:")},
		// Negative node ids (Codex P3) become routing keys like
		// "snapshot_complete.-1" and negative node_id query params.
		{"negative node id", WithNodeID(-1)},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			cfg := NewConfig("token", types.IntegrationEnvironment, append(append([]Option{}, base...), tc.opt)...)
			err := validateConfig(&cfg)
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("validateConfig = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

// TestValidateConfig_ZeroSemantics pins the "0 = use default / none"
// options that must NOT be rejected: initialSnapshotTime (0 = default
// lookback) and the size knobs (0 = default / unbuffered).
func TestValidateConfig_ZeroSemantics(t *testing.T) {
	base := []Option{WithAPIHost("api.example.com"), WithMQHost("mq.example.com")}
	ok := []struct {
		name string
		opt  Option
	}{
		{"initialSnapshotTime zero", WithInitialSnapshotTime(0)},
		{"prefetch zero", WithAMQPPrefetch(0)},
		{"subscription buffer zero", WithSubscriptionBuffer(0)},
		{"api body limit zero", WithAPICallBodyLimit(0)},
		{"node id zero", WithNodeID(0)},
		{"well-formed custom sport prefix", WithSportIDPrefix("sr:sport:")},
	}
	for _, tc := range ok {
		t.Run(tc.name, func(t *testing.T) {
			cfg := NewConfig("token", types.IntegrationEnvironment, append(append([]Option{}, base...), tc.opt)...)
			if err := validateConfig(&cfg); err != nil {
				t.Fatalf("validateConfig rejected a valid zero value: %v", err)
			}
		})
	}
}

// TestValidateConfig_ForcedHosts pins structural host validation: the MQ
// host must be a bare hostname (no port — the dialer appends
// WithMessagingPort), the API host may carry an explicit :port, and
// neither may look like a URL (userinfo / path / query / whitespace).
func TestValidateConfig_ForcedHosts(t *testing.T) {
	bad := []struct {
		name string
		opt  Option
	}{
		{"mq with port", WithMQHost("mq.example.com:5671")},
		{"mq with path", WithMQHost("mq.example.com/vhost")},
		{"mq with userinfo", WithMQHost("user@mq.example.com")},
		{"mq with query", WithMQHost("mq.example.com?x=1")},
		{"mq with whitespace", WithMQHost("mq example com")},
		{"api with path", WithAPIHost("api.example.com/v3")},
		{"api with userinfo", WithAPIHost("user@api.example.com")},
		{"api with fragment", WithAPIHost("api.example.com#frag")},
		{"api port out of range", WithAPIHost("api.example.com:70000")},
		// Malformed colon-bearing hosts that SplitHostPort errors on for
		// reasons OTHER than "missing port" — previously passed through.
		{"mq repeated colons", WithMQHost("mq.example.com:5671:5672")},
		{"api repeated colons", WithAPIHost("api.example.com:1:2")},
		{"mq unbracketed ipv6", WithMQHost("2001:db8::1")},
		{"api unbracketed ipv6", WithAPIHost("::1")},
		{"mq malformed brackets", WithMQHost("[fe80::1")},
		{"mq invalid bracketed ipv6", WithMQHost("[invalid]")},
		{"mq bracketed ipv4", WithMQHost("[127.0.0.1]")},
		{"api non-numeric port", WithAPIHost("api.example.com:https")},
		// net/url allows only ASCII digits in a port; strconv.Atoi would
		// otherwise accept the sign and let it fail later.
		{"api signed port", WithAPIHost("api.example.com:+443")},
		{"api negative port", WithAPIHost("api.example.com:-443")},
	}
	for _, tc := range bad {
		t.Run("reject/"+tc.name, func(t *testing.T) {
			cfg := NewConfig("token", types.IntegrationEnvironment, tc.opt)
			if err := validateConfig(&cfg); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("validateConfig = %v, want ErrInvalidConfig", err)
			}
		})
	}

	good := []struct {
		name string
		opt  Option
	}{
		{"mq bare host", WithMQHost("mq.example.com")},
		{"mq bracketed ipv6", WithMQHost("[2001:db8::1]")},
		{"api bare host", WithAPIHost("api.example.com")},
		{"api host with port", WithAPIHost("api.example.com:443")},
		// Absolute/FQDN form with a single trailing root dot is valid.
		{"mq fqdn trailing dot", WithMQHost("mq.example.com.")},
		{"api fqdn trailing dot", WithAPIHost("api.example.com.")},
	}
	for _, tc := range good {
		t.Run("accept/"+tc.name, func(t *testing.T) {
			cfg := NewConfig("token", types.IntegrationEnvironment, tc.opt)
			if err := validateConfig(&cfg); err != nil {
				t.Fatalf("validateConfig rejected a valid host: %v", err)
			}
		})
	}
}

// TestValidateConfig_RegionInjection is the regression for the
// authority-injection P1: Region is an open string interpolated into the
// derived endpoint host (types.Environment.APIEndpoint), so a crafted
// value like "x@evil.example/" made net/url read "evil.example" as the
// HOST — New's synchronous who-am-i probe then sent X-Access-Token to an
// attacker-controlled TLS endpoint. validateRegion must reject every
// authority delimiter and require dot-terminated DNS labels (a missing
// terminal dot silently retargets the base domain: "eu" → "euoddin.gg").
// No forced hosts here — the derived-endpoint path is the one under test.
func TestValidateConfig_RegionInjection(t *testing.T) {
	valid := []types.Region{
		types.RegionDefault,        // ""
		types.APSouthEast1,         // "ap-southeast-1."
		types.Region("eu-west-9."), // unknown-but-wellformed region stays allowed
		types.Region("a.b."),       // multi-label
	}
	for _, r := range valid {
		cfg := NewConfig("token", types.IntegrationEnvironment, WithRegion(r))
		if err := validateConfig(&cfg); err != nil {
			t.Errorf("region %q rejected: %v", r, err)
		}
	}

	invalid := []types.Region{
		"x@evil.example/",          // userinfo + path — the reported exploit
		"evil.example/",            // path delimiter via trailing-dot abuse
		"x/evil.",                  // path delimiter
		"x?q=1.",                   // query delimiter
		"x#frag.",                  // fragment delimiter
		"x:443.",                   // port/colon
		"x evil.",                  // whitespace
		"eu",                       // missing terminal dot → "euoddin.gg"
		"-bad.",                    // invalid label (leading hyphen)
		"a..",                      // empty label
		"x@evil.example.",          // userinfo, wellformed tail
		types.Region("žluťoučký."), // non-ASCII label
	}
	for _, r := range invalid {
		cfg := NewConfig("token", types.IntegrationEnvironment, WithRegion(r))
		err := validateConfig(&cfg)
		if !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("region %q: validateConfig = %v, want ErrInvalidConfig", r, err)
		}
	}
}

// TestConfig_FormattingRedactsToken pins the credential-redaction on
// every ordinary formatting path: Config stores the token in a plain
// field, so %v / %+v / %#v / slog.Any previously printed it verbatim
// despite the field being unexported.
func TestConfig_FormattingRedactsToken(t *testing.T) {
	const secret = "super-secret-access-token-123"
	cfg := NewConfig(secret, types.IntegrationEnvironment)

	// Fprintf (not Sprintf) for the explicit %s path: standalone
	// staticcheck's S1025 flags Sprintf("%s", stringer) and the
	// Makefile's raw staticcheck target has no golangci-style nolint.
	var viaFprintf strings.Builder
	fmt.Fprintf(&viaFprintf, "%s", cfg)
	renderings := map[string]string{
		"%v":       fmt.Sprintf("%v", cfg),
		"%+v":      fmt.Sprintf("%+v", cfg),
		"%#v":      fmt.Sprintf("%#v", cfg),
		"%s":       viaFprintf.String(),
		"stringer": cfg.String(),
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	logger.Info("cfg", slog.Any("config", cfg))
	renderings["slog"] = buf.String()

	for path, out := range renderings {
		if strings.Contains(out, secret) {
			t.Errorf("%s leaks the access token: %s", path, out)
		}
		if !strings.Contains(out, "[REDACTED]") {
			t.Errorf("%s carries no redaction marker: %s", path, out)
		}
	}
	// Pointer formatting too (fmt promotes value receivers).
	if out := fmt.Sprintf("%+v", &cfg); strings.Contains(out, secret) {
		t.Errorf("pointer %%+v leaks the access token: %s", out)
	}
}
