package renderer

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/podcd/podcd/pkg/model"
)

// quadletBinary finds podman's Quadlet generator, which is the only authority on
// which keys a rendered unit may use.
func quadletBinary(t *testing.T) string {
	t.Helper()
	for _, p := range []string{
		"/usr/libexec/podman/quadlet",
		"/usr/lib/podman/quadlet",
		"/usr/lib/systemd/user-generators/podman-user-generator",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skip("podman's quadlet generator is not installed here")
	return ""
}

// TestQuadletAcceptsAKubeUnitWithResources verifies that resource limits and
// network directives written to a .kube unit are accepted by the generator.
func TestQuadletAcceptsAKubeUnitWithResources(t *testing.T) {
	quadlet := quadletBinary(t)

	dir := t.TempDir()
	app := model.Application{
		Name:          "kitchen-sink",
		RestartPolicy: "on-failure",
		StopTimeout:   30,
		Resources:     model.Resources{Memory: "256M", CPU: "50%"},
		Networks:      []string{"podman"},
	}
	app.SetManifest([]byte("apiVersion: v1\nkind: Pod\nmetadata:\n  name: kitchen-sink\nspec:\n  containers:\n    - name: app\n      image: docker.io/library/busybox@sha256:aaaa\n"))

	r := &Renderer{UnitDir: dir, KubeDir: dir}
	unit, err := r.Render(app)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, unit.FileName), unit.Content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit.ManifestPath, unit.Manifest, 0o600); err != nil {
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
		t.Fatalf("quadlet rejected the rendered unit:\n%s\n--- unit ---\n%s", text, unit.Content)
	}
	if !strings.Contains(text, "kube play") {
		t.Fatalf("expected a kube play command in the generated service:\n%s", text)
	}
	for _, want := range []string{"MemoryMax=256M", "CPUQuota=50%", "TimeoutStopSec=30"} {
		if !strings.Contains(string(unit.Content), want) {
			t.Errorf("unit is missing %q:\n%s", want, unit.Content)
		}
	}
}
