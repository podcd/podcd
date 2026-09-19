package podman

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/podcd/podcd/pkg/model"
	"github.com/podcd/podcd/pkg/renderer"
)

// networkLsJSON is `podman network ls --format json` from podman 4.9 with
// podman's default network, one made by hand, and one a podcd unit created.
// Fields podcd does not read are omitted; the ones it reads are exact.
const networkLsJSON = `[
 {"name":"podman","id":"2f259bab93aa","driver":"bridge","network_interface":"podman0","subnets":[{"subnet":"10.88.0.0/16","gateway":"10.88.0.1"}],"ipv6_enabled":false,"internal":false,"dns_enabled":false,"ipam_options":{"driver":"host-local"}},
 {"name":"byhand","id":"e564e8ab614d","driver":"bridge","network_interface":"podman2","subnets":[{"subnet":"10.89.1.0/24","gateway":"10.89.1.1"}],"ipv6_enabled":false,"internal":false,"dns_enabled":true,"ipam_options":{"driver":"host-local"}},
 {"name":"orphan","id":"aa11bb22cc33","driver":"bridge","network_interface":"podman3","labels":{"io.podcd.managed":"true","io.podcd.network":"orphan"},"subnets":[{"subnet":"10.89.2.0/24","gateway":"10.89.2.1"}],"ipv6_enabled":false,"internal":false,"dns_enabled":true,"ipam_options":{"driver":"host-local"}}
]`

func writeNetworkUnit(t *testing.T, r *Runtime, n model.Network) renderer.NetworkUnit {
	t.Helper()
	u, err := r.rend.RenderNetwork(n)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(u.Path, u.Content, 0o644); err != nil {
		t.Fatal(err)
	}
	return u
}

func calls(f *fake) []string {
	var got []string
	for _, c := range f.calls {
		got = append(got, c.bin+" "+c.args)
	}
	return got
}

func TestInspectReadsNetworkUnitsAndPodmanTogether(t *testing.T) {
	r, f := newRuntime(t, nil)
	writeNetworkUnit(t, r, model.Network{Name: "backend", Subnet: "10.90.0.0/24"}) // unit, but podman does not have it
	if err := os.WriteFile(filepath.Join(r.unitDir, "podcd-foreign.network"), []byte("[Network]\nSubnet=10.1.0.0/24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.respond = func(bin string, args []string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case bin == "podman" && strings.HasPrefix(joined, "network ls"):
			return networkLsJSON, nil
		case bin == "podman":
			return "[]", nil
		case bin == "systemctl" && strings.Contains(joined, "podcd-backend-network.service"):
			return "Id=podcd-backend-network.service\nActiveState=inactive\nLoadState=loaded\n\n" +
				"Id=podcd-orphan-network.service\nActiveState=inactive\nLoadState=not-found\n", nil
		}
		return "", nil
	}

	st, err := r.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(st.NetworkNames(), ","); got != "backend,foreign,orphan" {
		t.Fatalf("want the managed unit, the foreign unit and the labelled orphan, got %s", got)
	}
	b := st.Networks["backend"]
	if !b.Managed || b.Exists || b.UnitState != model.UnitInactive || b.UnitName != "podcd-backend-network.service" {
		t.Fatalf("backend: %+v", b)
	}
	if o := st.Networks["orphan"]; !o.Managed || !o.Exists || o.UnitFile != "" || o.UnitState != model.UnitMissing {
		t.Fatalf("a labelled network with no unit is ours and missing its unit: %+v", o)
	}
	if st.Networks["foreign"].Managed {
		t.Fatal("a .network unit without podcd's header must not be claimed")
	}
	for _, not := range []string{"podman", "byhand"} {
		if _, ok := st.Networks[not]; ok {
			t.Errorf("%q is neither ours by unit nor by label and must not appear", not)
		}
	}
}

func TestInspectRefusesTwoUnitFilesForOneNetwork(t *testing.T) {
	r, f := newRuntime(t, nil)
	u := writeNetworkUnit(t, r, model.Network{Name: "backend"})
	if err := os.WriteFile(filepath.Join(r.unitDir, "podcd-backend-old.network"), u.Content, 0o644); err != nil {
		t.Fatal(err)
	}
	f.respond = func(string, []string) (string, error) { return "[]", nil }
	if _, err := r.Inspect(context.Background()); err == nil || !strings.Contains(err.Error(), "two unit files") {
		t.Fatalf("want a refusal, got %v", err)
	}
}

