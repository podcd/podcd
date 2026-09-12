package reconciler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/podcd/podcd/pkg/model"
)

// These tests pin the continuous-delivery contracts

const secretApp = `apiVersion: gitops.podcd.io/v1
kind: Application
metadata:
  name: api
spec:
  image: example.com/api@sha256:aaaa
  secretEnv:
    API_TOKEN: env:TEST_DELIVERY_TOKEN
---
apiVersion: gitops.podcd.io/v1
kind: Host
metadata:
  name: vm-1
spec:
  applications: [api]
`

func reconcile(t *testing.T, e *Engine) Result {
	t.Helper()
	res, err := e.Reconcile(context.Background(), Options{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func TestNewCommitRollsOutOnlyWhatChanged(t *testing.T) {
	rt := newFakeRuntime()
	e, repoDir := newTestEngine(t, rt, twoApps)
	reconcile(t, e)
	before, _ := e.store.Load()
	rt.applied = nil

	writeRepo(t, repoDir, strings.Replace(twoApps, "example.com/api@sha256:aaaa", "example.com/api@sha256:cccc", 1))
	res := reconcile(t, e)

	if strings.Join(rt.applied, ",") != "api" {
		t.Fatalf("applied %v, want only api", rt.applied)
	}
	if len(res.Applied) != 1 || res.Applied[0].Type != model.ActionUpdate || res.Applied[0].App != "api" {
		t.Fatalf("want one update of api, got %+v", res.Applied)
	}
	if !strings.Contains(res.Applied[0].Reason, "Git changed") {
		t.Errorf("the reason should say Git changed: %q", res.Applied[0].Reason)
	}

	after, _ := e.store.Load()
	if after.LastSuccess.Revisions["infra"] == before.LastSuccess.Revisions["infra"] {
		t.Error("the new commit was not recorded as the deployed revision")
	}
	rec := after.Applications["api"]
	if rec.Image != "example.com/api@sha256:cccc" || rec.PreviousImage != "example.com/api@sha256:aaaa" {
		t.Errorf("the record should show the rollout and what it replaced: %+v", rec)
	}
	if want := "infra=" + model.ShortRev(after.LastSuccess.Revisions["infra"]); rec.Revision != want || rec.PreviousRevision == rec.Revision {
		t.Errorf("the application should be tagged with the commit it came from (%s): %+v", want, rec)
	}
	if web := after.Applications["web"]; web.AppliedAt != before.Applications["web"].AppliedAt {
		t.Error("an untouched application must keep its record")
	}
}

func TestHandEditedUnitIsPutBack(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, twoApps)
	reconcile(t, e)
	rt.applied = nil

	// Someone edits the unit on the host. The bytes no longer match what Git
	// renders to, so it is rewritten - the spec hash is unchanged, and the
	// reason says so.
	cur := rt.apps["api"]
	cur.UnitContent = append(cur.UnitContent, []byte("Environment=DEBUG=1\n")...)
	cur.UnitFileHash = model.HashBytes(cur.UnitContent)
	rt.apps["api"] = cur

	res := reconcile(t, e)
	if strings.Join(rt.applied, ",") != "api" {
		t.Fatalf("applied %v, want api rewritten", rt.applied)
	}
	if !strings.Contains(res.Applied[0].Reason, "differs") {
		t.Errorf("reason should explain the drift: %q", res.Applied[0].Reason)
	}
	if !strings.Contains(strings.Join(res.Applied[0].Details, "\n"), "- Environment=DEBUG=1") {
		t.Errorf("the plan should show the line that goes away: %v", res.Applied[0].Details)
	}
	if len(reconcile(t, e).Applied) != 0 {
		t.Fatal("after putting the unit back, the next run must be a no-op")
	}
}

func TestStoppedUnitIsRestartedNotRewritten(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, twoApps)
	reconcile(t, e)
	rt.applied = nil

	cur := rt.apps["web"]
	cur.UnitState = model.UnitFailed
	rt.apps["web"] = cur

	res := reconcile(t, e)
	if len(rt.applied) != 0 || strings.Join(rt.restarts, ",") != "web" {
		t.Fatalf("want a restart of web and no rewrite, got applied=%v restarts=%v", rt.applied, rt.restarts)
	}
	if res.Applied[0].Type != model.ActionRestart {
		t.Fatalf("want a restart action, got %+v", res.Applied)
	}
	if rt.apps["web"].UnitState != model.UnitActive {
		t.Error("the unit should be running again")
	}
}

func TestRotatedSecretRestartsTheApplication(t *testing.T) {
	t.Setenv("TEST_DELIVERY_TOKEN", "v1")
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, secretApp)
	reconcile(t, e)
	if len(reconcile(t, e).Applied) != 0 {
		t.Fatal("an unchanged secret must not cause work")
	}
	rt.applied = nil

	t.Setenv("TEST_DELIVERY_TOKEN", "v2")
	res := reconcile(t, e)
	if strings.Join(rt.applied, ",") != "api" {
		t.Fatalf("a rotated secret should re-apply the application, got %v", rt.applied)
	}
	if res.Applied[0].Type != model.ActionUpdate {
		t.Fatalf("want an update, got %+v", res.Applied)
	}
	// The value itself never shows up in what is reported or stored.
	for _, a := range res.Applied {
		if strings.Contains(a.Reason+strings.Join(a.Details, ""), "v2") {
			t.Errorf("the secret value leaked into the plan: %+v", a)
		}
	}
	data, _ := os.ReadFile(e.store.Path())
	if strings.Contains(string(data), "v2") || strings.Contains(string(data), "v1") {
		t.Error("the secret value leaked into the state file")
	}
}

