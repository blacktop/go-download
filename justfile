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

# Run library and CLI tests with the race detector
test:
    go test -race ./...
    cd cmd/dl && go test -race ./...

# Vet + gofmt check on both modules
lint:
    go vet ./...
    cd cmd/dl && go vet ./...
    @out="$(gofmt -l . 2>/dev/null | grep -v '^OPC/' || true)"; \
    if [ -n "$out" ]; then echo "gofmt needed:"; echo "$out"; exit 1; fi

# gofmt both modules (OPC's nested module is outside ./... by construction;
# package-based selection also survives uncommitted file deletions, unlike
# git ls-files, which lists tracked-but-deleted paths)
fmt:
    go fix ./...
    go fmt ./...
    cd cmd/dl && go fix ./... && go fmt ./...

# Run the full verification suite (what CI runs)
check: lint test build

# All loopback benchmarks (see README Performance); the network benchmarks
# skip themselves unless DL_BENCH_URL is set
bench:
    go test -run '^$' -bench . -benchtime 5x -count=3 .

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
    branch="$(git branch --show-current)"
    if [[ "$branch" != "main" ]]; then
        echo "releases are cut from main, not '$branch'" >&2
        exit 1
    fi
    TAG={{ quote(tag) }}
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
    just check
    if [[ -n "$(git status --porcelain)" ]]; then
        echo "verification changed the tree (go.mod/go.sum or generated files?) —" >&2
        echo "inspect and commit those changes before releasing" >&2
        exit 1
    fi
    read -r -p "release $TAG (library) and cmd/dl/$TAG (CLI) from $(git rev-parse --short HEAD)? [y/N] " reply
    if [[ "$reply" != [yY] ]]; then
        echo "aborted"
        exit 1
    fi
    # Stage 1: publish the branch first — a rejected/non-fast-forward push
    # must not leave a local release tag behind — then the library tag.
    git push origin main
    git tag -a "$TAG" -m "Release $TAG"
    git push origin "refs/tags/$TAG"
    # Stage 2: pin the CLI to the version that now exists. The module proxy
    # fetches new tags on demand but can lag; retry briefly before leaving
    # the release half-done.
    for attempt in 1 2 3; do
        if env GOFLAGS=-mod=mod GOWORK=off go -C cmd/dl get "github.com/blacktop/go-download@$TAG" \
            && env GOWORK=off go -C cmd/dl mod tidy; then
            break
        fi
        if [[ "$attempt" == 3 ]]; then
            echo "pinning cmd/dl failed after 3 attempts; library tag $TAG is already pushed —" >&2
            echo "rerun the pin manually with the same isolation:" >&2
            echo "  env GOFLAGS=-mod=mod GOWORK=off go -C cmd/dl get github.com/blacktop/go-download@$TAG" >&2
            echo "  env GOWORK=off go -C cmd/dl mod tidy" >&2
            exit 1
        fi
        echo "go get $TAG not yet visible; retrying in 10s" >&2
        sleep 10
    done
    pinned="$(env GOWORK=off go -C cmd/dl list -m github.com/blacktop/go-download | awk '{print $2}')"
    if [[ "$pinned" != "$TAG" ]]; then
        echo "cmd/dl resolves go-download $pinned, want $TAG — aborting before the CLI tag" >&2
        exit 1
    fi
    if git diff --quiet cmd/dl/go.mod cmd/dl/go.sum; then
        echo "cmd/dl already pinned to $TAG; skipping pin commit"
    else
        git add cmd/dl/go.mod cmd/dl/go.sum
        git commit -m "chore(dl): pin go-download $TAG"
        git push origin main
    fi
    # Stage 3: publish the CLI from the pinned commit.
    git tag -a "cmd/dl/$TAG" -m "Release dl $TAG"
    git push origin "refs/tags/cmd/dl/$TAG"
    echo "released $TAG (library) and cmd/dl/$TAG (CLI)"
