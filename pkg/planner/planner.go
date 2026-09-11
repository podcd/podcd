// Package planner turns (desired, actual) into an explicit list of changes.
//
// Nothing here touches the system. That separation is what makes `podcd plan`
// trustworthy: the plan you read is exactly the plan that would be applied.
package planner

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/podcd/podcd/pkg/model"
	"github.com/podcd/podcd/pkg/renderer"
)

// Options tunes planning decisions that are policy rather than fact.
type Options struct {
	// Prune removes managed applications Git no longer declares. When false,
	// orphans are still reported - as no-ops with a reason - never hidden.
	Prune bool
}

// Build computes the plan that would make actual match desired.
func Build(desired model.DesiredState, actual model.ActualState, rend *renderer.Renderer, opts Options) (model.Plan, error) {
	var plan model.Plan
	seen := map[string]bool{}

	apps := append([]model.Application(nil), desired.Applications...)
	sort.Slice(apps, func(i, j int) bool { return apps[i].Name < apps[j].Name })

	for i := range apps {
		app := apps[i]
		seen[app.Name] = true

		unit, err := rend.Render(app)
		if err != nil {
			return model.Plan{}, fmt.Errorf("rendering %s: %w", app.Name, err)
		}

		cur, exists := actual.Apps[app.Name]
		switch {
		case !exists:
			plan.Actions = append(plan.Actions, model.Action{
				Type:        model.ActionCreate,
				App:         app.Name,
				Reason:      "not present on this host",
				Details:     imageDetails(app),
				Application: &apps[i],
			})

		case !cur.Managed:
			// Somebody else owns this unit. Guessing would be how you delete
			// someone's database.
			return model.Plan{}, fmt.Errorf("application %q: unit %s exists but is not managed by podcd; "+
				"remove it by hand or restore its podcd header before reconciling", app.Name, cur.UnitFile)

		case cur.UnitFileHash != model.HashBytes(unit.Content):
			plan.Actions = append(plan.Actions, model.Action{
				Type:        model.ActionUpdate,
				App:         app.Name,
				Reason:      updateReason(cur, unit),
				Details:     changeDetails(cur, unit),
				Application: &apps[i],
			})

		case cur.SecretsHash != unit.SecretsHash:
			plan.Actions = append(plan.Actions, model.Action{
				Type:        model.ActionUpdate,
				App:         app.Name,
				Reason:      "secret values changed",
				Application: &apps[i],
			})

		case cur.UnitState != model.UnitActive:
			plan.Actions = append(plan.Actions, model.Action{
				Type:        model.ActionRestart,
				App:         app.Name,
				Reason:      fmt.Sprintf("unit is %s, should be running", stateOrUnknown(cur.UnitState)),
				Application: &apps[i],
			})

		default:
			plan.Actions = append(plan.Actions, model.Action{
				Type:        model.ActionNoOp,
				App:         app.Name,
				Reason:      "up to date",
				Application: &apps[i],
			})
		}
	}

	for _, name := range actual.Names() {
		if seen[name] {
			continue
		}
		cur := actual.Apps[name]
		if !cur.Managed {
			continue // not ours; not our business
		}
		if !opts.Prune {
			plan.Actions = append(plan.Actions, model.Action{
				Type:   model.ActionNoOp,
				App:    name,
				Reason: "no longer declared in Git, but pruning is disabled",
			})
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

	sort.SliceStable(plan.Actions, func(i, j int) bool {
		if rank(plan.Actions[i].Type) != rank(plan.Actions[j].Type) {
			return rank(plan.Actions[i].Type) < rank(plan.Actions[j].Type)
		}
		return plan.Actions[i].App < plan.Actions[j].App
	})
	return plan, nil
}

// rank orders the plan so removals happen before creations. Freeing a host port
// before something else tries to bind it is the difference between a clean
// rename and a crash loop.
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

func stateOrUnknown(s model.UnitState) string {
	if s == "" {
		return string(model.UnitUnknown)
	}
	return string(s)
}

func updateReason(cur model.ActualApp, unit renderer.Unit) string {
	if cur.SpecHash != unit.SpecHash {
		return "configuration in Git changed"
	}
	return "unit file differs from the rendered unit (edited by hand, or written by an older podcd)"
}

// changeDetails explains an update: the unit lines that change and, for a kube
// workload, the manifest lines that change.
func changeDetails(cur model.ActualApp, unit renderer.Unit) []string {
	details := unitDiff(contentLines(string(cur.UnitContent)), contentLines(string(unit.Content)))
	if unit.IsKube() {
		details = append(details, unitDiff(manifestLines(cur.ManifestContent), manifestLines(unit.Manifest))...)
	}
	return details
}

// manifestLines prepares a played manifest for diffing. Secret documents are
// replaced by a one-line placeholder: their values are exactly what a plan
// printed to a terminal must never show.
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

// unitDiff reports the lines that would change, so a destructive or surprising
// edit is visible before it is applied rather than after.
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

// contentLines drops blank lines, podcd's marker comments and the derived
// spec-hash label: a changed hash is a consequence of the change, not an
// explanation of it, and it would bury the line a human actually needs to see.
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