func TestOfflineRemoteKeepsDeliveringTheLastCommit(t *testing.T) {
	rt := newFakeRuntime()
	e, repoDir := newTestEngine(t, rt, twoApps)
	reconcile(t, e)

	// The remote goes away. The checkout is still there, so the agent keeps
	// converging to the last commit it saw and says so.
	gone := repoDir + ".gone"
	if err := os.Rename(repoDir, gone); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(gone, repoDir)

	cur := rt.apps["api"]
	cur.UnitState = model.UnitInactive
	rt.apps["api"] = cur

	res := reconcile(t, e)
	if strings.Join(res.Offline, ",") != "infra" {
		t.Fatalf("the offline repository should be reported, got %v", res.Offline)
	}
	if strings.Join(rt.restarts, ",") != "api" {
		t.Fatalf("drift must still be fixed while offline, got restarts=%v", rt.restarts)
	}
	st, _ := e.store.Load()
	if st.LastFailure != nil || st.FailureCount != 0 {
		t.Errorf("an offline remote is not a failed reconcile: %+v", st.LastAttempt)
	}
}

func TestOfflineRemoteWithNoCheckoutIsAFailure(t *testing.T) {
	rt := newFakeRuntime()
	e, repoDir := newTestEngine(t, rt, twoApps)
	if err := os.RemoveAll(repoDir); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Reconcile(context.Background(), Options{}); err == nil {
		t.Fatal("nothing has ever been fetched: there is nothing to fall back to")
	}
	if len(rt.applied) != 0 {
		t.Fatal("nothing may be applied without a commit")
	}
}

func TestFailureIsCountedAndRecoveryResetsIt(t *testing.T) {
	rt := newFakeRuntime()
	e, repoDir := newTestEngine(t, rt, twoApps)
	reconcile(t, e)

	writeRepo(t, repoDir, strings.Replace(twoApps, "image:", "imagee:", 1))
	for i := 1; i <= 2; i++ {
		if _, err := e.Reconcile(context.Background(), Options{}); err == nil {
			t.Fatal("a broken commit must fail")
		}
		st, _ := e.store.Load()
		if st.FailureCount != i || st.LastFailure == nil || !strings.Contains(st.LastFailure.Error, "imagee") {
			t.Fatalf("run %d: failures should be counted and explained: %+v", i, st)
		}
		if st.LastSuccess == nil {
			t.Fatal("the last success must be remembered through failures")
		}
	}

	writeRepo(t, repoDir, twoApps)
	reconcile(t, e)
	st, _ := e.store.Load()
	if st.FailureCount != 0 || st.LastAttempt.Error != "" || st.LastSuccess.At != st.LastAttempt.At {
		t.Fatalf("a success should reset the failure count: %+v", st)
	}
	if st.LastFailure == nil {
		t.Error("the last failure stays visible for a human to look at")
	}
}

func TestPlanNeverChangesTheHostOrTheRecord(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, twoApps)

	res, err := e.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Plan.Changes()) != 2 {
		t.Fatalf("want two creates planned, got %+v", res.Plan.Actions)
	}
	if len(rt.applied) != 0 {
		t.Fatal("plan applied something")
	}
	st, _ := e.store.Load()
	if st.LastAttempt != nil {
		t.Fatal("plan must not be recorded as an attempt")
	}
}

func TestRunLoopReconcilesAndStopsWhenAsked(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, twoApps)
	e.cfg.Interval = 10 * time.Millisecond
	e.cfg.RetryInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()

	deadline := time.After(5 * time.Second)
	for len(rt.applied) < 2 {
		select {
		case <-deadline:
			t.Fatalf("the loop did not converge the host: applied=%v", rt.applied)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a cancelled loop is a clean stop, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not stop after cancellation")
	}
}

func TestRetryBackoffIsBoundedAndJitterStaysInRange(t *testing.T) {
	b := 5 * time.Second
	for _, want := range []time.Duration{10 * time.Second, 20 * time.Second, 30 * time.Second, 30 * time.Second} {
		if b = nextBackoff(b, 30*time.Second); b != want {
			t.Fatalf("backoff = %s, want %s", b, want)
		}
	}
	for i := 0; i < 100; i++ {
		d := withJitter(time.Minute, 10*time.Second)
		if d < time.Minute || d >= 70*time.Second {
			t.Fatalf("jitter out of range: %s", d)
		}
	}
	if withJitter(time.Minute, 0) != time.Minute {
		t.Error("no jitter means the plain interval")
	}
}

func TestIndexCanBeLimitedToNamedRepositories(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, twoApps)
	// A second repository that cannot be fetched: asking for the first by
	// name must not touch it.
	e.source.Repos = append(e.source.Repos, e.source.Repos[0])
	broken := *e.source.Repos[0]
	broken.Name, broken.URL, broken.Dir = "broken", filepath.Join(t.TempDir(), "missing"), filepath.Join(t.TempDir(), "broken")
	e.source.Repos[1] = &broken

	if _, _, _, err := e.Index(context.Background()); err == nil {
		t.Fatal("fetching every repository must fail on the broken one")
	}
	ix, revs, offline, err := e.Index(context.Background(), "infra")
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.Applications) != 2 || len(revs) != 1 || len(offline) != 0 {
		t.Fatalf("only infra should have been loaded: apps=%d revs=%v offline=%v", len(ix.Applications), revs, offline)
	}
}
