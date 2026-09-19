// Package runtime is the boundary between the reconciler and whatever actually
// runs containers.
//
// The interface is deliberately small. A runtime observes, applies one
// application, removes one application, restarts one application, and answers
// whether one application is healthy. Deciding *which* of those to do is the
// planner's job, not the runtime's.
package runtime

import (
	"context"
	"errors"

	"github.com/podcd/podcd/pkg/model"
)

// ErrNotImplemented is returned by runtimes that exist but are not finished.
var ErrNotImplemented = errors.New("not implemented")

// Runtime manages containerized applications on this host.
type Runtime interface {
	// Name identifies the runtime, e.g. "podman".
	Name() string

	// Available reports whether this runtime can be used here
	Available(ctx context.Context) (bool, string)

	// Inspect observes everything this runtime manages on the host.
	Inspect(ctx context.Context) (model.ActualState, error)

	// Apply makes one application exist and run as specified.
	// It must be safe to call when the application already exists and is already correct.
	Apply(ctx context.Context, app model.Application) error

	// Remove stops an application and removes its definition.
	// It never removes volumes: data outlives configuration.
	Remove(ctx context.Context, app string) error

	// Restart restarts an application that is already defined.
	Restart(ctx context.Context, app string) error

	// Health probes one application.
	Health(ctx context.Context, app model.Application) (model.Health, error)

	// Logs returns recent log output, for debugging on the host.
	Logs(ctx context.Context, app string, lines int) (string, error)

	// ApplyNetwork makes one network exist as specified. Like Apply, it is
	// safe to call when the network already exists and is already correct.
	// A network whose definition changed is recreated, which stops every
	// application on it; the planner restarts those afterwards.
	ApplyNetwork(ctx context.Context, net model.Network) error

	// RemoveNetwork removes a network's definition and the network itself.
	// It fails, rather than forces, when something still uses the network.
	RemoveNetwork(ctx context.Context, network string) error
}
