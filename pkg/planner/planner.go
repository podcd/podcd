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
}

// Build computes the plan that would make actual match desired.
func Build(desired model.DesiredState, actual model.ActualState, rend *renderer.Renderer, opts Options) (model.Plan, error) {
	var plan model.Plan
	seen := map[string]bool{}

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
		case cur.SecretsHash != unit.SecretsHash:
			act(model.ActionUpdate, "secret values changed")
		case cur.UnitState != model.UnitActive:
			act(model.ActionRestart, fmt.Sprintf("unit is %s, should be running", cmp.Or(cur.UnitState, model.UnitUnknown)))
		default:
			act(model.ActionNoOp, "up to date")
		}
	}

	for _, name := range actual.Names() {
		if seen[name] || !actual.Apps[name].Managed {
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
		return cmp.Or(cmp.Compare(rank(a.Type), rank(b.Type)), cmp.Compare(a.App, b.App))
	})
	return plan, nil
}

// rank orders the plan so removals happen before creations.
// Freeing a host port before something else tries to bind it is the difference between a clean rename and a crash loop.
func rank(t model.ActionType) int {
	switch t {
	case model.ActionDelete:
		return 0
	case model.ActionUpdate:
		return 1
	case model.ActionCreate:
		return 2
	case model.ActionRestart:
		return 3
	default:
		return 4
	}
}

func imageDetails(app model.Application) []string {
	images := app.ImageList()
	out := make([]string, 0, len(images))
	for _, img := range images {
		out = append(out, "image "+img)
	}
	if app.IsKube() {
		out = append([]string{"pod with " + strconv.Itoa(len(images)) + " container(s), played by podman"}, out...)
	}
	return out
}

func updateReason(cur model.ActualApp, unit renderer.Unit) string {
	if cur.SpecHash != unit.SpecHash {
		return "configuration in Git changed"
	}
	return "unit file differs from the rendered unit (edited by hand, or written by an older podcd)"
}

// changeDetails explains an update: the unit lines that change and, for a kube workload, the manifest lines that change.
func changeDetails(cur model.ActualApp, unit renderer.Unit) []string {
	details := unitDiff(contentLines(string(cur.UnitContent)), contentLines(string(unit.Content)))
	if unit.IsKube() {
		details = append(details, unitDiff(manifestLines(cur.ManifestContent), manifestLines(unit.Manifest))...)
	}
	return details
}

// manifestLines prepares a played manifest for diffing.
// Secret documents are replaced by a one-line placeholder: their values are exactly what a plan printed to a terminal must never show.
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
