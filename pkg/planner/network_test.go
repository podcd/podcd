package planner

import (
	"strings"
	"testing"

	"github.com/podcd/podcd/pkg/model"
)

func network(name, subnet string) model.Network {
	return model.Network{Name: name, Subnet: subnet}
}

// appOn returns an application that joins net through podcd's own unit.
func appOn(name string, net string) model.Application {
	a := app(name, "img@sha256:a")
	a.Networks = []string{net}
	a.ManagedNetworks = []string{net}
	return a
}

// existing builds the actual state for a network exactly in sync.
func existing(t *testing.T, n model.Network) model.ActualNetwork {
	t.Helper()
	u, err := rend().RenderNetwork(n)
	if err != nil {
		t.Fatal(err)
	}
	return model.ActualNetwork{
		Name: n.Name, Managed: true, UnitFile: u.Path, UnitFileHash: model.HashBytes(u.Content),
		UnitContent: u.Content, SpecHash: u.SpecHash, UnitName: u.ServiceName, UnitState: model.UnitActive, Exists: true,
	}
}

func withNetworks(s model.ActualState, nets ...model.ActualNetwork) model.ActualState {
	s.Networks = map[string]model.ActualNetwork{}
	for _, n := range nets {
		s.Networks[n.Name] = n
	}
	return s
}

// summary renders a plan as "kind:type:name" lines, in plan order.
func summary(p model.Plan) []string {
	var out []string
	for _, a := range p.Changes() {
		out = append(out, string(a.Kind)+":"+string(a.Type)+":"+a.App)
	}
	return out
}

func TestNetworkIsCreatedBeforeThePodThatJoinsIt(t *testing.T) {
	d := desired(appOn("api", "backend"))
	d.Networks = []model.Network{network("backend", "10.90.0.0/24")}
	p, err := Build(d, actual(), rend(), Options{Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(summary(p), " ")
	if got != "network:create:backend :create:api" {
		t.Fatalf("want the network first, got %q", got)
	}
	if p.Actions[0].Network == nil || p.Actions[0].Network.Name != "backend" {
		t.Fatal("the network action must carry its spec")
	}
}

func TestNetworkInSyncIsANoOp(t *testing.T) {
	n := network("backend", "10.90.0.0/24")
	a := appOn("api", "backend")
	d := desired(a)
	d.Networks = []model.Network{n}
	p, err := Build(d, withNetworks(actual(running(t, a)), existing(t, n)), rend(), Options{Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Empty() {
		t.Fatalf("want nothing to do, got %v", summary(p))
	}
}

// Quadlet cannot change a network in place, so a changed one is recreated,
// and every pod on it - whose own unit did not change - is restarted after.
func TestChangedNetworkIsRecreatedAndItsPodsRestarted(t *testing.T) {
	old := network("backend", "10.90.0.0/24")
	a, b := appOn("api", "backend"), appOn("worker", "backend")
	other := app("other", "img@sha256:a")
	d := desired(a, b, other)
	d.Networks = []model.Network{network("backend", "10.91.0.0/24")}

	p, err := Build(d, withNetworks(actual(running(t, a), running(t, b), running(t, other)), existing(t, old)), rend(), Options{Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(summary(p), " ")
	if got != "network:update:backend :restart:api :restart:worker" {
		t.Fatalf("want recreate then restarts of the pods on it only, got %q", got)
	}
	if !strings.Contains(p.Actions[0].Reason, "recreated") {
		t.Fatalf("reason: %q", p.Actions[0].Reason)
	}
	var details string
	for _, l := range p.Actions[0].Details {
		details += l + "\n"
	}
	if !strings.Contains(details, "- Subnet=10.90.0.0/24") || !strings.Contains(details, "+ Subnet=10.91.0.0/24") {
		t.Fatalf("want the changed line explained, got %q", details)
	}
}

func TestNetworkUnitPresentButNetworkGoneIsRestarted(t *testing.T) {
	n := network("backend", "")
	a := appOn("api", "backend")
	d := desired(a)
	d.Networks = []model.Network{n}
	cur := existing(t, n)
	cur.Exists = false
	p, err := Build(d, withNetworks(actual(running(t, a)), cur), rend(), Options{Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(summary(p), " "); got != "network:restart:backend" {
		t.Fatalf("got %q", got)
	}
}

// Removing the last pod on a network removes the network, after the pod.
func TestOrphanNetworkIsRemovedAfterItsPods(t *testing.T) {
	n := network("backend", "")
	a := appOn("api", "backend")
	p, err := Build(desired(), withNetworks(actual(running(t, a)), existing(t, n)), rend(), Options{Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(summary(p), " "); got != ":delete:api network:delete:backend" {
		t.Fatalf("want the pod gone before the network, got %q", got)
	}
	if !p.Actions[1].Destructive {
		t.Fatal("removing a network is destructive")
	}

	noPrune, _ := Build(desired(), withNetworks(actual(), existing(t, n)), rend(), Options{Prune: false})
	if !noPrune.Empty() || len(noPrune.Actions) != 1 || !strings.Contains(noPrune.Actions[0].Reason, "pruning is disabled") {
		t.Fatalf("with pruning off the orphan is reported, not removed: %+v", noPrune.Actions)
	}
}

// A network whose only pod could not be compiled this round is kept: the
// pod's unit on disk still names it.
func TestNetworkOfAProtectedApplicationIsKept(t *testing.T) {
	n := network("backend", "")
	a := appOn("api", "backend")
	act := withNetworks(actual(running(t, a)), existing(t, n))
	p, err := Build(desired(), act, rend(), Options{Prune: true, Protected: map[string]bool{"api": true}})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Empty() {
		t.Fatalf("want nothing removed, got %v", summary(p))
	}
}

func TestUnmanagedNetworkUnitIsRefused(t *testing.T) {
	d := desired(appOn("api", "backend"))
	d.Networks = []model.Network{network("backend", "")}
	foreign := model.ActualNetwork{Name: "backend", Managed: false, UnitFile: "/units/podcd-backend.network", Exists: true}
	_, err := Build(d, withNetworks(actual(), foreign), rend(), Options{Prune: true})
	if err == nil || !strings.Contains(err.Error(), "not managed by podcd") {
		t.Fatalf("want a refusal, got %v", err)
	}
	// A foreign one Git does not name is simply left alone.
	p, err := Build(desired(), withNetworks(actual(), foreign), rend(), Options{Prune: true})
	if err != nil || !p.Empty() {
		t.Fatalf("got %v %v", summary(p), err)
	}
}

// A renamed network: the old one goes, the new one comes, and the pod moves.
func TestFullOrderingAcrossKinds(t *testing.T) {
	oldNet, newNet := network("old", ""), network("new", "")
	oldApp := appOn("gone", "old")
	stay := appOn("api", "old")
	moved := appOn("api", "new")
	d := desired(moved)
	d.Networks = []model.Network{newNet}
	p, err := Build(d, withNetworks(actual(running(t, oldApp), running(t, stay)), existing(t, oldNet)), rend(), Options{Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(summary(p), " ")
	want := ":delete:gone network:delete:old network:create:new :update:api"
	if got != want {
		t.Fatalf("want %q\n got %q", want, got)
	}
}
