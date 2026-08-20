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
// input is replaced wholesale since it cannot be safely partitioned.
func redactURL(raw string) string {
	if !strings.ContainsAny(raw, "@?#") {
		return raw // nothing that can carry credentials
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "(redacted invalid url)"
	}
	u.User = nil
	if u.RawQuery != "" {
		u.RawQuery = "REDACTED"
	}
	u.Fragment = ""
	return u.String()
}

// redactErr rewrites the URL field of the *url.Error in err's chain (both
// net/http transport errors and url.Parse failures carry the full URL there,
// signed query included); we own these error values, so mutating is safe.
// URLs frozen into inner message text are not covered, and neither are
// messages already rendered: it must run BEFORE fmt.Errorf("%w") wrapping,
// which formats eagerly.
func redactErr(err error) error {
	if uerr, ok := errors.AsType[*url.Error](err); ok {
		uerr.URL = redactURL(uerr.URL)
	}
	return err
}

// parseURL is url.Parse with a redacted failure: the returned *url.Error
// would otherwise re-render the raw URL (credentials included) through %w.
func parseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, redactErr(err)
	}
	return u, nil
}
