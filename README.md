# go-download

[![Go](https://github.com/blacktop/go-download/actions/workflows/go.yml/badge.svg)](https://github.com/blacktop/go-download/actions/workflows/go.yml) [![Go Reference](https://pkg.go.dev/badge/github.com/blacktop/go-download.svg)](https://pkg.go.dev/github.com/blacktop/go-download) [![License](http://img.shields.io/:license-mit-blue.svg)](http://doge.mit-license.org)

> Golang download manager package.

---

## Install

```bash
$ go get github.com/blacktop/go-download
```

## Getting Started

```go
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"    

    "github.com/blacktop/go-download"
)

func main() {
    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    log := slog.NewJSONHandler(os.Stdout, nil)

    mgr, err := download.New(&download.Config{
        Context:  ctx,
        Logger:   log,
        Progress: true,
        Parts:    4,
    })
    if err != nil {
		log.Error(err.Error())
		os.Exit(1)
    }

    if err := mgr.Get(os.Args[1]); err != nil {
		log.Error(err.Error())
		os.Exit(1)
    }
}
```

## License

MIT Copyright (c) 2024 **blacktop**