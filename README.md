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

import "github.com/blacktop/go-download"

func main() {
    d, err := download.NewDownloader("URL")
    if err != nil {
        panic(err)
    }

    d.Get()
}
```

## License

MIT Copyright (c) 2024 **blacktop**