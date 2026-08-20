package download

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestRedactURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "signed query",
			in:   "https://host/file.ipsw?accessKey=secret&expires=123",
			want: "https://host/file.ipsw?REDACTED",
		},
		{
			name: "userinfo",
			in:   "https://user:pass@host/file",
			want: "https://host/file",
		},
		{
			name: "fragment",
			in:   "https://host/file#token=abc",
			want: "https://host/file",
		},
		{
			name: "everything",
			in:   "https://u:p@host/f?sig=s#frag",
			want: "https://host/f?REDACTED",
		},
		{
			name: "plain url unchanged",
			in:   "https://host/path/file.bin",
			want: "https://host/path/file.bin",
		},
		{
			name: "unparseable",
			in:   "http://bad url\x7f?key=secret",
			want: "(redacted invalid url)",
		},
		{
			name: "opaque http url",
			in:   "https:user:pass@host/path",
			want: "(redacted invalid url)",
		},
		{
			name: "hostless http url",
			in:   "https:/path?accessKey=secret",
			want: "(redacted invalid url)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := redactURL(tc.in); got != tc.want {
				t.Errorf("redactURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRedactErrHidesUnsafeInnerMessage(t *testing.T) {
	t.Parallel()
	cause := errors.New(`failed to parse Location header "https://cdn.test/%zz?accessKey=secret"`)
	uerr := &url.Error{
		Op:  "Get",
		URL: "https://origin.test/start",
		Err: cause,
	}

	got := redactErr(uerr)
	msg := got.Error()
	if strings.Contains(msg, "secret") {
		t.Fatalf("redacted error still leaks inner URL details: %q", got)
	}
	if !strings.Contains(msg, "url details redacted") {
		t.Fatalf("redacted error does not explain the omitted details: %q", got)
	}
	if errors.Is(got, cause) {
		t.Fatal("unsafe inner error remains reachable after redaction")
	}
}

func TestRedactErrRewritesURLErrors(t *testing.T) {
	t.Parallel()
	uerr := &url.Error{
		Op:  "Get",
		URL: "https://host/f?accessKey=secret",
		Err: errors.New("connection refused"),
	}
	// Redact at the source, then wrap — the order every call site uses
	// (fmt.Errorf renders eagerly, so wrapping first would freeze the leak).
	wrapped := fmt.Errorf("outer: %w", redactErr(uerr))
	got := wrapped.Error()
	if strings.Contains(got, "secret") {
		t.Errorf("redacted error still leaks the query: %q", got)
	}
	if !strings.Contains(got, "https://host/f?REDACTED") {
		t.Errorf("redacted error lost the URL context: %q", got)
	}
	// The url.Error must still be findable in the chain after redaction.
	if _, ok := errors.AsType[*url.Error](wrapped); !ok {
		t.Error("redaction broke the error chain")
	}
}

func TestGetErrorNeverLeaksSignedQuery(t *testing.T) {
	t.Parallel()
	// A transport that fails instantly and cancels the context so elect
	// returns after one attempt instead of sleeping through its backoff.
	ctx, cancel := context.WithCancel(t.Context())
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		cancel()
		return nil, errors.New("connection refused")
	})
	d := newDL(t, &Options{Transport: rt})
	_, err := d.Get(ctx,
		"http://cdn.test/file.ipsw?accessKey=secret&Signature=alsosecret", "")
	if err == nil {
		t.Fatal("expected connection error")
	}
	msg := err.Error()
	if strings.Contains(msg, "secret") {
		t.Fatalf("error leaks signed query params: %q", msg)
	}
	if !strings.Contains(msg, "/file.ipsw") {
		t.Errorf("error lost useful URL context: %q", msg)
	}
}

func TestGetErrorNeverLeaksUnparseableURL(t *testing.T) {
	t.Parallel()
	// url.Parse fails on the control character; the *url.Error it returns
	// re-renders the raw URL through %w unless redacted at the source.
	d := newDL(t, nil)
	_, err := d.Get(t.Context(), "http://host/\x7f?accessKey=secret", "")
	if err == nil {
		t.Fatal("expected parse error")
	}
	if msg := err.Error(); strings.Contains(msg, "secret") {
		t.Fatalf("parse error leaks signed query params: %q", msg)
	}
}

func TestGetErrorNeverLeaksMalformedRedirect(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://cdn.test/%zz?accessKey=secret")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	d := newDL(t, nil)
	_, err := d.Get(t.Context(), srv.URL+"/start", "")
	if err == nil {
		t.Fatal("expected malformed redirect error")
	}
	msg := err.Error()
	if strings.Contains(msg, "secret") || strings.Contains(msg, "accessKey") {
		t.Fatalf("redirect error leaks signed query params: %q", msg)
	}
}

func TestGetErrorNeverLeaksNonHierarchicalURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		rawURL string
	}{
		{name: "opaque", rawURL: "https:user:pass@host/path"},
		{name: "missing host", rawURL: "https:/user:pass/path?accessKey=secret"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newDL(t, nil)
			_, err := d.Get(t.Context(), tc.rawURL, "")
			if err == nil {
				t.Fatal("expected invalid URL error")
			}
			msg := err.Error()
			if strings.Contains(msg, "user:pass") || strings.Contains(msg, "accessKey") ||
				strings.Contains(msg, "host/path") {
				t.Fatalf("invalid URL error leaks credentials: %q", msg)
			}
		})
	}
}
