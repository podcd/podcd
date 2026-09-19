package renderer

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/podcd/podcd/pkg/model"
)

func TestRenderNetworkWritesEveryQuadletKeyItKnows(t *testing.T) {
	r := &Renderer{UnitDir: "/units", KubeDir: "/kube"}
	net := model.Network{
		Name: "backend", Driver: "bridge", Subnet: "10.90.0.0/24", Gateway: "10.90.0.1", IPRange: "10.90.0.128/25",
		Internal: true, IPv6: true, DisableDNS: true, DNS: []string{"10.90.0.53"},
		Options:    map[string]string{"mtu": "1400", "isolate": "true"},
		SourceRepo: "infra", Origins: []string{"net.yaml:1"},
	}
	u, err := r.RenderNetwork(net)
	if err != nil {
		t.Fatal(err)
	}
	if u.FileName != "podcd-backend.network" || u.Path != "/units/podcd-backend.network" || u.ServiceName != "podcd-backend-network.service" {
		t.Fatalf("names: %+v", u)
	}
	text := string(u.Content)
	for _, want := range []string{
		"# Managed by podcd", "# podcd-network: backend", "# podcd-spec-hash: " + net.SpecHash(),
		"[Network]\n", "NetworkName=backend\n", "Label=io.podcd.managed=true\n", "Label=io.podcd.network=backend\n",
		"Driver=bridge\n", "Subnet=10.90.0.0/24\n", "Gateway=10.90.0.1\n", "IPRange=10.90.0.128/25\n",
		"Internal=true\n", "IPv6=true\n", "DisableDNS=true\n", "DNS=10.90.0.53\n",
		"Options=isolate=true\nOptions=mtu=1400\n", // sorted, so the bytes are stable
		"[Install]\nWantedBy=default.target\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("unit is missing %q:\n%s", want, text)
		}
	}
	m := ParseMarkers(u.Content)
	if !m.Managed || m.Network != "backend" || m.App != "" || m.SpecHash != net.SpecHash() {
		t.Fatalf("markers: %+v", m)
	}

	// The same network renders to the same bytes; provenance is not part of it.
	moved := net
	moved.SourceRepo, moved.Origins = "elsewhere", nil
	again, _ := r.RenderNetwork(moved)
	if string(again.Content) != text {
		t.Fatal("rendering is not deterministic across provenance")
	}
}

func TestRenderNetworkMinimalSpecIsAPlainBridge(t *testing.T) {
	r := &Renderer{UnitDir: "/units"}
	u, err := r.RenderNetwork(model.Network{Name: "ai"})
	if err != nil {
		t.Fatal(err)
	}
	text := string(u.Content)
	for _, absent := range []string{"Driver=", "Subnet=", "Internal=", "IPv6=", "DNS=", "Options="} {
		if strings.Contains(text, absent) {
			t.Errorf("an empty spec must not write %q:\n%s", absent, text)
		}
	}
	if _, err := r.RenderNetwork(model.Network{}); err == nil {
		t.Fatal("a network without a name must not render")
	}
	if _, err := r.RenderNetwork(model.Network{Name: "x", Subnet: "10.0.0.0/8\nEvil=1"}); err == nil {
		t.Fatal("a newline in a value must be refused, not written into the unit")
	}
}

// A pod on a managed network names the network's unit, which is how Quadlet
// orders the two; one on any other network names the network as it is.
func TestKubeUnitRefersToManagedNetworksByTheirUnit(t *testing.T) {
	r := &Renderer{UnitDir: "/units", KubeDir: "/kube"}
	app := model.Application{Name: "api", Networks: []string{"backend", "podman"}, ManagedNetworks: []string{"backend"}}
	app.SetManifest([]byte("apiVersion: v1\nkind: Pod\n"))
	u, err := r.Render(app)
	if err != nil {
		t.Fatal(err)
	}
	text := string(u.Content)
	if !strings.Contains(text, "Network=podcd-backend.network\n") || !strings.Contains(text, "Network=podman\n") {
		t.Fatalf("network lines:\n%s", text)
	}
	if strings.Contains(text, "Network=backend\n") {
		t.Fatal("a managed network must not also be named bare")
	}
}

func TestNetworkFileNames(t *testing.T) {
	if n, ok := NetworkFromFileName("podcd-ai.network"); !ok || n != "ai" {
		t.Fatalf("got %q %v", n, ok)
	}
	for _, not := range []string{"podcd-ai.kube", "ai.network", "podcd-ai.container"} {
		if _, ok := NetworkFromFileName(not); ok {
			t.Errorf("%q must not be taken for a managed network", not)
		}
	}
	if NetworkServiceName("ai") != "podcd-ai-network.service" {
		t.Fatal(NetworkServiceName("ai"))
	}
}

// The generator is the only authority on which keys a .network unit may
// carry, and on whether Network=<file>.network in a .kube unit resolves.
func TestQuadletAcceptsANetworkUnitAndAKubeUnitDependingOnIt(t *testing.T) {
	quadlet := quadletBinary(t)
	dir := t.TempDir()
	r := &Renderer{UnitDir: dir, KubeDir: dir}

	nu, err := r.RenderNetwork(model.Network{
		Name: "backend", Driver: "bridge", Subnet: "10.90.0.0/24", Gateway: "10.90.0.1", IPRange: "10.90.0.128/25",
		Internal: true, DNS: []string{"10.90.0.53"}, Options: map[string]string{"mtu": "1400"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nu.Path, nu.Content, 0o644); err != nil {
		t.Fatal(err)
	}

	app := model.Application{Name: "api", Networks: []string{"backend"}, ManagedNetworks: []string{"backend"}}
	app.SetManifest([]byte("apiVersion: v1\nkind: Pod\nmetadata:\n  name: api\nspec:\n  containers:\n    - name: app\n      image: docker.io/library/busybox@sha256:aaaa\n"))
	ku, err := r.Render(app)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ku.FileName), ku.Content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ku.ManifestPath, ku.Manifest, 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(quadlet, "-dryrun", "-user")
	cmd.Env = append(os.Environ(), "QUADLET_UNIT_DIRS="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running quadlet: %v\n%s", err, out)
	}
	text := string(out)
	if strings.Contains(text, "unsupported key") || strings.Contains(text, `": converting`) {
		t.Fatalf("quadlet rejected a rendered unit:\n%s\n--- network ---\n%s\n--- kube ---\n%s", text, nu.Content, ku.Content)
	}
	for _, want := range []string{
		"network create", "--ignore", "--subnet=10.90.0.0/24", "--internal", "--opt mtu=1400", "--label io.podcd.network=backend backend",
		"Requires=podcd-backend-network.service", "After=podcd-backend-network.service", "--network=backend",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("generated services are missing %q:\n%s", want, text)
		}
	}
}
