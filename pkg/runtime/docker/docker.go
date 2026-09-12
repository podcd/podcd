// Package docker is a placeholder for Docker support.
package docker

import (
	"context"
	"fmt"

	"github.com/podcd/podcd/pkg/model"
	"github.com/podcd/podcd/pkg/runtime"
)

// Runtime is the not-yet-implemented Docker runtime.
type Runtime struct{}

// New returns a Docker runtime.
func New() *Runtime { return &Runtime{} }

// Name implements runtime.Runtime.
func (r *Runtime) Name() string { return "docker" }

// Available implements runtime.Runtime.
func (r *Runtime) Available(context.Context) (bool, string) {
	return false, "the docker runtime is not implemented yet; use runtime: podman"
}

// Inspect implements runtime.Runtime.
func (r *Runtime) Inspect(context.Context) (model.ActualState, error) {
	return model.ActualState{}, fmt.Errorf("docker runtime: %w", runtime.ErrNotImplemented)
}

// Apply implements runtime.Runtime.
func (r *Runtime) Apply(context.Context, model.Application) error {
	return fmt.Errorf("docker runtime: %w", runtime.ErrNotImplemented)
}

// Remove implements runtime.Runtime.
func (r *Runtime) Remove(context.Context, string) error {
	return fmt.Errorf("docker runtime: %w", runtime.ErrNotImplemented)
}

// Restart implements runtime.Runtime.
func (r *Runtime) Restart(context.Context, string) error {
	return fmt.Errorf("docker runtime: %w", runtime.ErrNotImplemented)
}

// Health implements runtime.Runtime.
func (r *Runtime) Health(context.Context, model.Application) (model.Health, error) {
	return model.Health{}, fmt.Errorf("docker runtime: %w", runtime.ErrNotImplemented)
}

// Logs implements runtime.Runtime.
func (r *Runtime) Logs(context.Context, string, int) (string, error) {
	return "", fmt.Errorf("docker runtime: %w", runtime.ErrNotImplemented)
}

// compile-time check that the stub really does satisfy the interface.
var _ runtime.Runtime = (*Runtime)(nil)
