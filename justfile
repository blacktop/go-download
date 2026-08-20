# List available tasks
default:
    @just --list

# Build the library and the dl CLI
build:
    go build ./...
    cd cmd/dl && go build ./...

# Run library tests with the race detector
test:
    go test -race ./...

# Vet + gofmt check on both modules
lint:
    go vet ./...
    cd cmd/dl && go vet ./...
    @out="$(gofmt -l . cmd/dl 2>/dev/null | grep -v '^OPC/' || true)"; \
    if [ -n "$out" ]; then echo "gofmt needed:"; echo "$out"; exit 1; fi

# gofmt everything (except the OPC reference clone)
fmt:
    go fix ./...
    gofmt -w $(git ls-files '*.go')

# Run the full verification suite (what CI runs)
check: lint test build

# Install the dl CLI from the working tree
install:
    cd cmd/dl && go install .

# Bump version: tag the library and CLI modules, then push
# (Go has no version file to edit — modules version via git tags,
# and the cmd/dl module needs its own cmd/dl/vX.Y.Z tag)
bump:
    #!/usr/bin/env bash
    set -euo pipefail
    if [[ -n "$(git status --porcelain)" ]]; then
        echo "working tree dirty — commit or stash first" >&2
        exit 1
    fi
    TAG="$(svu patch)"
    git tag -a "$TAG" -m "Release $TAG"
    git tag -a "cmd/dl/$TAG" -m "Release dl $TAG"
    git push && git push --tags
    echo "released $TAG"
