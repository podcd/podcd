//go:build unix

package reconciler

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLockIsExclusiveAndReleasable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "reconcile.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(path); err == nil || !strings.Contains(err.Error(), "another reconcile") {
		t.Fatalf("a second holder must be refused with a clear message, got %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(path)
	if err != nil {
		t.Fatalf("the lock should be free after release: %v", err)
	}
	defer second.Release()
	if err := first.Release(); err != nil {
		t.Errorf("releasing twice is harmless: %v", err)
	}
}
