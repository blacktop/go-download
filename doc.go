// Package download fetches large files over HTTP as fast as possible using
// aria2-style techniques: the file is split into ranges downloaded over
// parallel HTTP/1.1 connections, remaining work is dynamically re-split so
// fast connections steal the tail of slow ones, and parallelism expands only
// while it improves aggregate throughput. Interrupted downloads resume
// automatically from a sidecar state file.
//
// The package has no dependencies outside the standard library.
//
//	dl, err := download.New(nil) // defaults: 8 parts, resume, silent
//	if err != nil { ... }
//	res, err := dl.Get(ctx, "https://example.com/big.ipsw", "")
package download
