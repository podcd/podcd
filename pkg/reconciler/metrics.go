package reconciler

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/podcd/podcd/pkg/model"
	"github.com/podcd/podcd/pkg/state"
)

// WriteMetrics renders state.json in the Prometheus text format. It reads the
// file on every call, so values survive a restart and include a manual
// `podcd reconcile`. A missing or unreadable file renders as a fresh state,
// as it does for the reconcile that will rewrite it.
func (e *Engine) WriteMetrics(w io.Writer, version string) error {
	st, err := e.store.Load()
	if err != nil {
		e.log.Debug("local state could not be read", "error", err)
	}
	var b strings.Builder
	gauge := func(name, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
	}

	gauge("podcd_info", "The agent's version and host, always 1.")
	fmt.Fprintf(&b, "podcd_info{version=%s,host=%s} 1\n", label(version), label(e.ident.Host))

	// 0 for never, so an age alert fires instead of finding no sample.
	gauge("podcd_last_attempt_timestamp_seconds", "When the last reconcile started, whether it succeeded or not; 0 if never.")
	fmt.Fprintf(&b, "podcd_last_attempt_timestamp_seconds %d\n", unix(st.LastAttempt))
	gauge("podcd_last_success_timestamp_seconds", "When the last successful reconcile started; 0 if never.")
	fmt.Fprintf(&b, "podcd_last_success_timestamp_seconds %d\n", unix(st.LastSuccess))
	gauge("podcd_consecutive_failures", "Reconciles failed since the last success.")
	fmt.Fprintf(&b, "podcd_consecutive_failures %d\n", st.FailureCount)

	r := e.cfg.Repository
	var attempted, succeeded string
	if a := st.LastAttempt; a != nil {
		attempted = a.Revisions[r.Name]
	}
	if a := st.LastSuccess; a != nil {
		succeeded = a.Revisions[r.Name]
	}
	gauge("podcd_repository_info", "Always 1: the revision agent.yaml targets and the commits of the last reconcile and the last successful one.")
	fmt.Fprintf(&b, "podcd_repository_info{repository=%s,target_revision=%s,last_attempt_revision=%s,last_success_revision=%s} 1\n",
		label(r.Name), label(r.Revision), label(attempted), label(succeeded))
	gauge("podcd_repository_behind", "1 if the host does not run the commit the last reconcile tried to deploy.")
	fmt.Fprintf(&b, "podcd_repository_behind{repository=%s} %d\n", label(r.Name), bit(attempted != "" && attempted != succeeded))

	gauge("podcd_repository_offline", "1 if the last reconcile used the commit on disk because the remote was unreachable.")
	if a := st.LastAttempt; a != nil {
		for _, repo := range slices.Sorted(maps.Keys(a.Revisions)) {
			fmt.Fprintf(&b, "podcd_repository_offline{repository=%s} %d\n", label(repo), bit(slices.Contains(a.Offline, repo)))
		}
	}

	gauge("podcd_application_healthy", "1 if the application's last health check passed.")
	for _, name := range slices.Sorted(maps.Keys(st.Applications)) {
		if h := st.Applications[name].Health; h != "" {
			fmt.Fprintf(&b, "podcd_application_healthy{application=%s} %d\n", label(name), bit(h == string(model.HealthHealthy)))
		}
	}

	_, err = io.WriteString(w, b.String())
	return err
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// label quotes a Prometheus label value.
func label(s string) string { return `"` + labelEscaper.Replace(s) + `"` }

func unix(a *state.Attempt) int64 {
	if a == nil {
		return 0
	}
	return a.At.Unix()
}

func bit(b bool) int {
	if b {
		return 1
	}
	return 0
}
