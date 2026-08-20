// Package download fetches large files over HTTP as fast as possible using
// aria2-style techniques: the file is split into ranges downloaded over
// parallel HTTP/1.1 connections, remaining work is dynamically re-split so
// fast connections steal the tail of slow ones, and when the host resolves to
// multiple CDN edge nodes each connection is pinned to a specific node whose
// throughput is measured so statistically bad nodes are abandoned for better
// ones. Interrupted downloads resume automatically from a sidecar state file.
//
// The package has no dependencies outside the standard library.
//
//	dl, err := download.New(nil) // defaults: 8 parts, resume, silent
//	if err != nil { ... }
//	res, err := dl.Get(ctx, "https://example.com/big.ipsw", "")
package download
