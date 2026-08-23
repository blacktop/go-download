package download

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// TestPolicySeesByteServingHost: the policy is consulted with the
// post-redirect URL and its result governs the run: MinParts == Parts opens
// every connection eagerly, which the election-only host never sees.
func TestPolicySeesByteServingHost(t *testing.T) {
	t.Parallel()
	const minPart = 64 << 10
	data := testData(4 * minPart)
	var st stats
	origin := httptest.NewServer(rangeHandler(data, `"v1"`, &st))
	t.Cleanup(origin.Close)
	var frontHits atomic.Int32
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		frontHits.Add(1)
		http.Redirect(w, r, origin.URL+"/cdn/file.bin", http.StatusFound)
	}))
	t.Cleanup(front.Close)

	var seen []string
	d := newDL(t, &Options{Parts: 4, MinParts: 1, MinPartSize: minPart,
		Policy: func(finalURL string) Concurrency {
			seen = append(seen, finalURL)
			if strings.HasPrefix(finalURL, origin.URL) {
				return Concurrency{MinParts: 4}
			}
			return Concurrency{}
		}})
	dest := filepath.Join(t.TempDir(), "file.bin")
	res, got := mustGet(t, d, front.URL+"/file.bin", dest)
	if !bytes.Equal(got, data) {
		t.Fatal("downloaded bytes differ from source")
	}
	if len(seen) != 1 || seen[0] != origin.URL+"/cdn/file.bin" {
		t.Fatalf("policy consulted with %v, want the byte-serving URL once", seen)
	}
	if res.FinalURL != origin.URL+"/cdn/file.bin" {
		t.Fatalf("Result.FinalURL = %q, want the byte-serving URL", res.FinalURL)
	}
	if hits := frontHits.Load(); hits != 1 {
		t.Fatalf("redirecting front end saw %d requests, want the election only", hits)
	}
	// Four eager flows: every sibling range request is issued, which only a
	// MinParts == Parts policy can produce without throughput measurement on
	// a 256 KiB object.
	if starts := st.rangeStarts(); len(starts) != 4 {
		t.Fatalf("origin saw range starts %v, want four eager ranges", starts)
	}
}

func TestPolicyZeroFieldsKeepOptions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		policy Concurrency
		want   Concurrency
	}{
		{name: "all zero keeps options", policy: Concurrency{},
			want: Concurrency{Parts: 6, MinParts: 3, MinPartSize: 1 << 20}},
		{name: "lowered parts clamps inherited floor",
			policy: Concurrency{Parts: 2, MinPartSize: 4 << 20},
			want:   Concurrency{Parts: 2, MinParts: 2, MinPartSize: 4 << 20}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			policy := tc.policy
			d := newDL(t, &Options{Parts: 6, MinParts: 3, MinPartSize: 1 << 20,
				Policy: func(string) Concurrency { return policy }})
			conc, err := d.concurrencyFor("https://example.org/x")
			if err != nil {
				t.Fatal(err)
			}
			if conc != tc.want {
				t.Fatalf("concurrency = %+v, want %+v", conc, tc.want)
			}
		})
	}
}

// TestInvalidPolicyFailsAfterElection: a policy result that violates the
// concurrency invariants aborts the download after the election (itself a
// ranged GET) and before any additional range request.
func TestInvalidPolicyFailsAfterElection(t *testing.T) {
	t.Parallel()
	data := testData(256 << 10)
	var st stats
	srv := httptest.NewServer(rangeHandler(data, `"v1"`, &st))
	t.Cleanup(srv.Close)
	d := newDL(t, &Options{Parts: 4, MinPartSize: 64 << 10,
		Policy: func(string) Concurrency { return Concurrency{MinParts: 8} }})
	_, err := d.Get(t.Context(), srv.URL+"/file.bin", filepath.Join(t.TempDir(), "f"))
	if err == nil || !strings.Contains(err.Error(), "policy for") ||
		!strings.Contains(err.Error(), "MinParts") {
		t.Fatalf("invalid policy error = %v, want a policy validation failure", err)
	}
	if n := len(st.rangeHeaders()); n != 1 {
		t.Fatalf("server saw %d requests, want only the election", n)
	}
}

func TestNormalizeHost(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"CDN.Apple.com": "cdn.apple.com", "cdn.test.": "cdn.test", " cdn.test ": "cdn.test",
		"cdn.test..": "cdn.test.", "": "",
	} {
		if got := NormalizeHost(in); got != want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}
