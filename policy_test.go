package download

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

// TestPolicyAppliesBeforeResumeScheduling pins the ordering contract: the
// byte-serving URL's policy is resolved before a matching sidecar is turned
// into scheduler work. A policy-raised floor must therefore split only the
// missing tail, not fall back to the Options-level single worker or restart
// already staged bytes.
func TestPolicyAppliesBeforeResumeScheduling(t *testing.T) {
	t.Parallel()
	data := testData(1 << 20)
	var st stats
	tail := int64(len(data)) - 64<<10
	var arrivals atomic.Int32
	var barrierTimedOut atomic.Bool
	var releaseOnce sync.Once
	release := make(chan struct{})
	serveRange := rangeHandler(data, `"v1"`, &st)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if start, ok := parseRangeStart(r.Header.Get("Range")); ok && start >= tail {
			if arrivals.Add(1) >= 4 {
				releaseOnce.Do(func() { close(release) })
			}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			case <-time.After(time.Second):
				barrierTimedOut.Store(true)
			}
		}
		serveRange.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	rawURL := srv.URL + "/artifact.bin?credential=refreshed"
	sourceURL, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "artifact.bin")
	const resumeID = "stable-artifact"
	seedInterruptedDownload(t, dest, data, resumeIdentity(resumeID, sourceURL))

	var policyURL string
	d := newDL(t, &Options{
		Parts: 1, MinPartSize: 1 << 20,
		Policy: func(finalURL string) Concurrency {
			policyURL = finalURL
			return Concurrency{Parts: 4, MinParts: 4, MinPartSize: 8 << 10}
		},
	})
	res, err := d.Do(t.Context(), &Request{URL: rawURL, Dest: dest, ResumeID: resumeID})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Resumed {
		t.Fatal("matching sidecar was not resumed")
	}
	if policyURL != rawURL {
		t.Fatalf("policy URL = %q, want byte-serving URL %q", policyURL, rawURL)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("policy-governed resume installed different bytes")
	}
	starts := st.rangeStarts()
	if barrierTimedOut.Load() || arrivals.Load() < 4 || len(starts) < 5 || starts[0] != 0 {
		t.Fatalf("range starts = %v arrivals=%d barrier_timeout=%t, want election plus four concurrent resumed-tail ranges",
			starts, arrivals.Load(), barrierTimedOut.Load())
	}
	for _, start := range starts[1:] {
		if start < tail {
			t.Fatalf("policy-governed resume restarted staged bytes at %d before tail %d", start, tail)
		}
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
