GOLANGCI_LINT_VERSION := v1.64.6

codecheck: lint govulncheck
	go fmt ./...
	go fix ./...
	go vet ./...

lint:
	docker run --rm -v $(shell pwd):/app:cached \
	  	-v $(shell go env GOCACHE):/cache/go \
		-v $(shell go env GOPATH)/pkg:/go/pkg \
		-e GOCACHE=/cache/go \
		-e GOLANGCI_LINT_CACHE=/cache/go \
		-w /app golangci/golangci-lint:${GOLANGCI_LINT_VERSION} \
		golangci-lint run --config .golangci.yml --exclude-dirs vendor -v

govulncheck:
	go run golang.org/x/vuln/cmd/govulncheck

test-unit:
	go test -tags=unit ./...

test-integration:
	go test -tags=integration ./...
