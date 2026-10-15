package reconciler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/podcd/podcd/pkg/state"
)

func TestMetricsRenderTheState(t *testing.T) {
	store := state.NewFileStore(filepath.Join(t.TempDir(), "state.json"))
	e := &Engine{store: store}
	e.ident.Host = "vm-1"
	at := time.Unix(1700000000, 0)
	if err := store.Save(state.State{
		LastAttempt:  &state.Attempt{At: at.Add(time.Minute), Revisions: map[string]string{"infra": "a", "apps": "b"}, Offline: []string{"apps"}},
		LastSuccess:  &state.Attempt{At: at},
		FailureCount: 2,
		Applications: map[string]state.AppRecord{
			"web":   {Name: "web", Health: "unhealthy"},
			"api":   {Name: "api", Health: "healthy"},
			"fresh": {Name: "fresh"}, // never probed: no sample
		},
	}); err != nil {
		t.Fatal(err)
	}

	var b strings.Builder
	if err := e.WriteMetrics(&b, `v1.2.3"x`); err != nil {
		t.Fatal(err)
	}
	want := `# HELP podcd_info The agent's version and host, always 1.
# TYPE podcd_info gauge
podcd_info{version="v1.2.3\"x",host="vm-1"} 1
# HELP podcd_last_attempt_timestamp_seconds When the last reconcile started, whether it succeeded or not.
# TYPE podcd_last_attempt_timestamp_seconds gauge
podcd_last_attempt_timestamp_seconds 1700000060
# HELP podcd_last_success_timestamp_seconds When the last successful reconcile started.
# TYPE podcd_last_success_timestamp_seconds gauge
podcd_last_success_timestamp_seconds 1700000000
# HELP podcd_consecutive_failures Reconciles failed since the last success.
# TYPE podcd_consecutive_failures gauge
podcd_consecutive_failures 2
# HELP podcd_repository_offline 1 if the last reconcile used the commit on disk because the remote was unreachable.
# TYPE podcd_repository_offline gauge
podcd_repository_offline{repository="apps"} 1
podcd_repository_offline{repository="infra"} 0
# HELP podcd_application_healthy 1 if the application's last health check passed.
# TYPE podcd_application_healthy gauge
podcd_application_healthy{application="api"} 1
podcd_application_healthy{application="web"} 0
`
	if got := b.String(); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// The offline list must reach state.json for the metric to see it.
func TestMetricsSeeAnOfflineRemote(t *testing.T) {
	rt := newFakeRuntime()
	e, repoDir := newTestEngine(t, rt, twoApps)
	reconcile(t, e)
	if err := os.Rename(repoDir, repoDir+".gone"); err != nil {
		t.Fatal(err)
	}
	reconcile(t, e)

	var b strings.Builder
	if err := e.WriteMetrics(&b, "dev"); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		`podcd_repository_offline{repository="infra"} 1`,
		`podcd_application_healthy{application="web"} 1`,
		"podcd_consecutive_failures 0",
	} {
		if !strings.Contains(b.String(), line+"\n") {
			t.Errorf("missing %q in:\n%s", line, b.String())
		}
	}
}
