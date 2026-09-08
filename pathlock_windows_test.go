//go:build windows

package download

import (
	"path/filepath"
	"testing"
)

func TestDestinationLockCaseAliases(t *testing.T) {
	dir := t.TempDir()
	unlock, err := acquireDestination(t.Context(), filepath.Join(dir, "File.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	other, contended, err := tryAcquireDestination(t.Context(), filepath.Join(dir, "file.bin"))
	if other != nil {
		defer other()
	}
	if err != nil || !contended {
		t.Fatalf("case alias got a separate lock: contended=%v err=%v", contended, err)
	}
}
