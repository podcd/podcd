package cli

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	sigyaml "sigs.k8s.io/yaml"

	"github.com/podcd/podcd/pkg/model"
	"github.com/podcd/podcd/pkg/reconciler"
	"github.com/podcd/podcd/pkg/state"
)

func printStatus(env *environment, st reconciler.Status) {
	w := table(env.out)
	fmt.Fprintf(w, "host\t%s\t(%s)\n", st.Identity.Host, st.Identity.Source)
	runtimeNote := "available"
	if !st.Available {
		runtimeNote = "UNAVAILABLE: " + st.Why
	}
	fmt.Fprintf(w, "runtime\t%s\t(%s)\n", st.Runtime, runtimeNote)
	fmt.Fprintf(w, "config\t%s\t\n", st.Config)
	fmt.Fprintf(w, "state\t%s\t\n", env.engine.Store().Path())
	w.Flush()

	fmt.Fprintln(env.out, "\nrepository")
	w = table(env.out)
	r := st.Repo
	rev := ""
	if st.State.LastSuccess != nil {
		rev = model.ShortRev(st.State.LastSuccess.Revisions[r.Name])
	}
	fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", r.Name, r.URL, r.Revision, rev)
	w.Flush()

	fmt.Fprintln(env.out, "\nlast reconcile")
	w = table(env.out)
	fmt.Fprintf(w, "  success\t%s\t%s\n", attemptTime(st.State.LastSuccess), attemptDetail(st.State.LastSuccess))
	fmt.Fprintf(w, "  failure\t%s\t%s\n", attemptTime(st.State.LastFailure), attemptDetail(st.State.LastFailure))
	if st.State.FailureCount > 0 {
		fmt.Fprintf(w, "  failing\t%d consecutive failure(s)\t\n", st.State.FailureCount)
	}
	w.Flush()

	fmt.Fprintln(env.out, "\napplications")
	if len(st.Actual.Apps) == 0 {
		fmt.Fprintln(env.out, "  (none)")
	} else {
		w = table(env.out)
		fmt.Fprintln(w, "  APP\tUNIT\tCONTAINERS\tRESTARTS\tIMAGE\tLAST HEALTH\tAPPLIED")
		for _, name := range st.Actual.Names() {
			a := st.Actual.Apps[name]
			rec := st.State.Applications[name]
			owner := ""
			if !a.Managed {
				owner = " (not managed by podcd)"
			}
			fmt.Fprintf(w, "  %s\t%s%s\t%s\t%s\t%s\t%s\t%s\n",
				name, string(a.UnitState), owner, containerSummary(a),
				restartSummary(a),
				dash(shortImage(cmp.Or(rec.Image, a.ContainerImage))),
				dash(rec.Health), dash(rec.AppliedAt))
		}
		w.Flush()
	}

	if len(st.Actual.Networks) == 0 {
		return
	}
	fmt.Fprintln(env.out, "\nnetworks")
	w = table(env.out)
	fmt.Fprintln(w, "  NETWORK\tUNIT\tPODMAN")
	for _, name := range st.Actual.NetworkNames() {
		n := st.Actual.Networks[name]
		owner := ""
		if !n.Managed {
			owner = " (not managed by podcd)"
		}
		exists := "missing"
		if n.Exists {
			exists = "exists"
		}
		fmt.Fprintf(w, "  %s\t%s%s\t%s\n", name, string(n.UnitState), owner, exists)
	}
	w.Flush()
}

func printPlan(w io.Writer, res reconciler.Result) {
	fmt.Fprintf(w, "plan for %s at %s\n", res.Desired.Host, res.Desired.RevisionString())
	for _, r := range res.Offline {
		fmt.Fprintf(w, "  warning: repository %q could not be refreshed; using the commit already on disk\n", r)
	}
	changes := res.Plan.Changes()
	if len(changes) == 0 {
		fmt.Fprintf(w, "  no changes (%d application(s) and %d network(s) already match Git)\n", len(res.Desired.Applications), len(res.Desired.Networks))
		return
	}
	for _, a := range changes {
		destructive := ""
		if a.Destructive {
			destructive = "  [destructive]"
		}
		fmt.Fprintf(w, "  %s %s - %s%s\n", actionSymbol(a.Type), a.Subject(), a.Reason, destructive)
		for _, d := range a.Details {
			fmt.Fprintf(w, "      %s\n", d)
		}
	}
	fmt.Fprintf(w, "%d change(s)\n", len(changes))
}

