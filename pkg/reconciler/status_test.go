package reconciler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/podcd/podcd/pkg/model"
)

// Health asks Git what this host should run and the runtime how each of those
// is doing. Anything not in Git is not reported: it is remove's business.
func TestHealthReportsEveryDesiredApplication(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, twoApps)
	rt.unhealthy["web"] = true

	got, err := e.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]model.Health{}
	for _, h := range got {
		byName[h.App] = h
	}
	if len(byName) != 2 || byName["api"].Status != model.HealthHealthy || byName["web"].Status != model.HealthUnhealthy {
		t.Fatalf("got %+v", got)
	}
	if byName["web"].Message != "it is broken" {
		t.Fatalf("the runtime's explanation must be passed on: %+v", byName["web"])
	}
}

func TestHealthTurnsARuntimeErrorIntoAnUnknownVerdict(t *testing.T) {
	rt := newFakeRuntime()
	rt.healthErr = errors.New("podman is not answering")
	e, _ := newTestEngine(t, rt, oneApp)

	got, err := e.Health(context.Background())
	if err != nil {
		t.Fatalf("one app failing to answer must not fail the whole report: %v", err)
	}
	if len(got) != 1 || got[0].Status != model.HealthUnknown || !strings.Contains(got[0].Message, "not answering") || got[0].CheckedAt.IsZero() {
		t.Fatalf("got %+v", got)
	}
}

func TestHealthNeedsGit(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, oneApp)
	e.source.Repos[0].URL = "/nonexistent/repo"
	if _, err := e.Health(context.Background()); err == nil {
		t.Fatal("without Git there is no list of applications to check")
	}
}

// Status is the offline view: local state plus what the runtime sees.
func TestStatusWorksWithoutGitAndCarriesTheRuntimeView(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, twoApps)
	if _, err := e.Reconcile(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	e.source.Repos[0].URL = "/nonexistent/repo"

	s, err := e.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Identity.Host != "vm-1" || s.Runtime != "fake" || !s.Available || s.Why != "" {
		t.Fatalf("got %+v", s)
	}
	if s.State.LastSuccess == nil || len(s.State.Applications) != 2 {
		t.Fatalf("the local record of the last reconcile should be included: %+v", s.State)
	}
	if len(s.Actual.Apps) != 2 || !s.Actual.Apps["api"].Managed {
		t.Fatalf("the runtime's view should be included: %+v", s.Actual)
	}
	if s.Repo.Name != "infra" {
		t.Fatalf("the configured repository should be listed: %+v", s.Repo)
	}
}

func TestStatusSurvivesAMissingStateFile(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, oneApp)
	s, err := e.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.State.LastAttempt != nil || len(s.Actual.Apps) != 0 {
		t.Fatalf("a fresh host has nothing to report yet: %+v", s)
	}
}

func TestLogsGoStraightToTheRuntime(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, oneApp)
	out, err := e.Logs(context.Background(), "api", 20)
	if err != nil || out != "api:20" {
		t.Fatalf("got %q %v", out, err)
	}
}
