//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package download

import (
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestClassifyFlockErr(t *testing.T) {
	t.Parallel()
	if err := classifyFlockErr(nil, "x"); err != nil {
		t.Errorf("nil error classified as %v", err)
	}
	for _, e := range []error{syscall.EWOULDBLOCK, syscall.EAGAIN} {
		if err := classifyFlockErr(e, "x"); !errors.Is(err, ErrLocked) {
			t.Errorf("%v classified as %v, want ErrLocked", e, err)
		}
	}
	for _, e := range []error{syscall.ENOTSUP, syscall.EOPNOTSUPP, syscall.ENOSYS, syscall.EINVAL} {
		err := classifyFlockErr(e, "x")
		if !errors.Is(err, errFlockUnsupported) {
			t.Errorf("%v classified as %v, want errFlockUnsupported", e, err)
		}
		if errors.Is(err, ErrLocked) {
			t.Errorf("%v must not read as contention", e)
		}
	}
	// Anything else fails closed: neither locked nor unsupported.
	err := classifyFlockErr(syscall.EIO, "x")
	if err == nil || errors.Is(err, ErrLocked) || errors.Is(err, errFlockUnsupported) {
		t.Errorf("EIO classified as %v, want a hard failure", err)
	}
}

// TestInstallHoldsLockThroughRename pins the P1 fix: on flock platforms the
// staged file's descriptor (and so the cross-process lock) must survive
// verification and installation — a second process must see ErrLocked right
// up until the .part name is gone.
func TestInstallHoldsLockThroughRename(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dest := filepath.Join(dir, "file.bin")
	part := dest + ".part"
	data := testData(4 << 10)
	if err := os.WriteFile(part, data, 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(part, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := lockStaging(file); err != nil {
		t.Fatal(err)
	}

	d := newDL(t, nil)
	r := &run{d: d, rep: NopReporter{}, destPath: dest, partPath: part, total: int64(len(data))}
	res, err := r.verifyAndFinalize(file, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != dest {
		t.Errorf("installed path = %q", res.Path)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("destination missing after install: %v", err)
	}
	// The descriptor must still be open (lock held until the caller's
	// deferred Close): a second Close must be the FIRST close.
	if err := file.Close(); err != nil {
		t.Errorf("descriptor was closed before install finished: %v", err)
	}
}

func TestLockStagingConflict(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "f.part")
	f1, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f1.Close()
	if err := lockStaging(f1); err != nil {
		t.Fatalf("first lock: %v", err)
	}

	f2, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f2.Close()
	if err := lockStaging(f2); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock = %v, want ErrLocked", err)
	}

	// Releasing the first descriptor frees the lock.
	f1.Close()
	if err := lockStaging(f2); err != nil {
		t.Fatalf("lock after release: %v", err)
	}
}

func TestLockStagingRejectsReplacedPath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		recreate bool
	}{
		{name: "removed"},
		{name: "recreated", recreate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "f.part")
			if err := os.WriteFile(path, []byte("old staging inode"), 0o644); err != nil {
				t.Fatal(err)
			}

			owner, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			if err := lockStaging(owner); err != nil {
				t.Fatal(err)
			}

			stale, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer stale.Close()
			if err := os.Rename(path, filepath.Join(dir, "installed")); err != nil {
				t.Fatal(err)
			}
			if tc.recreate {
				if err := os.WriteFile(path, []byte("new staging inode"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := owner.Close(); err != nil {
				t.Fatal(err)
			}

			if err := lockStaging(stale); !errors.Is(err, ErrLocked) {
				t.Fatalf("lock on replaced staging path = %v, want ErrLocked", err)
			}
		})
	}
}

func TestGetRefusesLockedStagingFile(t *testing.T) {
	t.Parallel()
	data := testData(64 << 10)
	var st stats
	srv := httptest.NewServer(rangeHandler(data, `"v1"`, &st))
	t.Cleanup(srv.Close)

	dest := filepath.Join(t.TempDir(), "file.bin")
	// Simulate another process holding the staging lock.
	other, err := os.OpenFile(dest+".part", os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := lockStaging(other); err != nil {
		t.Fatal(err)
	}

	d := newDL(t, &Options{MinPartSize: 4 << 10})
	if _, err := d.Get(t.Context(), srv.URL+"/file.bin", dest); !errors.Is(err, ErrLocked) {
		t.Fatalf("Get on a locked staging file = %v, want ErrLocked", err)
	}

	// Release and retry: the download proceeds normally.
	other.Close()
	if _, err := d.Get(t.Context(), srv.URL+"/file.bin", dest); err != nil {
		t.Fatalf("Get after lock release: %v", err)
	}
}
