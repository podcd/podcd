package podman

// Networks follow the same shape as applications: podcd writes a Quadlet
// .network unit, systemd runs the oneshot service Quadlet generates from it,
// and that service is what calls `podman network create`. The one thing
// Quadlet does not do is change a network that exists - `--ignore` keeps
// whatever is there - or remove one when its unit goes away. Those two are
// the podman calls below.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/podcd/podcd/internal/atomicfile"
	"github.com/podcd/podcd/pkg/model"
	"github.com/podcd/podcd/pkg/renderer"
)

// labelNetwork is the label a podcd-created network carries, so one whose
// unit file is gone is still recognised as ours.
const labelNetwork = "io.podcd.network"

// inspectNetworks fills state.Networks: the .network units on disk, whether
// podman has each network, and any labelled network with no unit left.
func (r *Runtime) inspectNetworks(ctx context.Context, state *model.ActualState) error {
	state.Networks = map[string]model.ActualNetwork{}

	entries, err := os.ReadDir(r.unitDir)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading unit directory %s: %w", r.unitDir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name, ok := renderer.NetworkFromFileName(e.Name())
		if !ok {
			continue
		}
		path := filepath.Join(r.unitDir, e.Name())
		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		m := renderer.ParseMarkers(content)
		if m.Network != "" {
			name = m.Network
		}
		if prev, ok := state.Networks[name]; ok {
			return fmt.Errorf("network %q has two unit files: %s and %s; remove one", name, prev.UnitFile, path)
		}
		state.Networks[name] = model.ActualNetwork{
			Name:         name,
			Managed:      m.Managed,
			UnitFile:     path,
			UnitFileHash: model.HashBytes(content),
			UnitContent:  content,
			SpecHash:     m.SpecHash,
			UnitName:     renderer.NetworkServiceName(name),
			UnitState:    model.UnitUnknown,
		}
	}

	existing, err := r.listNetworks(ctx)
	if err != nil {
		return err
	}
	for name, ours := range existing {
		cur, ok := state.Networks[name]
		if !ok {
			if !ours {
				continue // somebody else's network, or podman's own
			}
			// Labelled as ours but its unit is gone: still ours to remove.
			cur = model.ActualNetwork{Name: name, Managed: true, UnitName: renderer.NetworkServiceName(name), UnitState: model.UnitMissing}
		}
		cur.Exists = true
		state.Networks[name] = cur
	}
	return r.fillNetworkUnitStates(ctx, state)
}

// listNetworks asks podman for every network, and reports for each whether
// it carries podcd's label.
func (r *Runtime) listNetworks(ctx context.Context) (map[string]bool, error) {
	out, err := r.podmanRun(ctx, "network", "ls", "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("listing networks: %w", err)
	}
	var raw []struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	}
	trimmed := strings.TrimSpace(out)
	if trimmed == "" || trimmed == "null" {
		return map[string]bool{}, nil
	}
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return nil, fmt.Errorf("parsing podman network ls output: %w", err)
	}
	result := make(map[string]bool, len(raw))
	for _, n := range raw {
		result[n.Name] = n.Labels[labelNetwork] != ""
	}
	return result, nil
}

// fillNetworkUnitStates asks systemd about every network unit in one call.
func (r *Runtime) fillNetworkUnitStates(ctx context.Context, state *model.ActualState) error {
	names := state.NetworkNames()
	if len(names) == 0 {
		return nil
	}
	args := []string{"show", "--property=Id", "--property=ActiveState", "--property=LoadState"}
	for _, n := range names {
		args = append(args, renderer.NetworkServiceName(n))
	}
	out, err := r.systemctlRun(ctx, args...)
	if err != nil {
		return fmt.Errorf("querying systemd: %w", err)
	}
	byUnit := parseShowBlocks(out)
	for _, n := range names {
		net := state.Networks[n]
		net.UnitState = unitStateOf(byUnit[renderer.NetworkServiceName(n)])
		state.Networks[n] = net
	}
	return nil
}

// ApplyNetwork writes the unit for one network and makes systemd run it.
//
// A network whose unit podcd already wrote is being changed, and Quadlet
// cannot change a network in place: the service is stopped - which stops
// every unit that Requires= it, that is, every pod on it - the network is
// removed, and the service started again creates it fresh. A network with
// no unit yet is only created; if podman already has one of that name, from
// a hand-run `podman network create`, it is adopted as it is rather than
// torn out from under whatever is using it.
func (r *Runtime) ApplyNetwork(ctx context.Context, net model.Network) error {
	unit, err := r.rend.RenderNetwork(net)
	if err != nil {
		return err
	}
	_, hadUnit := os.Stat(unit.Path)
	recreate := hadUnit == nil

	if err := atomicfile.Write(unit.Path, unit.Content, 0o644); err != nil {
		return fmt.Errorf("writing unit for network %s: %w", net.Name, err)
	}
	if err := r.daemonReload(ctx); err != nil {
		return err
	}
	if recreate {
		if _, err := r.systemctlRun(ctx, "stop", unit.ServiceName); err != nil && !unitUnknown(err) {
			return fmt.Errorf("stopping %s: %w", unit.ServiceName, err)
		}
		if _, err := r.podmanRun(ctx, "network", "rm", net.Name); err != nil && !networkMissing(err) {
			return fmt.Errorf("removing network %s so it can be recreated: %w", net.Name, err)
		}
	}
	if _, err := r.systemctlRun(ctx, "start", unit.ServiceName); err != nil {
		return fmt.Errorf("starting %s: %w", unit.ServiceName, err)
	}
	return nil
}

// RemoveNetwork stops the network's unit, deletes it, and removes the network.
//
// It never forces: a network something still uses is an ordering mistake
// the planner is supposed to have avoided, and `--force` would take that
// something's containers with it.
func (r *Runtime) RemoveNetwork(ctx context.Context, network string) error {
	service := renderer.NetworkServiceName(network)
	if _, err := r.systemctlRun(ctx, "stop", service); err != nil && !unitUnknown(err) {
		return fmt.Errorf("stopping %s: %w", service, err)
	}
	if err := removeIfExists(filepath.Join(r.unitDir, renderer.NetworkFileName(network))); err != nil {
		return fmt.Errorf("removing unit for network %s: %w", network, err)
	}
	if err := r.daemonReload(ctx); err != nil {
		return err
	}
	if _, err := r.podmanRun(ctx, "network", "rm", network); err != nil && !networkMissing(err) {
		return fmt.Errorf("removing network %s: %w", network, err)
	}
	return nil
}

// unitUnknown reports a systemctl error that means "no such unit": already
// stopped, as far as stopping is concerned.
func unitUnknown(err error) bool {
	return strings.Contains(err.Error(), "not loaded") || strings.Contains(err.Error(), "not found")
}

// networkMissing reports podman's error for a network that does not exist.
func networkMissing(err error) bool {
	return strings.Contains(err.Error(), "network not found") || strings.Contains(err.Error(), "no such network")
}
