GOLANGCI_LINT_VERSION := v1.64.8

build:
	go build .

codecheck:
	go fmt ./...
	go fix ./...
	go vet ./...
	go run honnef.co/go/tools/cmd/staticcheck ./...

.PHONY: test
test:
	go test -cover -coverprofile=coverage.txt ./...

test-integration:
	go test -tags=integration ./...

golangci-lint-version:
	 @echo ${GOLANGCI_LINT_VERSION}

lint:
	docker run --rm -v $(shell pwd):/app:cached \
		-v $(shell go env GOCACHE):/cache/go \
		-v $(shell go env GOPATH)/pkg:/go/pkg \
		-e GOCACHE=/cache/go \
		-e GOLANGCI_LINT_CACHE=/cache/go \
		-e GOPRIVATE=oddin.gg,github.com/oddin-gg \
		-w /app golangci/golangci-lint:${GOLANGCI_LINT_VERSION} \
		golangci-lint run --config .golangci.yml --exclude-dirs vendor -v

govulncheck:
	go run golang.org/x/vuln/cmd/govulncheck
