package download

import (
	"errors"
	"fmt"
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
