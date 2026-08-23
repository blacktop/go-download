package download

import (
	"bytes"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResumeIdentity(t *testing.T) {
	t.Parallel()
	a, err := url.Parse("https://example.org/app.ipa?accessKey=one")
	if err != nil {
		t.Fatal(err)
	}
	b, err := url.Parse("https://example.org/app.ipa?accessKey=two")
	if err != nil {
		t.Fatal(err)
	}
	if resumeIdentity("", a) != sourceIdentity(a) {
		t.Error("empty ResumeID must keep the URL-derived identity")
	}
	if resumeIdentity("", a) == resumeIdentity("", b) {
		t.Error("without ResumeID, different queries must stay different resources")
	}
	if resumeIdentity("app", a) != resumeIdentity("app", b) {
		t.Error("ResumeID must make rotating query credentials the same resource")
	}
	if resumeIdentity("app", a) == resumeIdentity("other", a) {
		t.Error("different ResumeIDs must be different resources")
	}
	if resumeIdentity(a.String(), a) == sourceIdentity(a) {
		t.Error("a ResumeID equal to the URL text must not collide with the URL-derived identity")
	}
	if id := resumeIdentity("app", a); len(id) != 64 || strings.Contains(id, "app") {
		t.Errorf("ResumeID identity %q must be an opaque 64-hex digest", id)
	}
}

// seedInterruptedDownload stages a mostly complete .part and its sidecar
// under identity id, as an interrupted run would leave them.
func seedInterruptedDownload(t *testing.T, dest string, data []byte, id string) {
	t.Helper()
	part := dest + ".part"
	staged := make([]byte, len(data))
	copy(staged, data)
	chunks := []chunkState{{Off: int64(len(data)) - 64<<10, End: int64(len(data)), Done: 0}}
	for i := chunks[0].Off; i < chunks[0].End; i++ {
		staged[i] = 0
	}
	if err := os.WriteFile(part, staged, 0o644); err != nil {
		t.Fatal(err)
	}
	side := &stateFile{Version: stateVersion, SourceID: id, Size: int64(len(data)),
		ETag: `"v1"`, Chunks: chunks}
	if err := side.save(statePath(part)); err != nil {
		t.Fatal(err)
	}
}

// TestResumeIDSurvivesRotatedCredentials: an interrupted download under one
// signed URL resumes under a refreshed one when ResumeID names the object;
// without ResumeID the refreshed URL is a different resource and restarts.
func TestResumeIDSurvivesRotatedCredentials(t *testing.T) {
	t.Parallel()
	data := testData(1 << 20)
	for _, tc := range []struct {
		name       string
		resumeID   string
		wantResume bool
	}{
		{name: "resume id carries across query rotation", resumeID: "app.ipa", wantResume: true},
		{name: "default identity restarts on query rotation", wantResume: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var st stats
			srv := httptest.NewServer(rangeHandler(data, `"v1"`, &st))
			t.Cleanup(srv.Close)
			old, err := url.Parse(srv.URL + "/app.ipa?accessKey=expired")
			if err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(t.TempDir(), "app.ipa")
			seedInterruptedDownload(t, dest, data, resumeIdentity(tc.resumeID, old))

			d := newDL(t, &Options{Parts: 2, MinPartSize: 16 << 10, ResumeID: tc.resumeID})
			res, got := mustGet(t, d, srv.URL+"/app.ipa?accessKey=refreshed", dest)
			if !bytes.Equal(got, data) {
				t.Fatal("downloaded bytes differ from source")
			}
			if res.Resumed != tc.wantResume {
				t.Fatalf("Resumed = %t, want %t", res.Resumed, tc.wantResume)
			}
			if tc.wantResume {
				// Only the missing tail may be requested from the server; the
				// election (bytes=0-) is closed unread once the sidecar is accepted.
				for _, start := range st.rangeStarts() {
					if start != 0 && start < int64(len(data))-64<<10 {
						t.Fatalf("resumed run requested bytes from %d; want only the missing tail", start)
					}
				}
			}
		})
	}
}

// TestResumeIDStillHonoursValidators: ResumeID names the object, but a
// changed server validator still invalidates the staged bytes.
func TestResumeIDStillHonoursValidators(t *testing.T) {
	t.Parallel()
	data := testData(256 << 10)
	var st stats
	srv := httptest.NewServer(rangeHandler(data, `"v2"`, &st))
	t.Cleanup(srv.Close)
	dest := filepath.Join(t.TempDir(), "app.ipa")
	seedInterruptedDownload(t, dest, data, resumeIdentity("app.ipa", nil)) // sidecar says "v1"
	d := newDL(t, &Options{Parts: 2, MinPartSize: 16 << 10, ResumeID: "app.ipa"})
	res, got := mustGet(t, d, srv.URL+"/app.ipa?accessKey=new", dest)
	if !bytes.Equal(got, data) {
		t.Fatal("downloaded bytes differ from source")
	}
	if res.Resumed {
		t.Fatal("stale ETag must invalidate the sidecar even with a matching ResumeID")
	}
}
