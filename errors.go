package download

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

var (
	// ErrMaxRetry is returned when a chunk exhausts its retry budget.
	ErrMaxRetry = errors.New("max retries exceeded")

	// ErrDestExists is returned when the destination file already exists
	// and Options.Overwrite is false.
	ErrDestExists = errors.New("destination exists")

	// ErrLocked is returned when another process holds the staging lock or
	// changed the .part pathname while this process was acquiring it.
	ErrLocked = errors.New("staging file locked by another process")

	// errFlockUnsupported marks platforms/filesystems where the advisory
	// staging lock cannot be enforced; downloads proceed unprotected.
	errFlockUnsupported = errors.New("staging lock unsupported")

	errURLDetailsRedacted = errors.New("request failed (url details redacted)")
)

// ContentTypeError is returned when the initial response's media type matches
// Options.RejectContentTypes. Nothing was written to disk.
type ContentTypeError struct {
	ContentType string // the offending Content-Type header value
}

func (e *ContentTypeError) Error() string {
	return fmt.Sprintf("rejected content type %q", e.ContentType)
}

// StatusError is an unexpected, non-retryable HTTP status code.
type StatusError int

func (e StatusError) Error() string {
	return fmt.Sprintf("unexpected HTTP status: %d", int(e))
}

// ChecksumError is returned when the downloaded file does not match the
// expected SHA-256/SHA-1. The fully-downloaded bytes are retained at Path
// (published checksums are sometimes simply wrong). For resumable downloads
// (multipart with a server validator) a rerun with a corrected checksum — or
// none — finalizes from the staged file without refetching content; a rerun
// with the same wrong checksum avoids the refetch but still rehashes the
// staged file. Single-stream and validator-less downloads cannot reuse the
// staged bytes and download again.
type ChecksumError struct {
	Algo     string // "sha256" or "sha1"
	Expected string
	Actual   string
	Path     string // retained staging file
}

func (e *ChecksumError) Error() string {
	algo := e.Algo
	if algo == "" {
		algo = "sha256"
	}
	msg := fmt.Sprintf("%s mismatch: expected %s, got %s", algo, e.Expected, e.Actual)
	if e.Path != "" {
		msg += fmt.Sprintf(" (downloaded bytes retained at %s)", e.Path)
	}
	return msg
}

// SizeError is returned when the final file size does not match the size
// advertised by the server.
type SizeError struct {
	Expected int64
	Actual   int64
}

func (e *SizeError) Error() string {
	return fmt.Sprintf("size mismatch: expected %d bytes, got %d", e.Expected, e.Actual)
}

// RedactURL strips credentials from a URL so callers can safely embed it in
// their own errors and logs: userinfo, query (signed URLs carry access keys
// there), and fragment are removed; unparseable input is replaced wholesale.
// This is the same redaction the package applies to its own errors.
func RedactURL(raw string) string { return redactURL(raw) }

// redactURL strips credentials from a URL before it lands in an error or a
// log line: userinfo, query (signed URLs carry access keys there —
// url.URL.Redacted only hides the password), and fragment. An unparseable
// input, opaque URL, or non-hierarchical HTTP URL is replaced wholesale since
// it cannot be safely partitioned.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || invalidHTTPURLShape(u) {
		return "(redacted invalid url)"
	}
	if !strings.ContainsAny(raw, "@?#") {
		return raw // nothing that can carry credentials
	}
	u.User = nil
	if u.RawQuery != "" {
		u.RawQuery = "REDACTED"
	}
	u.Fragment = ""
	return u.String()
}

// redactErr rewrites the URL field of the *url.Error in err's chain. Some
// net/http failures, notably malformed redirects, have already rendered a
// peer-controlled URL into the inner error text. Replace those messages so
// the sensitive text is not reachable through the returned error chain.
func redactErr(err error) error {
	if uerr, ok := errors.AsType[*url.Error](err); ok {
		uerr.URL = redactURL(uerr.URL)
		if uerr.Err != nil && containsUnsafeURLDetails(uerr.Err.Error()) {
			uerr.Err = errURLDetailsRedacted
		}
	}
	return err
}

func containsUnsafeURLDetails(message string) bool {
	message = strings.ToLower(message)
	return strings.ContainsAny(message, "@?#") ||
		strings.Contains(message, "http:") || strings.Contains(message, "https:")
}

func invalidHTTPURLShape(u *url.URL) bool {
	if u == nil {
		return true
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return u.Opaque != "" || u.Host == ""
	default:
		return false
	}
}

// parseURL is url.Parse with a redacted failure and rejects HTTP URL shapes
// that net/http cannot request safely.
func parseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, redactErr(err)
	}
	if invalidHTTPURLShape(u) {
		return nil, errors.New("invalid URL: HTTP URLs must be hierarchical and include a host")
	}
	return u, nil
}
