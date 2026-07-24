GOLANGCI_LINT_VERSION := v2.12.1
GOFLAGS               ?=
PKG                   ?= ./...

.PHONY: help
help:
	@echo "Available targets:"
	@echo "  build              - go build ./..."
	@echo "  test               - go test -race -cover ./..."
	@echo "  test-integration   - tests with -tags=integration"
	@echo "  test-smoke         - tests with -tags=smoke"
	@echo "  fmt                - gofmt + goimports across the tree"
	@echo "  tidy               - go mod tidy"
	@echo "  vet                - go vet ./..."
	@echo "  staticcheck        - run staticcheck via go tool"
	@echo "  govulncheck        - run govulncheck via go tool"
	@echo "  codecheck          - fmt + vet + staticcheck"
	@echo "  lint               - run golangci-lint inside docker (uses .golangci.yml)"
	@echo "  lint-local         - run a locally-installed golangci-lint binary"
	@echo "  lint-fix           - golangci-lint with --fix (apply autofixes)"
	@echo "  coverage           - test + open coverage HTML report"
	@echo "  clean              - remove build artefacts (coverage.txt, etc.)"
	@echo "  golangci-lint-version - print the pinned golangci-lint version"

# --- build / test ---

.PHONY: build
build:
	go build $(GOFLAGS) $(PKG)

.PHONY: test
test:
	go test $(GOFLAGS) -race -cover -coverprofile=coverage.txt $(PKG)

# NOTE: no tracked test carries the integration/smoke build tag yet —
# these targets are placeholders for the pre-beta live-broker suite and
# currently run exactly the unit tests. A green run here is NOT extra
# coverage.
.PHONY: test-integration
test-integration:
	go test $(GOFLAGS) -race -tags=integration $(PKG)

.PHONY: test-smoke
test-smoke:
	go test $(GOFLAGS) -race -tags=smoke $(PKG)

.PHONY: coverage
coverage: test
	go tool cover -html=coverage.txt

# --- formatters / static checks (no docker required) ---

.PHONY: fmt
fmt:
	go fmt $(PKG)
	@command -v goimports >/dev/null 2>&1 && goimports -w -local github.com/oddin-gg/gosdk . || \
		echo "goimports not installed; skip (go install golang.org/x/tools/cmd/goimports@latest)"

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: vet
vet:
	go vet $(PKG)

.PHONY: staticcheck
staticcheck:
	go tool staticcheck $(PKG)

.PHONY: govulncheck
govulncheck:
	go tool govulncheck $(PKG)

.PHONY: codecheck
codecheck: fmt vet staticcheck

# --- linter (docker — pinned version, reproducible across machines) ---

.PHONY: golangci-lint-version
golangci-lint-version:
	@echo $(GOLANGCI_LINT_VERSION)

.PHONY: lint
lint:
	docker run --rm -v $(shell pwd):/app:cached \
		-v $(shell go env GOCACHE):/cache/go \
		-v $(shell go env GOPATH)/pkg:/go/pkg \
		-e GOCACHE=/cache/go \
		-e GOLANGCI_LINT_CACHE=/cache/go \
		-e GOPRIVATE=oddin.gg,github.com/oddin-gg \
		-w /app golangci/golangci-lint:$(GOLANGCI_LINT_VERSION) \
		golangci-lint run --config .golangci.yml -v

# Convenience target for fast local iteration. Uses whatever golangci-lint
# is on PATH (intentionally NOT version-pinned — for CI parity, use `make lint`).
.PHONY: lint-local
lint-local:
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint not installed. Run: brew install golangci-lint"; exit 1; }
	golangci-lint run --config .golangci.yml

.PHONY: lint-fix
lint-fix:
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint not installed. Run: brew install golangci-lint"; exit 1; }
	golangci-lint run --config .golangci.yml --fix

# --- maintenance ---

.PHONY: clean
clean:
	rm -f coverage.txt coverage.html
