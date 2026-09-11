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

// TestQuadletAcceptsRenderedUnits runs the real generator over a unit that uses
// every feature the renderer can emit.
//
// This is the test that catches the difference between "looks like a Quadlet
// file" and "is one": Quadlet rejects unknown keys outright, and which keys
// exist changes between podman releases.
func TestQuadletAcceptsRenderedUnits(t *testing.T) {
	quadlet := quadletBinary(t)

	dir := t.TempDir()
	r := &Renderer{UnitDir: dir, EnvDir: dir}
	app := model.Application{
		Name:       "kitchen-sink",
		Image:      "docker.io/library/busybox@sha256:aaaa",
		Entrypoint: []string{"/bin/sh", "-c", "echo hello world"},
		Command:    []string{"--flag", "value with spaces"},
		WorkingDir: "/srv",
		Env:        map[string]string{"A": "1", "GREETING": "hello world"},
		SecretEnv:  map[string]string{"TOKEN": "s3cret"},
		Ports: []model.Port{
			{Host: 8080, Container: 80, Protocol: "tcp"},
			{Host: 9090, Container: 9090, Protocol: "udp", HostIP: "127.0.0.1"},
		},
		Volumes:       []model.Volume{{Source: "/srv/data", Destination: "/data", Options: "ro"}},
		Networks:      []string{"podman"},
		Labels:        map[string]string{"team": "platform"},
		User:          "1000:1000",
		RestartPolicy: "on-failure",
		StopTimeout:   30,
		Resources:     model.Resources{Memory: "256M", CPU: "50%"},
	}

	unit, err := r.Render(app)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, unit.FileName), unit.Content, 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(quadlet, "-dryrun", "-user")
	cmd.Env = append(os.Environ(), "QUADLET_UNIT_DIRS="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running quadlet: %v\n%s", err, out)
	}
	text := string(out)

	// Quadlet reports a rejected unit as `converting "x.container": ...`.
	if strings.Contains(text, "unsupported key") || strings.Contains(text, `": converting`) {
		t.Fatalf("quadlet rejected the rendered unit:\n%s\n--- unit ---\n%s", text, unit.Content)
	}
	if !strings.Contains(text, "ExecStart=") {
		t.Fatalf("quadlet produced no service:\n%s", text)
	}

	// The flags that matter must reach podman as single arguments.
	for _, want := range []string{
		`--entrypoint=[\"/bin/sh\",\"-c\",\"echo hello world\"]`,
		"--workdir=/srv",
		"--publish 8080:80",
		"--publish 127.0.0.1:9090:9090/udp",
		"-v /srv/data:/data:ro",
		"--env-file",
		"--user=1000:1000",
		"busybox@sha256:aaaa",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the generated command is missing %q:\n%s", want, text)
		}
	}
}

// TestQuadletAcceptsAMinimalUnit covers the common case: no entrypoint, no
// secrets, no extras.
func TestQuadletAcceptsAMinimalUnit(t *testing.T) {
	quadlet := quadletBinary(t)

	dir := t.TempDir()
	r := &Renderer{UnitDir: dir}
	unit, err := r.Render(model.Application{
		Name:          "plain",
		Image:         "docker.io/library/busybox@sha256:aaaa",
		RestartPolicy: "always",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, unit.FileName), unit.Content, 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(quadlet, "-dryrun", "-user")
	cmd.Env = append(os.Environ(), "QUADLET_UNIT_DIRS="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running quadlet: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "unsupported key") {
		t.Fatalf("quadlet rejected the unit:\n%s\n--- unit ---\n%s", out, unit.Content)
	}
	// Quadlet's own ExecStart uses --replace, which is what makes re-applying an
	// application safe when a container of the same name is still around.
	if !strings.Contains(string(out), "--replace") {
		t.Errorf("expected quadlet to generate a --replace run command:\n%s", out)
	}
}
