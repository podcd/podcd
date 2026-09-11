// Package atomicfile writes files without ever leaving a half-written one behind.
//
// The agent writes unit files and state files while systemd and Podman may be
// reading them. A torn write there is a broken host, so every write lands as a
// rename over the target.
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// Write creates or replaces path with data, atomically, with the given mode.
func Write(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeded

	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	// Persist the rename itself, so a power cut cannot lose a unit file that
	// the agent has already reported as applied.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
