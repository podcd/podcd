package atomicfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteCreatesDirectoriesAndSetsTheMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "secret.env")
	if err := Write(path, []byte("TOKEN=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", info.Mode().Perm())
	}
	if got, _ := os.ReadFile(path); string(got) != "TOKEN=x\n" {
		t.Errorf("content = %q", got)
	}
}

func TestWriteReplacesWholeFilesAndLeavesNoTempBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unit.container")
	if err := Write(path, []byte(strings.Repeat("long line\n", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte("short\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "short\n" {
		t.Errorf("a shorter write must not leave the old tail behind: %q", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temp files were left behind: %v", entries)
	}
}
