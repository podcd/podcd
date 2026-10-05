// Package planner turns (desired, actual) into an explicit list of changes.
package planner

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/podcd/podcd/pkg/model"
	"github.com/podcd/podcd/pkg/renderer"
)

// Options tunes planning decisions that are policy rather than fact.
type Options struct {
	// Prune removes managed applications Git no longer declares; otherwise they are reported as no-ops.
	Prune bool
	// Protected applications failed to compile transiently (e.g. ExternalSecret); never pruned.
	Protected map[string]bool
}

// Build computes the plan that would make actual match desired.
func Build(desired model.DesiredState, actual model.ActualState, rend *renderer.Renderer, opts Options) (model.Plan, error) {
	var plan model.Plan
	seen := map[string]bool{}

	// Networks first: recreating one takes its pods down, so they get restarted below.
	recreated, err := planNetworks(desired, actual, rend, opts, &plan)
	if err != nil {
		return model.Plan{}, err
	}

	apps := slices.Clone(desired.Applications)
	slices.SortFunc(apps, func(a, b model.Application) int { return cmp.Compare(a.Name, b.Name) })

	for i := range apps {
		app := &apps[i]
		seen[app.Name] = true
		act := func(t model.ActionType, reason string, details ...string) {
			plan.Actions = append(plan.Actions, model.Action{Type: t, App: app.Name, Reason: reason, Details: details, Application: app})
		}

		unit, err := rend.Render(*app)
		if err != nil {
			return model.Plan{}, fmt.Errorf("rendering %s: %w", app.Name, err)
		}

		cur, exists := actual.Apps[app.Name]
		switch {
		case !exists:
			act(model.ActionCreate, "not present on this host", imageDetails(*app)...)
		case !cur.Managed:
			// Somebody else owns this unit, guessing would be how you delete someone's database.
			return model.Plan{}, fmt.Errorf("application %q: unit %s exists but is not managed by podcd; "+
				"remove it by hand or restore its podcd header before reconciling", app.Name, cur.UnitFile)
		case cur.UnitFileHash != model.HashBytes(unit.Content):
			act(model.ActionUpdate, updateReason(cur, unit), changeDetails(cur, unit)...)
		case cur.UnitState == model.UnitActivating:
			act(model.ActionNoOp, "unit is starting")
		case cur.UnitState != model.UnitActive:
			act(model.ActionRestart, fmt.Sprintf("unit is %s, should be running", cmp.Or(cur.UnitState, model.UnitUnknown)))
		case len(deadContainers(cur, *app)) > 0:
			// systemd only watches the service container; a dead workload container leaves the unit active.
			act(model.ActionRestart, "container "+strings.Join(deadContainers(cur, *app), ", ")+", should be running")
		case len(changedNetworks(*app, recreated)) > 0:
			act(model.ActionRestart, "network "+strings.Join(changedNetworks(*app, recreated), ", ")+" is recreated")
		default:
			act(model.ActionNoOp, "up to date")
		}
	}

	for _, name := range actual.Names() {
		if seen[name] || opts.Protected[name] || !actual.Apps[name].Managed {
			continue // ours and still wanted, or not ours at all
		}
		if !opts.Prune {
			plan.Actions = append(plan.Actions, model.Action{Type: model.ActionNoOp, App: name, Reason: "no longer declared in Git, but pruning is disabled"})
			continue
		}
		plan.Actions = append(plan.Actions, model.Action{
			Type:        model.ActionDelete,
			App:         name,
			Reason:      "no longer declared in Git",
			Details:     []string{"stops and removes " + renderer.ServiceName(name)},
			Destructive: true,
		})
	}

	slices.SortStableFunc(plan.Actions, func(a, b model.Action) int {
		return cmp.Or(cmp.Compare(rank(a), rank(b)), cmp.Compare(a.App, b.App))
	})
	return plan, nil
}

// planNetworks adds network actions and returns those (re)created, whose pods need a restart.
func planNetworks(desired model.DesiredState, actual model.ActualState, rend *renderer.Renderer, opts Options, plan *model.Plan) (map[string]bool, error) {
	seen := map[string]bool{}
	recreated := map[string]bool{}

	nets := slices.Clone(desired.Networks)
	slices.SortFunc(nets, func(a, b model.Network) int { return cmp.Compare(a.Name, b.Name) })
	for i := range nets {
		net := &nets[i]
		seen[net.Name] = true
		act := func(t model.ActionType, reason string, details ...string) {
			plan.Actions = append(plan.Actions, model.Action{Type: t, Kind: model.KindNetwork, App: net.Name, Reason: reason, Details: details, Network: net})
		}

		unit, err := rend.RenderNetwork(*net)
		if err != nil {
			return nil, fmt.Errorf("rendering network %s: %w", net.Name, err)
		}

		cur, exists := actual.Networks[net.Name]
		switch {
		case !exists:
			// An existing hand-made network of this name is adopted by the runtime.
			act(model.ActionCreate, "not present on this host")
		case !cur.Managed:
			return nil, fmt.Errorf("network %q: unit %s exists but is not managed by podcd; "+
				"remove it by hand or restore its podcd header before reconciling", net.Name, cur.UnitFile)
		case cur.UnitFile == "":
			// Labelled as ours, unit gone: write it again.
			act(model.ActionCreate, "unit file is missing")
		case cur.UnitFileHash != model.HashBytes(unit.Content):
			recreated[net.Name] = true
			act(model.ActionUpdate, "configuration in Git changed; the network is recreated and every application on it restarted",
				unitDiff(contentLines(string(cur.UnitContent)), contentLines(string(unit.Content)))...)
		case !cur.Exists, cur.UnitState != model.UnitActive:
			act(model.ActionRestart, "network should exist")
		default:
			act(model.ActionNoOp, "up to date")
		}
	}

	// Managed networks no longer needed, unless a protected application still uses one.
	for _, name := range actual.NetworkNames() {
		cur := actual.Networks[name]
		if seen[name] || !cur.Managed || usedByProtected(name, actual, opts.Protected) {
			continue
		}
		if !opts.Prune {
			plan.Actions = append(plan.Actions, model.Action{Type: model.ActionNoOp, Kind: model.KindNetwork, App: name, Reason: "no longer needed by anything in Git, but pruning is disabled"})
			continue
		}
		plan.Actions = append(plan.Actions, model.Action{
			Type:        model.ActionDelete,
			Kind:        model.KindNetwork,
			App:         name,
			Reason:      "no longer needed by anything in Git",
			Details:     []string{"removes " + renderer.NetworkServiceName(name) + " and the podman network"},
			Destructive: true,
		})
	}
	return recreated, nil
}

