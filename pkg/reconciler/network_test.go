package reconciler

import (
	"context"
	"strings"
	"testing"
)

const twoAppsOnANetwork = `apiVersion: gitops.podcd.io/v1
kind: Network
metadata: {name: backend}
spec:
  subnet: 10.90.0.0/24
---
apiVersion: v1
kind: Pod
metadata:
  name: api
  annotations: {io.podcd.networks: backend}
spec:
  containers:
    - name: api
      image: example.com/api@sha256:aaaa
---
apiVersion: v1
kind: Pod
metadata:
  name: web
  annotations: {io.podcd.networks: backend}
spec:
  containers:
    - name: web
      image: example.com/web@sha256:bbbb
---
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: vm-1}
spec:
  applications: [api, web]
`

// The same, with the subnet changed: a network Quadlet has to recreate.
const twoAppsOnAChangedNetwork = `apiVersion: gitops.podcd.io/v1
kind: Network
metadata: {name: backend}
spec:
  subnet: 10.91.0.0/24
---
apiVersion: v1
kind: Pod
metadata:
  name: api
  annotations: {io.podcd.networks: backend}
spec:
  containers:
    - name: api
      image: example.com/api@sha256:aaaa
---
apiVersion: v1
kind: Pod
metadata:
  name: web
  annotations: {io.podcd.networks: backend}
spec:
  containers:
    - name: web
      image: example.com/web@sha256:bbbb
---
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: vm-1}
spec:
  applications: [api, web]
`

func applied(res Result) string {
	var out []string
	for _, a := range res.Applied {
		out = append(out, string(a.Type)+" "+a.Subject())
	}
	return strings.Join(out, ", ")
}

func TestReconcileCreatesTheNetworkBeforeItsPodsAndRecordsBoth(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, twoAppsOnANetwork)
	res, err := e.Reconcile(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := applied(res); got != "create network backend, create api, create web" {
		t.Fatalf("got %q", got)
	}
	if len(rt.networksApplied) != 1 || rt.networksApplied[0] != "backend" {
		t.Fatalf("network not applied: %v", rt.networksApplied)
	}
	st, _ := e.store.Load()
	if got := strings.Join(st.LastSuccess.Actions, ", "); got != "create network backend, create api, create web" {
		t.Fatalf("state records %q", got)
	}

	// Nothing changed: nothing to do, network included.
	again, err := e.Reconcile(context.Background(), Options{})
	if err != nil || len(again.Applied) != 0 || !again.Plan.Empty() {
		t.Fatalf("second reconcile is not idempotent: %v %+v", err, again.Plan.Changes())
	}
}

func TestReconcileRecreatesAChangedNetworkAndRestartsItsPods(t *testing.T) {
	rt := newFakeRuntime()
	e, repoDir := newTestEngine(t, rt, twoAppsOnANetwork)
	if _, err := e.Reconcile(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	writeRepo(t, repoDir, twoAppsOnAChangedNetwork)
	res, err := e.Reconcile(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := applied(res); got != "update network backend, restart api, restart web" {
		t.Fatalf("got %q", got)
	}
	if strings.Join(rt.restarts, ",") != "api,web" {
		t.Fatalf("restarts: %v", rt.restarts)
	}
}

func TestReconcileRemovesANetworkNobodyJoinsAnyMoreAfterItsPods(t *testing.T) {
	rt := newFakeRuntime()
	e, repoDir := newTestEngine(t, rt, twoAppsOnANetwork)
	if _, err := e.Reconcile(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	writeRepo(t, repoDir, oneApp) // api stays, off any network; web goes
	res, err := e.Reconcile(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := applied(res); got != "delete web, delete network backend, update api" {
		t.Fatalf("got %q", got)
	}
	if strings.Join(rt.networksRemoved, ",") != "backend" {
		t.Fatalf("network not removed: %v", rt.networksRemoved)
	}
	actual, _ := rt.Inspect(context.Background())
	if len(actual.Networks) != 0 {
		t.Fatalf("network still there: %+v", actual.Networks)
	}
}

func TestRemoveAllAndPruneTakeNetworksWithThem(t *testing.T) {
	rt := newFakeRuntime()
	e, repoDir := newTestEngine(t, rt, twoAppsOnANetwork)
	if _, err := e.Reconcile(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}

	apps, nets, _, err := e.RemoveCandidates(context.Background(), nil, true)
	if err != nil || strings.Join(apps, ",") != "api,web" || strings.Join(nets, ",") != "backend" {
		t.Fatalf("candidates: %v %v %v", apps, nets, err)
	}
	// By name, networks are not up for removal: they go with --all or prune.
	if _, nets, _, _ := e.RemoveCandidates(context.Background(), []string{"api"}, false); len(nets) != 0 {
		t.Fatalf("a named removal must not touch networks: %v", nets)
	}

	// Prune sees the orphan network once Git drops the pods on it.
	writeRepo(t, repoDir, oneApp)
	apps, nets, _, err = e.PruneCandidates(context.Background())
	if err != nil || strings.Join(apps, ",") != "web" || strings.Join(nets, ",") != "backend" {
		t.Fatalf("prune candidates: %v %v %v", apps, nets, err)
	}
	res, err := e.Prune(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Removed, ",") != "web" || strings.Join(res.Networks, ",") != "backend" || !res.OK() {
		t.Fatalf("prune result: %+v", res)
	}
	if got := rt.removed; len(got) != 1 || len(rt.networksRemoved) != 1 || got[0] != "web" {
		t.Fatalf("pods before networks: removed %v networks %v", got, rt.networksRemoved)
	}

	// --all on what is left takes the application and would take any network.
	all, err := e.Remove(context.Background(), nil, true)
	if err != nil || strings.Join(all.Removed, ",") != "api" || len(all.Networks) != 0 {
		t.Fatalf("remove --all: %+v %v", all, err)
	}
	actual, _ := rt.Inspect(context.Background())
	if len(actual.Apps) != 0 || len(actual.Networks) != 0 {
		t.Fatalf("the host should be empty: %+v", actual)
	}
}

func TestRemoveResultEmptyCountsNetworks(t *testing.T) {
	if !(RemoveResult{}).Empty() {
		t.Fatal("nothing removed is empty")
	}
	if (RemoveResult{Networks: []string{"backend"}}).Empty() {
		t.Fatal("a removed network is a change")
	}
	if (RemoveResult{Failed: map[string]error{"network backend": context.Canceled}}).Err() == nil {
		t.Fatal("a failed network removal is a failure")
	}
}
