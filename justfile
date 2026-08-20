# List available tasks
default:
    @just --list

# One-time dev setup: workspace so cmd/dl builds against the in-tree library
setup:
    #!/usr/bin/env bash
    set -euo pipefail
    if [[ ! -f go.work ]]; then
        go work init . ./cmd/dl
        echo "created go.work"
    else
        echo "go.work already exists"
    fi

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
# and the cmd/dl module needs its own cmd/dl/vX.Y.Z tag).
# TAG defaults to the next patch (svu); pass one to override: `just bump v1.2.0`
bump tag="":
    #!/usr/bin/env bash
    set -euo pipefail
    if [[ -n "$(git status --porcelain)" ]]; then
        echo "working tree dirty — commit or stash first" >&2
        exit 1
    fi
    TAG="{{tag}}"
    if [[ -z "$TAG" ]]; then
        TAG="$(svu patch)"
    elif [[ ! "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
        echo "invalid tag '$TAG' — want semver like v1.2.3" >&2
        exit 1
    fi
    if git rev-parse -q --verify "refs/tags/$TAG" >/dev/null; then
        echo "tag $TAG already exists" >&2
        exit 1
    fi
    git tag -a "$TAG" -m "Release $TAG"
    git tag -a "cmd/dl/$TAG" -m "Release dl $TAG"
    git push && git push --tags
    echo "released $TAG"
