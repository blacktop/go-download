# go-download

[![Go](https://github.com/blacktop/go-download/actions/workflows/go.yml/badge.svg)](https://github.com/blacktop/go-download/actions/workflows/go.yml) [![Go Reference](https://pkg.go.dev/badge/github.com/blacktop/go-download.svg)](https://pkg.go.dev/github.com/blacktop/go-download) [![License](http://img.shields.io/:license-mit-blue.svg)](http://doge.mit-license.org)

> Download LARGE files as fast as possible. Zero dependencies.

---

## Features

- **Parallel parts** — the file is split into ranges downloaded over parallel HTTP/1.1 connections (HTTP/2 is deliberately avoided: it would multiplex every range onto a single TCP connection)
- **Dynamic work stealing** — fast connections steal the remaining tail of slow ones (aria2-style segment allocation), so one bad connection never drags out the whole download
- **CDN node racing** — when the host resolves to multiple edge nodes, each connection is pinned to one node, per-node throughput is measured (EWMA), and statistically slow nodes are abandoned for better ones
- **Automatic resume** — progress is tracked in a `.part.json` sidecar; an interrupted download picks up where it left off, validated against the server's ETag/Last-Modified
- **Stall detection** — a per-read timeout (adaptive on flaky links) aborts and retries hung connections; retries resume mid-range, never re-downloading bytes
- **Safe by construction** — bytes are staged in a preallocated `.part` file written at disjoint offsets; the destination path is only created by an atomic rename after size (and optional SHA-256) verification
- **Zero dependencies** — the library is pure standard library; bring your own progress UI via the `Reporter` interface (or use the `dl` CLI)

## Install

```bash
go get github.com/blacktop/go-download
```

## Getting Started

```go
package main

import (
    "context"
    "fmt"
    "os"
    "os/signal"
    "syscall"

    "github.com/blacktop/go-download"
)

func main() {
    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    dl, err := download.New(nil) // defaults: 8 parts, resume, silent
    if err != nil {
        fmt.Fprintln(os.Stderr, err)
        os.Exit(1)
    }

    res, err := dl.Get(ctx, os.Args[1], "")
    if err != nil {
        fmt.Fprintln(os.Stderr, err)
        os.Exit(1)
    }
    fmt.Printf("saved %s (%d bytes in %s)\n", res.Path, res.Size, res.Elapsed)
}
```

All knobs live on `download.Options`:

```go
dl, err := download.New(&download.Options{
    Parts:          8,                // parallel connections
    MinPartSize:    16 << 20,         // never split ranges below 2x this
    ExpectedSHA256: "6ca0e5...",      // verify before the final rename
    Reporter:       myProgressUI,     // receive progress events
})
```

HTTP/3? Plug a QUIC `http.RoundTripper` (e.g. quic-go) into `Options.Transport` — the library stays dependency-free.

## CLI

The repo ships a `dl` command (separate module) with multi-bar progress:

```bash
# go install ...@latest cannot build cmd/dl (its go.mod carries a local
# replace directive), so build from a clone:
git clone https://github.com/blacktop/go-download.git
cd go-download/cmd/dl && go install .

dl -p 8 --sha256 020a1e8... https://dl.google.com/go/go1.26.7.darwin-arm64.tar.gz
```

Interrupted? Run the same command again — it resumes.

## License

MIT License - see [LICENSE](LICENSE) for details.
