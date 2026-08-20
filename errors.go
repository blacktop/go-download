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

	errURLDetailsRedacted = errors.New("request failed (url details redacted)")
)

// StatusError is an unexpected, non-retryable HTTP status code.
type StatusError int

func (e StatusError) Error() string {
	return fmt.Sprintf("unexpected HTTP status: %d", int(e))
}

// ChecksumError is returned when the downloaded file does not match
// Options.ExpectedSHA256 or Options.ExpectedSHA1.
type ChecksumError struct {
	Algo     string // "sha256" or "sha1"
	Expected string
	Actual   string
}

func (e *ChecksumError) Error() string {
	algo := e.Algo
	if algo == "" {
		algo = "sha256"
	}
	return fmt.Sprintf("%s mismatch: expected %s, got %s", algo, e.Expected, e.Actual)
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
