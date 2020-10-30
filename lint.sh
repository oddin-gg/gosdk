#!/usr/bin/env bash

set -euo pipefail

go mod vendor

# check that golint is installed
GOLINT=$(go env GOPATH)/bin/golint
if [[ ! -x ${GOLINT} ]]; then
    echo -e "ERROR: Can not find ${GOLINT}, use \`go get -u golang.org/x/lint/golint\`"
    exit 1
fi

# golint
echo -e "   ### GOLINT ###"
${GOLINT} -set_exit_status $(go list ./... | grep -v /vendor/) | sed 's,.*:.*:,\x1B[31m&\x1B[0m,'