// usedByProtected reports whether a protected application's on-disk unit names this network.
func usedByProtected(network string, actual model.ActualState, protected map[string]bool) bool {
	ref := "Network=" + renderer.NetworkFileName(network)
	for app := range protected {
		for _, line := range strings.Split(string(actual.Apps[app].UnitContent), "\n") {
			if strings.TrimSpace(line) == ref {
				return true
			}
		}
	}
	return false
}

// changedNetworks lists an application's networks this plan recreates, sorted.
func changedNetworks(app model.Application, recreated map[string]bool) []string {
	var out []string
	for _, n := range app.ManagedNetworks {
		if recreated[n] {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// rank orders removals before creations (freeing host ports), with networks
// between the applications leaving them and those joining them.
func rank(a model.Action) int {
	if a.Kind == model.KindNetwork {
		switch a.Type {
		case model.ActionDelete:
			return 1
		case model.ActionUpdate, model.ActionCreate, model.ActionRestart:
			return 2
		default:
			return 9
		}
	}
	switch a.Type {
	case model.ActionDelete:
		return 0
	case model.ActionUpdate:
		return 3
	case model.ActionCreate:
		return 4
	case model.ActionRestart:
		return 5
	default:
		return 9
	}
}

func imageDetails(app model.Application) []string {
	images := app.ImageList()
	out := make([]string, 0, len(images))
	for _, img := range images {
		out = append(out, "image "+img)
	}
	return append([]string{"pod with " + strconv.Itoa(len(images)) + " container(s), played by podman"}, out...)
}

// deadContainers lists non-running workload containers, init containers excluded.
func deadContainers(cur model.ActualApp, app model.Application) []string {
	var out []string
	for _, c := range cur.Containers {
		if c.State == "running" || model.IsInitContainer(app.InitContainers, c.Name) {
			continue
		}
		out = append(out, fmt.Sprintf("%s is %s", c.Name, cmp.Or(c.State, "in an unknown state")))
	}
	return out
}

func updateReason(cur model.ActualApp, unit renderer.Unit) string {
	if cur.SpecHash != unit.SpecHash {
		return "configuration in Git changed"
	}
	return "unit file differs from the rendered unit (edited by hand, or written by an older podcd)"
}

// changeDetails lists the unit and manifest lines an update changes.
func changeDetails(cur model.ActualApp, unit renderer.Unit) []string {
	details := unitDiff(contentLines(string(cur.UnitContent)), contentLines(string(unit.Content)))
	return append(details, unitDiff(manifestLines(cur.ManifestContent), manifestLines(unit.Manifest))...)
}

// manifestLines prepares a manifest for diffing, replacing Secret documents with a placeholder.
func manifestLines(manifest []byte) []string {
	if len(manifest) == 0 {
		return nil
	}
	var out []string
	for _, doc := range strings.Split(string(manifest), "\n---\n") {
		if strings.Contains(doc, "\nkind: Secret\n") || strings.HasPrefix(doc, "kind: Secret\n") {
			name := "?"
			for _, l := range strings.Split(doc, "\n") {
				if v, ok := strings.CutPrefix(l, "  name: "); ok {
					name = v
					break
				}
			}
			out = append(out, "Secret "+name+": (values redacted; hash "+model.HashBytes([]byte(doc))[:12]+")")
			continue
		}
		out = append(out, contentLines(doc)...)
	}
	return out
}

// unitDiff reports the lines that would change.
func unitDiff(oldLines, newLines []string) []string {
	if len(oldLines) == 0 {
		return nil
	}

	inOld := map[string]int{}
	for _, l := range oldLines {
		inOld[l]++
	}
	inNew := map[string]int{}
	for _, l := range newLines {
		inNew[l]++
	}

	var details []string
	for _, l := range oldLines {
		if inNew[l] == 0 {
			details = append(details, "- "+l)
			inNew[l] = -1 // report each distinct line once
		}
	}
	for _, l := range newLines {
		if inOld[l] == 0 {
			details = append(details, "+ "+l)
			inOld[l] = -1
		}
	}
	const maxDetails = 20
	if len(details) > maxDetails {
		extra := len(details) - maxDetails
		details = append(details[:maxDetails], fmt.Sprintf("... and %d more line(s)", extra))
	}
	return details
}

// contentLines drops blank lines, podcd's markers and the spec-hash label (noise in a diff).
func contentLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimRight(l, "\r")
		if strings.TrimSpace(l) == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if strings.HasPrefix(l, "Label=io.podcd.spec-hash=") {
			continue
		}
		out = append(out, l)
	}
	return out
}
