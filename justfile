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

# Loopback benchmarks: stdlib baseline vs the engine, unconstrained and
# per-connection-throttled (see README Performance)
bench:
    go test -run '^$' -bench 'BenchmarkGetMultipart|BenchmarkStdlibGet|BenchmarkConstrained' -benchtime 5x -count=3 .

# Install the dl CLI from the working tree
install:
    cd cmd/dl && go install .

# Bump version in two stages so cmd/dl/vX.Y.Z never ships a stale engine:
# 1. tag + push the library, 2. pin cmd/dl to that published version
# (go get + tidy, committed), 3. tag + push the CLI from that commit.
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
    for t in "$TAG" "cmd/dl/$TAG"; do
        if git rev-parse -q --verify "refs/tags/$t" >/dev/null; then
            echo "tag $t already exists" >&2
            exit 1
        fi
    done
    # Stage 1: publish the library.
    git tag -a "$TAG" -m "Release $TAG"
    git push && git push origin "refs/tags/$TAG"
    # Stage 2: pin the CLI to the version that now exists.
    (cd cmd/dl && GOFLAGS=-mod=mod GOWORK=off go get "github.com/blacktop/go-download@$TAG" \
        && GOWORK=off go mod tidy)
    git add cmd/dl/go.mod cmd/dl/go.sum
    git commit -m "chore(dl): pin go-download $TAG"
    git push
    # Stage 3: publish the CLI from the pinned commit.
    git tag -a "cmd/dl/$TAG" -m "Release dl $TAG"
    git push origin "refs/tags/cmd/dl/$TAG"
    echo "released $TAG (library) and cmd/dl/$TAG (CLI)"
