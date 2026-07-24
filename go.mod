module github.com/oddin-gg/gosdk

// Language minimum: 1.26 — consumer repos (kollector / ots) have been
// authorised to move to 1.26. The bump unlocks `sync.WaitGroup.Go`
// (1.25), the stable `testing/synctest` package (1.26), and the rest
// of the modern stdlib without `//go:build go1.x` shims.
go 1.26.0

// Toolchain: 1.26.5 — pulls in stdlib CVE patches when building/
// testing this module (GO-2026-4971 net.Dial / GO-2026-4918
// net/http/internal/http2; both reachable from the SDK feed/api
// layers per govulncheck). Consumers using their own toolchain
// (their go.mod's `go` directive) are not affected.
toolchain go1.26.5

require (
	github.com/cenkalti/backoff/v5 v5.0.3
	github.com/google/uuid v1.6.0
	github.com/hashicorp/golang-lru/v2 v2.0.7
	github.com/rabbitmq/amqp091-go v1.11.0
	golang.org/x/mod v0.35.0
	golang.org/x/sync v0.20.0
)

require (
	github.com/BurntSushi/toml v1.6.0 // indirect
	golang.org/x/exp/typeparams v0.0.0-20260410095643-746e56fc9e2f // indirect
	golang.org/x/sys v0.43.0 // indirect
	golang.org/x/telemetry v0.0.0-20260507140634-e88f59f58e45 // indirect
	golang.org/x/tools v0.44.0 // indirect
	golang.org/x/vuln v1.3.0 // indirect
	honnef.co/go/tools v0.7.0 // indirect
)

tool (
	golang.org/x/vuln/cmd/govulncheck
	honnef.co/go/tools/cmd/staticcheck
)