// A first ApplyNetwork writes the unit and starts its service, and nothing
// more: a network already there by that name is adopted, not replaced.
func TestApplyNetworkCreateWritesReloadsAndStarts(t *testing.T) {
	r, f := newRuntime(t, nil)
	n := model.Network{Name: "backend", Subnet: "10.90.0.0/24"}
	if err := r.ApplyNetwork(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	u, _ := r.rend.RenderNetwork(n)
	if _, err := os.Stat(u.Path); err != nil {
		t.Fatalf("unit not written: %v", err)
	}
	want := []string{"systemctl --user daemon-reload", "systemctl --user start podcd-backend-network.service"}
	if got := calls(f); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("want %v\n got %v", want, got)
	}
}

// A second ApplyNetwork is a change to a network podcd created, which Quadlet
// cannot do in place: stop the service (and with it every pod that requires
// it), remove the network, start the service so it is created anew.
func TestApplyNetworkUpdateRecreatesTheNetwork(t *testing.T) {
	r, f := newRuntime(t, nil)
	writeNetworkUnit(t, r, model.Network{Name: "backend", Subnet: "10.90.0.0/24"})
	if err := r.ApplyNetwork(context.Background(), model.Network{Name: "backend", Subnet: "10.91.0.0/24"}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"systemctl --user daemon-reload",
		"systemctl --user stop podcd-backend-network.service",
		"podman network rm backend",
		"systemctl --user start podcd-backend-network.service",
	}
	if got := calls(f); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("want %v\n got %v", want, got)
	}
	content, _ := os.ReadFile(filepath.Join(r.unitDir, "podcd-backend.network"))
	if !strings.Contains(string(content), "Subnet=10.91.0.0/24") {
		t.Fatal("the unit on disk was not rewritten")
	}
}

func TestApplyNetworkUpdateToleratesANetworkPodmanNoLongerHas(t *testing.T) {
	r, f := newRuntime(t, func(bin string, args []string) (string, error) {
		if bin == "podman" {
			return "", errors.New(`Error: unable to find network with name or ID backend: network not found`)
		}
		return "", nil
	})
	writeNetworkUnit(t, r, model.Network{Name: "backend"})
	if err := r.ApplyNetwork(context.Background(), model.Network{Name: "backend", Internal: true}); err != nil {
		t.Fatalf("a missing network is nothing to remove: %v", err)
	}
	if !f.has("systemctl", "start podcd-backend-network.service") {
		t.Fatal("the service must still be started")
	}
}

func TestApplyNetworkReportsANetworkStillInUse(t *testing.T) {
	r, _ := newRuntime(t, func(bin string, args []string) (string, error) {
		if bin == "podman" {
			return "", errors.New(`Error: "backend" has associated containers with it. Use -f to forcibly delete containers and pods: network is being used`)
		}
		return "", nil
	})
	writeNetworkUnit(t, r, model.Network{Name: "backend"})
	err := r.ApplyNetwork(context.Background(), model.Network{Name: "backend", Internal: true})
	if err == nil || !strings.Contains(err.Error(), "so it can be recreated") || !strings.Contains(err.Error(), "being used") {
		t.Fatalf("want the in-use error surfaced, never forced: %v", err)
	}
}

func TestRemoveNetworkStopsDeletesTheUnitAndTheNetwork(t *testing.T) {
	r, f := newRuntime(t, nil)
	u := writeNetworkUnit(t, r, model.Network{Name: "backend"})
	if err := r.RemoveNetwork(context.Background(), "backend"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(u.Path); !os.IsNotExist(err) {
		t.Fatal("the unit should be gone")
	}
	want := []string{
		"systemctl --user stop podcd-backend-network.service",
		"systemctl --user daemon-reload",
		"podman network rm backend",
	}
	if got := calls(f); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("want %v\n got %v", want, got)
	}
	for _, c := range f.calls {
		if strings.Contains(c.args, "--force") || strings.Contains(c.args, " -f") {
			t.Fatal("removing a network must never be forced")
		}
	}
}

func TestRemoveNetworkToleratesWhatIsAlreadyGone(t *testing.T) {
	r, _ := newRuntime(t, func(bin string, args []string) (string, error) {
		switch bin {
		case "systemctl":
			if args[1] == "stop" {
				return "", errors.New("Failed to stop podcd-backend-network.service: Unit podcd-backend-network.service not loaded.")
			}
		case "podman":
			return "", errors.New("Error: unable to find network with name or ID backend: network not found")
		}
		return "", nil
	})
	if err := r.RemoveNetwork(context.Background(), "backend"); err != nil {
		t.Fatalf("nothing to stop and nothing to remove is success: %v", err)
	}
}
