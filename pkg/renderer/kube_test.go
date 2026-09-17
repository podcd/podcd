package renderer

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/podcd/podcd/pkg/model"
)

func kubeApp() model.Application {
	app := model.Application{
		Name:          "web",
		Image:         "example.com/web@sha256:aaaa",
		RestartPolicy: "always",
	}
	app.SetManifest([]byte("apiVersion: v1\nkind: Pod\nmetadata:\n  name: web\nspec:\n  containers:\n    - name: web\n      image: example.com/web@sha256:aaaa\n"))
	return app
}

func TestKubeUnitPointsAtTheManifest(t *testing.T) {
	r := &Renderer{UnitDir: "/units", KubeDir: "/kube"}
	u, err := r.Render(kubeApp())
	if err != nil {
		t.Fatal(err)
	}
	if u.FileName != "podcd-web.kube" || u.ServiceName != "podcd-web.service" {
		t.Errorf("names: %q %q", u.FileName, u.ServiceName)
	}
	if u.ManifestPath != "/kube/web.yaml" {
		t.Errorf("manifest path = %q", u.ManifestPath)
	}
	got := string(u.Content)
	for _, want := range []string{"[Kube]", "Yaml=/kube/web.yaml", "WantedBy=default.target", "# podcd-manifest-hash: " + u.ManifestHash} {
		if !strings.Contains(got, want) {
			t.Errorf("unit is missing %q:\n%s", want, got)
		}
	}
	m := ParseMarkers(u.Content)
	if m.ManifestHash != u.ManifestHash || !m.Managed {
		t.Errorf("markers did not round-trip: %+v", m)
	}
}

func TestManifestChangeChangesTheUnit(t *testing.T) {
	r := &Renderer{UnitDir: "/units", KubeDir: "/kube"}
	before, err := r.Render(kubeApp())
	if err != nil {
		t.Fatal(err)
	}
	app := kubeApp()
	app.SetManifest(append(app.Manifest, []byte("      args: [\"--flag\"]\n")...))
	after, err := r.Render(app)
	if err != nil {
		t.Fatal(err)
	}
	if string(before.Content) == string(after.Content) {
		t.Fatal("a changed manifest must change the unit bytes, or the planner never notices")
	}
}

func TestOnlyKubeFilesAreOurs(t *testing.T) {
	if app, ok := AppFromFileName("podcd-web.kube"); !ok || app != "web" {
		t.Errorf("AppFromFileName(podcd-web.kube) = %q, %v", app, ok)
	}
	// podcd writes nothing but .kube units, so a .container sharing the prefix
	// belongs to somebody else.
	if _, ok := AppFromFileName("podcd-web.container"); ok {
		t.Error("a .container unit must not be claimed")
	}
}

func TestQuadletAcceptsAKubeUnit(t *testing.T) {
	quadlet := quadletBinary(t)
	dir := t.TempDir()
	r := &Renderer{UnitDir: dir, KubeDir: dir}
	u, err := r.Render(kubeApp())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, u.FileName), u.Content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(u.ManifestPath, u.Manifest, 0o600); err != nil {
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
		t.Fatalf("quadlet rejected the kube unit:\n%s", text)
	}
	if !strings.Contains(text, "kube play") || !strings.Contains(text, u.ManifestPath) {
		t.Fatalf("expected a kube play command for the manifest:\n%s", text)
	}
}
