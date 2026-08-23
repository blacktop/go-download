package download

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"slices"
)

const stateVersion = 2

// chunkState is one incomplete range in the resume sidecar. [Off+Done, End)
// still needs downloading; ranges of the file not covered by any chunkState
// are fully written.
type chunkState struct {
	Off  int64 `json:"off"`
	End  int64 `json:"end"`
	Done int64 `json:"done"`
}

// stateFile is the sidecar written next to the .part file so an interrupted
// download can resume.
type stateFile struct {
	Version      int          `json:"v"`
	SourceID     string       `json:"source_id"`
	Size         int64        `json:"size"`
	ETag         string       `json:"etag,omitempty"`
	LastModified string       `json:"last_modified,omitempty"`
	Chunks       []chunkState `json:"chunks"`
}

// sourceIdentity binds resume data to the caller's original resource URL,
// rather than a possibly short-lived signed redirect target. Hashing avoids
// persisting URL credentials or query tokens in the sidecar.
func sourceIdentity(source *url.URL) string {
	if source == nil {
		return ""
	}
	u := *source
	u.Fragment = ""
	u.RawFragment = ""
	sum := sha256.Sum256([]byte(u.String()))
	return hex.EncodeToString(sum[:])
}

// resumeIdentity is the sidecar identity for a run: the caller's ResumeID
// when set (domain-separated so it can never collide with a URL-derived
// identity), otherwise the request URL's.
func resumeIdentity(resumeID string, source *url.URL) string {
	if resumeID == "" {
		return sourceIdentity(source)
	}
	sum := sha256.Sum256([]byte("resume-id\x00" + resumeID))
	return hex.EncodeToString(sum[:])
}

func statePath(partPath string) string { return partPath + ".json" }

// save writes the sidecar atomically (tmp file + rename).
func (st *stateFile) save(path string) error {
	data, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write state %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename state: %w", err)
	}
	return nil
}

// loadState reads and structurally validates a sidecar. It returns nil (no
// error) when the sidecar is missing, unreadable, or invalid — resume is
// best-effort and a bad sidecar just means a fresh start.
func loadState(path string) *stateFile {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var st stateFile
	if err := json.Unmarshal(data, &st); err != nil {
		return nil
	}
	if st.Version != stateVersion || len(st.SourceID) != sha256.Size*2 || st.Size <= 0 {
		return nil
	}
	if st.ETag == "" && st.LastModified == "" {
		return nil // no validator: cannot prove the remote content is unchanged
	}
	chunks := slices.Clone(st.Chunks)
	slices.SortFunc(chunks, func(a, b chunkState) int {
		return cmp.Compare(a.Off, b.Off)
	})
	var prevEnd int64 = -1
	for _, c := range chunks {
		if c.Off < 0 || c.End > st.Size || c.Off >= c.End || c.Done < 0 || c.Done > c.End-c.Off {
			return nil
		}
		if c.Off < prevEnd {
			return nil // overlapping chunks
		}
		prevEnd = c.End
	}
	return &st
}

// usable reports whether the locked .part file is consistent with the sidecar
// and with the validators observed from the server right now.
func (st *stateFile) usable(
	part *os.File, sourceID string, size int64, etag, lastModified string,
) bool {
	if st.SourceID != sourceID || st.Size != size {
		return false
	}
	// Mirror run.validator: a strong ETag proves the content unchanged;
	// otherwise fall back to Last-Modified (a weak ETag validates nothing).
	if isStrongETag(st.ETag) {
		if st.ETag != etag {
			return false
		}
	} else if st.LastModified == "" || st.LastModified != lastModified {
		return false
	}
	fi, err := part.Stat()
	if err != nil {
		return false
	}
	return fi.Size() == size // preallocation intact
}

func isStrongETag(etag string) bool {
	return len(etag) >= 2 && etag[0] == '"'
}

// remaining returns the number of bytes still to download.
func (st *stateFile) remaining() int64 {
	var n int64
	for _, c := range st.Chunks {
		n += (c.End - c.Off) - c.Done
	}
	return n
}
