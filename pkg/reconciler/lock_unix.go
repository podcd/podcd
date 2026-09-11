//go:build unix

package reconciler

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Lock is an advisory lock held while a reconcile is applying changes.
//
// Two reconciles at once would race on the same unit files. The lock is held on
// an open file descriptor, so it is released by the kernel if the agent is killed.
type Lock struct{ f *os.File }

// Acquire takes the reconcile lock without waiting.
func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another reconcile is already running (lock %s held): %w", path, err)
	}
	return &Lock{f: f}, nil
}

// Release drops the lock.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	closeErr := l.f.Close()
	l.f = nil
	if err != nil {
		return err
	}
	return closeErr
}
