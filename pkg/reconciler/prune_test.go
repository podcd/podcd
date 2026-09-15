package reconciler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/podcd/podcd/pkg/model"
)

func TestPruneRemovesTheGivenNames(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, twoApps)
	if _, err := e.Reconcile(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}

	res, err := e.Prune(context.Background(), []string{"web"}, false)
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
		t.Error("the pruned application is still in the local state")
	}
	if _, ok := st.Applications["api"]; !ok {
		t.Error("api should still be recorded; only web was pruned")
	}
}

func TestPruneAllRemovesEveryManagedApplication(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, twoApps)
	if _, err := e.Reconcile(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}

	res, err := e.Prune(context.Background(), nil, true)
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

func TestPruneNeverTouchesAnUnmanagedUnit(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, twoApps)
	rt.apps["theirs"] = model.ActualApp{Name: "theirs", Managed: false, UnitState: model.UnitActive}

	res, err := e.Prune(context.Background(), []string{"theirs"}, false)
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
	res, err = e.Prune(context.Background(), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range res.Removed {
		if name == "theirs" {
			t.Fatal("--all removed a unit podcd does not manage")
		}
	}
}

func TestPruneReportsNamesThatDoNotExist(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, twoApps)

	res, err := e.Prune(context.Background(), []string{"never-existed"}, false)
	if err != nil {
		t.Fatalf("a name that is not there is not a failure: %v", err)
	}
	if strings.Join(res.NotFound, ",") != "never-existed" {
		t.Fatalf("got %+v", res)
	}
}

func TestPruneContinuesPastAFailureAndReportsIt(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, twoApps)
	if _, err := e.Reconcile(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	rt.removeErr = map[string]error{"api": errors.New("unit is busy")}

	res, err := e.Prune(context.Background(), []string{"api", "web"}, false)
	if err == nil {
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
		t.Error("api failed to prune; it must still be in local state")
	}
	if _, ok := st.Applications["web"]; ok {
		t.Error("web was pruned; it must be forgotten")
	}
}

func TestPruneWithNothingToDoIsANoOp(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, twoApps)

	res, err := e.Prune(context.Background(), nil, false)
	if err != nil || !res.Empty() {
		t.Fatalf("no names and all=false should do nothing: %+v, %v", res, err)
	}
}

func TestPruneHoldsTheReconcileLock(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, twoApps)

	lock, err := Acquire(e.cfg.LockPath())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	if _, err := e.Prune(context.Background(), []string{"api"}, false); err == nil ||
		!strings.Contains(err.Error(), "another reconcile") {
		t.Fatalf("prune must not run alongside a reconcile, got: %v", err)
	}
}

func TestCandidatesAllListsOnlyManagedNamesSorted(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, twoApps)
	if _, err := e.Reconcile(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	rt.apps["theirs"] = model.ActualApp{Name: "theirs", Managed: false}

	got, _, err := e.Candidates(context.Background(), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "api,web" {
		t.Fatalf("got %v", got)
	}
}
