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
	@echo "  update-local-dep   - update the pinned golangci-lint tool"
	@echo "  vet                - go vet ./..."
	@echo "  staticcheck        - run staticcheck via go tool"
	@echo "  govulncheck        - run govulncheck via go tool"
	@echo "  codecheck          - fmt + vet + staticcheck"
	@echo "  lint               - run golangci-lint via go tool (uses .golangci.yml)"
	@echo "  lint-fix           - golangci-lint via go tool with --fix (apply autofixes)"
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

# --- linter (Go tool — pinned version, reproducible across machines) ---

# Lint under the Go release go.mod declares, as CI does: golangci-lint cannot analyse a Go release newer than the one it was built for.
GOLANGCI_LINT := GOTOOLCHAIN=$(shell awk '/^toolchain /{t=$$2} /^go /{g=$$2} END{if(t!="")print t; else print "go" g (split(g,p,".")==2?".0":"")}' go.mod) go tool golangci-lint

.PHONY: golangci-lint-version
golangci-lint-version:
	@grep -m1 -oE 'github.com/golangci/golangci-lint/v2 v[0-9]+\.[0-9]+\.[0-9]+' go.mod | cut -d' ' -f2

.PHONY: lint
lint:
	$(GOLANGCI_LINT) run --config .golangci.yml -v

.PHONY: lint-fix
lint-fix:
	$(GOLANGCI_LINT) run --config .golangci.yml --fix

# --- maintenance ---

.PHONY: update-local-dep
update-local-dep:
	@echo "Updating golangci-lint@v2.15.0" && go get -tool github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0
	@go mod tidy

.PHONY: clean
clean:
	rm -f coverage.txt coverage.html
