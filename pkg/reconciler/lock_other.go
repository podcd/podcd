//go:build !unix

package reconciler

import "errors"

// Lock is unavailable outside unix. podcd targets Linux hosts; this file exists
// only so the packages still build elsewhere for development.
type Lock struct{}

// Acquire always fails on unsupported platforms.
func Acquire(string) (*Lock, error) {
	return nil, errors.New("podcd reconcile locking requires a unix host")
}

// Release is a no-op.
func (l *Lock) Release() error { return nil }
