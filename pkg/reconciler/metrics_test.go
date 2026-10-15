package reconciler

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/podcd/podcd/pkg/config"
	"github.com/podcd/podcd/pkg/state"
)

func TestMetricsRenderTheState(t *testing.T) {
	store := state.NewFileStore(filepath.Join(t.TempDir(), "state.json"))
	e := &Engine{store: store}
	e.ident.Host = "vm-1"
	e.cfg.Repository = config.RepositorySpec{Name: "infra", Revision: "main"}
	at := time.Unix(1700000000, 0)
	if err := store.Save(state.State{
		LastAttempt:  &state.Attempt{At: at.Add(time.Minute), Revisions: map[string]string{"infra": "a", "apps": "b"}, Offline: []string{"apps"}},
		LastSuccess:  &state.Attempt{At: at, Revisions: map[string]string{"infra": "z"}},
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
# HELP podcd_repository_info Always 1: the revision agent.yaml targets and the commits of the last reconcile and the last successful one.
# TYPE podcd_repository_info gauge
podcd_repository_info{repository="infra",target_revision="main",last_attempt_revision="a",last_success_revision="z"} 1
# HELP podcd_repository_behind 1 if the host does not run the commit the last reconcile tried to deploy.
# TYPE podcd_repository_behind gauge
podcd_repository_behind{repository="infra"} 1
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
		`podcd_repository_behind{repository="infra"} 0`,
		`podcd_application_healthy{application="web"} 1`,
		"podcd_consecutive_failures 0",
	} {
		if !strings.Contains(b.String(), line+"\n") {
			t.Errorf("missing %q in:\n%s", line, b.String())
		}
	}
}

// A load that fails after the remote was found unreachable must still record it.
func TestMetricsSeeAnOfflineRemoteWhenTheLoadFails(t *testing.T) {
	rt := newFakeRuntime()
	e, repoDir := newTestEngine(t, rt, twoApps)
	reconcile(t, e)
	if err := os.Rename(repoDir, repoDir+".gone"); err != nil {
		t.Fatal(err)
	}
	e.source.ValuesFiles["infra"] = []string{"missing.yaml"}
	if _, err := e.Reconcile(context.Background(), Options{}); err == nil {
		t.Fatal("a missing values file must fail the reconcile")
	}

	var b strings.Builder
	if err := e.WriteMetrics(&b, "dev"); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		`podcd_repository_offline{repository="infra"} 1`,
		`podcd_repository_behind{repository="infra"} 0`,
		"podcd_consecutive_failures 1",
	} {
		if !strings.Contains(b.String(), line+"\n") {
			t.Errorf("missing %q in:\n%s", line, b.String())
		}
	}
}

// state.json is metadata a user may delete or break; metrics must still render.
func TestMetricsSurviveALostState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	e := &Engine{store: state.NewFileStore(path), log: slog.New(slog.DiscardHandler)}
	for _, content := range []string{"", "{not json"} {
		if content != "" {
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		var b strings.Builder
		if err := e.WriteMetrics(&b, "dev"); err != nil {
			t.Fatalf("state %q: %v", content, err)
		}
		if !strings.Contains(b.String(), "podcd_consecutive_failures 0\n") {
			t.Errorf("state %q:\n%s", content, b.String())
		}
	}
}
