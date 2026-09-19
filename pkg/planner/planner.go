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
	// Prune removes managed applications Git no longer declares.
	// When false, orphans are still reported as no-ops with a reason, never hidden.
	Prune bool
	// Protected applications could not be compiled because a transient
	// dependency failed (such as ExternalSecret provisioning). They must not
	// be removed merely because they are absent from this partial desired state.
	Protected map[string]bool
}

// Build computes the plan that would make actual match desired.
func Build(desired model.DesiredState, actual model.ActualState, rend *renderer.Renderer, opts Options) (model.Plan, error) {
	var plan model.Plan
	seen := map[string]bool{}

	// Networks first: a pod's unit Requires= its network's unit, so a network
	// that is recreated takes its pods down with it, and those must come
	// back. recreated remembers which, for the application loop below.
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
			// systemd only watches the pod's service container, so a workload
			// container that died - or was killed - leaves the unit active and
			// the application broken. Restarting the unit replays the pod.
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

// planNetworks adds the actions for networks and returns the names of the
// ones being created or recreated - the ones whose pods need a restart.
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
			// No unit and no label. podman may still have a network of this
			// name, made by hand; the runtime adopts it rather than replacing it.
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
			// The unit is right but the network is not there, or its oneshot
			// never ran: running it is all that is needed.
			act(model.ActionRestart, "network should exist")
		default:
			act(model.ActionNoOp, "up to date")
		}
	}

	// A managed network Git no longer needs here. An application that could
	// not be compiled this round keeps its network, since nobody can say yet
	// whether it still joins it.
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

// usedByProtected reports whether a protected application's unit on disk
// names this network. Protected applications were not compiled, so their
// desired networks are unknown; what they currently use is on disk.
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

// changedNetworks lists the managed networks of an application that this
// plan recreates, sorted.
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

// rank orders the plan so removals happen before creations, and networks
// are dealt with between the applications that leave them and the ones that
// join them: freeing a host port before something else binds it is the
// difference between a clean rename and a crash loop, and a network cannot
// be removed while a pod is on it, nor joined before it exists.
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

// deadContainers lists the workload containers the runtime shows in a state
// other than running. Init containers are excluded: they exit by design, and
// kube play removes them once they have.
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

// changeDetails explains an update: the unit lines that change, and the
// manifest lines that change with them.
func changeDetails(cur model.ActualApp, unit renderer.Unit) []string {
	details := unitDiff(contentLines(string(cur.UnitContent)), contentLines(string(unit.Content)))
	return append(details, unitDiff(manifestLines(cur.ManifestContent), manifestLines(unit.Manifest))...)
}

// manifestLines prepares a played manifest for diffing.
// Secret documents are replaced by a one-line placeholder: their values are a plan printed to a terminal must never show.
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

// unitDiff reports the lines that would change, so a destructive or surprising edit is visible before it is applied rather than after.
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

// contentLines drops blank lines, podcd's marker comments, and the derived spec-hash label.
// A changed hash is a consequence of the change, not an explanation of it, and it would bury the line a human actually needs to see.
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