func printHealth(w io.Writer, results []model.Health) {
	if len(results) == 0 {
		return
	}
	sorted := slices.Clone(results)
	slices.SortFunc(sorted, func(a, b model.Health) int { return cmp.Compare(a.App, b.App) })
	fmt.Fprintln(w, "health")
	tw := table(w)
	for _, h := range sorted {
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", h.App, h.Status, h.Message)
	}
	tw.Flush()
}

// table is the aligned two-space-gap writer every listing uses.
func table(w io.Writer) *tabwriter.Writer { return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0) }

func actionSymbol(t model.ActionType) string {
	switch t {
	case model.ActionCreate:
		return "+ create"
	case model.ActionUpdate:
		return "~ update"
	case model.ActionDelete:
		return "- delete"
	case model.ActionRestart:
		return "> restart"
	default:
		return "= noop"
	}
}

type outputFormat string

func (o *outputFormat) String() string { return string(*o) }
func (o *outputFormat) Type() string   { return "format" }

func (o *outputFormat) Set(v string) error {
	switch v {
	case "json", "yaml":
		*o = outputFormat(v)
		return nil
	}
	return fmt.Errorf("must be json or yaml")
}

func (o *outputFormat) addFlag(cmd *cobra.Command) {
	cmd.Flags().VarP(o, "output", "o", "output format: json or yaml")
}

// write encodes v in the chosen format. The yaml goes through JSON first so
// the json tags - and the secret redaction behind them - apply to both.
func (o outputFormat) write(w io.Writer, v any) error {
	switch o {
	case "yaml":
		j, err := json.Marshal(v)
		if err != nil {
			return err
		}
		y, err := sigyaml.JSONToYAML(j)
		if err != nil {
			return err
		}
		_, err = w.Write(y)
		return err
	default:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
}

func attemptTime(a *state.Attempt) string {
	if a == nil || a.At.IsZero() {
		return "never"
	}
	return fmt.Sprintf("%s (%s ago)", a.At.Format(time.RFC3339), time.Since(a.At).Round(time.Second))
}

func attemptDetail(a *state.Attempt) string {
	switch {
	case a == nil:
		return ""
	case a.Error != "":
		return firstLine(a.Error)
	case len(a.Actions) > 0:
		return strings.Join(a.Actions, ", ")
	default:
		return "no changes"
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " ..."
	}
	return s
}

func shortImage(s string) string {
	if i := strings.Index(s, "@sha256:"); i > 0 && len(s) > i+20 {
		return s[:i+20] + "..."
	}
	return s
}

func portList(ports []model.Port) string {
	if len(ports) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		parts = append(parts, fmt.Sprintf("%d→%d", p.Host, p.Container))
	}
	return strings.Join(parts, ",")
}

func countUnhealthy(results []model.Health) int {
	n := 0
	for _, h := range results {
		if !h.OK() {
			n++
		}
	}
	return n
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// containerSummary is "running/total", with any healthcheck verdict that is
// not plain healthy called out: "2/2", "1/2", "2/2 (starting)".
func containerSummary(a model.ActualApp) string {
	if len(a.Containers) == 0 {
		return dash(a.ContainerState)
	}
	running := 0
	var flagged []string
	for _, c := range a.Containers {
		if c.State == "running" {
			running++
		}
		if c.Health == "unhealthy" || c.Health == "starting" {
			flagged = append(flagged, c.Health)
		}
	}
	out := fmt.Sprintf("%d/%d", running, len(a.Containers))
	if len(flagged) > 0 {
		out += " (" + strings.Join(slices.Compact(slices.Sorted(slices.Values(flagged))), ", ") + ")"
	}
	return out
}

// restartSummary is the total restart count across the workload's containers.
func restartSummary(a model.ActualApp) string {
	if len(a.Containers) == 0 {
		return "-"
	}
	n := 0
	for _, c := range a.Containers {
		n += c.Restarts
	}
	return strconv.Itoa(n)
}
