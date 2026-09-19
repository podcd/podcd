package reconciler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/podcd/podcd/pkg/model"
)

func TestRemoveRemovesTheGivenNames(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, twoApps)
	if _, err := e.Reconcile(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}

	res, err := e.Remove(context.Background(), []string{"web"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Removed, ",") != "web" || !res.OK() || res.Empty() {
		t.Fatalf("got %+v", res)
	}
	if strings.Join(rt.removed, ",") != "web" {
		t.Fatalf("runtime.Remove was not called for web: %v", rt.removed)
	}
	if strings.Join(rt.applied, ",") != "api,web" {
		t.Fatalf("api should not have been touched: applied=%v", rt.applied)
	}

	st, _ := e.store.Load()
	if _, ok := st.Applications["web"]; ok {
		t.Error("the removed application is still in the local state")
	}
	if _, ok := st.Applications["api"]; !ok {
		t.Error("api should still be recorded; only web was removed")
	}
}

func TestRemoveAllRemovesEveryManagedApplication(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, twoApps)
	if _, err := e.Reconcile(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}

	res, err := e.Remove(context.Background(), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Removed, ",") != "api,web" {
		t.Fatalf("want both applications removed, got %+v", res.Removed)
	}
	actual, _ := rt.Inspect(context.Background())
	if len(actual.Apps) != 0 {
		t.Fatalf("the host should be empty, got %+v", actual.Apps)
	}
}

func TestRemoveNeverTouchesAnUnmanagedUnit(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, twoApps)
	rt.apps["theirs"] = model.ActualApp{Name: "theirs", Managed: false, UnitState: model.UnitActive}

	res, err := e.Remove(context.Background(), []string{"theirs"}, false)
	if err != nil {
		t.Fatalf("skipping an unmanaged unit is not a failure: %v", err)
	}
	if strings.Join(res.Skipped, ",") != "theirs" || len(res.Removed) != 0 {
		t.Fatalf("got %+v", res)
	}
	if len(rt.removed) != 0 {
		t.Fatalf("runtime.Remove must never be called for an unmanaged unit, got %v", rt.removed)
	}

	// --all must not sweep it up either.
	res, err = e.Remove(context.Background(), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range res.Removed {
		if name == "theirs" {
			t.Fatal("--all removed a unit podcd does not manage")
		}
	}
}

func TestRemoveReportsNamesThatDoNotExist(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, twoApps)

	res, err := e.Remove(context.Background(), []string{"never-existed"}, false)
	if err != nil {
		t.Fatalf("a name that is not there is not a failure: %v", err)
	}
	if strings.Join(res.NotFound, ",") != "never-existed" {
		t.Fatalf("got %+v", res)
	}
}

func TestRemoveContinuesPastAFailureAndReportsIt(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, twoApps)
	if _, err := e.Reconcile(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	rt.removeErr = map[string]error{"api": errors.New("unit is busy")}

	res, err := e.Remove(context.Background(), []string{"api", "web"}, false)
	if err != nil {
		t.Fatalf("a per-application failure is reported on the result, not as a call error: %v", err)
	}
	if res.Err() == nil {
		t.Fatal("a failed removal must be reported as an error")
	}
	if res.OK() {
		t.Fatal("OK() must be false when something failed")
	}
	if res.Failed["api"] == nil || !strings.Contains(res.Failed["api"].Error(), "unit is busy") {
		t.Fatalf("api's failure was not recorded: %+v", res.Failed)
	}
	if strings.Join(res.Removed, ",") != "web" {
		t.Fatalf("web should still have been removed despite api failing: %+v", res.Removed)
	}

	st, _ := e.store.Load()
	if _, ok := st.Applications["api"]; !ok {
		t.Error("api failed to remove; it must still be in local state")
	}
	if _, ok := st.Applications["web"]; ok {
		t.Error("web was removed; it must be forgotten")
	}
}

func TestRemoveWithNothingToDoIsANoOp(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, twoApps)

	res, err := e.Remove(context.Background(), nil, false)
	if err != nil || !res.Empty() {
		t.Fatalf("no names and all=false should do nothing: %+v, %v", res, err)
	}
}

func TestRemoveHoldsTheReconcileLock(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, twoApps)

	lock, err := Acquire(e.cfg.LockPath())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	if _, err := e.Remove(context.Background(), []string{"api"}, false); err == nil ||
		!strings.Contains(err.Error(), "another reconcile") {
		t.Fatalf("remove must not run alongside a reconcile, got: %v", err)
	}
}

func TestRemoveCandidatesAllListsOnlyManagedNamesSorted(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, twoApps)
	if _, err := e.Reconcile(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	rt.apps["theirs"] = model.ActualApp{Name: "theirs", Managed: false}

	got, _, _, err := e.RemoveCandidates(context.Background(), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "api,web" {
		t.Fatalf("got %v", got)
	}
}

// Prune consults Git: it removes what the repository no longer declares for
// this host, and only that.
func TestPruneRemovesOnlyWhatGitNoLongerDeclares(t *testing.T) {
	rt := newFakeRuntime()
	e, repoDir := newTestEngine(t, rt, twoApps)
	if _, err := e.Reconcile(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}

	// Git drops web; api stays.
	writeRepo(t, repoDir, oneApp)

	names, _, _, err := e.PruneCandidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "web" {
		t.Fatalf("only web is gone from Git, got candidates %v", names)
	}

	res, err := e.Prune(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Removed, ",") != "web" || !res.OK() {
		t.Fatalf("got %+v", res)
	}
	if strings.Join(rt.removed, ",") != "web" {
		t.Fatalf("runtime.Remove should have been called for web only: %v", rt.removed)
	}
	actual, _ := rt.Inspect(context.Background())
	if _, ok := actual.Apps["api"]; !ok {
		t.Fatal("api is still declared in Git and must survive a prune")
	}
}

// An application Git still declares is never pruned, whatever state it is in.
func TestPruneLeavesEverythingStillInGitAlone(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, twoApps)
	if _, err := e.Reconcile(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	// Even a stopped one.
	stopped := rt.apps["web"]
	stopped.UnitState = model.UnitInactive
	rt.apps["web"] = stopped

	res, err := e.Prune(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Empty() || len(rt.removed) != 0 {
		t.Fatalf("nothing left Git, so nothing should be pruned: %+v removed=%v", res, rt.removed)
	}
}

// A unit podcd does not manage is invisible to prune, even though Git does
// not declare it either.
func TestPruneNeverTouchesAnUnmanagedUnit(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, twoApps)
	rt.apps["theirs"] = model.ActualApp{Name: "theirs", Managed: false, UnitState: model.UnitActive}

	names, _, _, err := e.PruneCandidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if n == "theirs" {
			t.Fatal("an unmanaged unit must never be a prune candidate")
		}
	}
}

// Prune needs Git. Without it there is no way to know what is still wanted,
// so it must refuse rather than guess - that is what remove is for.
func TestPruneFailsWhenGitIsUnreachable(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, twoApps)
	e.source.Repos[0].URL = "/nonexistent/repo"

	if _, _, _, err := e.PruneCandidates(context.Background()); err == nil {
		t.Fatal("prune must fail when the repository cannot be read")
	}
	if len(rt.removed) != 0 {
		t.Fatalf("nothing may be removed when Git is unknown: %v", rt.removed)
	}
}
