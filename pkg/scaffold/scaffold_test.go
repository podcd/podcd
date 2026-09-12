package scaffold

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/podcd/podcd/pkg/config"
)

const digest = "@sha256:0000000000000000000000000000000000000000000000000000000000000000"

// resolve loads the given files as one repository and compiles host.
func resolve(t *testing.T, host string, files map[string]string) []string {
	t.Helper()
	dir := t.TempDir()
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ix := config.NewIndex()
	if err := ix.LoadTree("test", dir); err != nil {
		t.Fatalf("scaffolded documents do not load: %v", err)
	}
	desired, err := ix.Resolve(context.Background(), config.ResolveOptions{Host: host})
	if err != nil {
		t.Fatalf("scaffolded documents do not resolve for %s: %v", host, err)
	}
	return desired.Names()
}

func TestInitProducesARepositoryThatResolves(t *testing.T) {
	dir := t.TempDir()
	written, err := Init(dir, "vm-1", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(written, ",") != "README.md,apps.yaml,hosts.yaml" {
		t.Fatalf("written = %v", written)
	}
	ix := config.NewIndex()
	if err := ix.LoadTree("init", dir); err != nil {
		t.Fatal(err)
	}
	desired, err := ix.Resolve(context.Background(), config.ResolveOptions{Host: "vm-1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(desired.Names(), ",") != "nginx" {
		t.Fatalf("vm-1 should run nginx, got %v", desired.Names())
	}
	app := desired.Applications[0]
	if !strings.Contains(app.Image, "@sha256:") || app.Healthcheck == nil || app.Healthcheck.HTTP == nil || app.Ports[0].Host != 8080 {
		t.Fatalf("the scaffolded nginx is not what the README promises: %+v", app)
	}

	// Files are not clobbered by a second init.
	if _, err := Init(dir, "vm-1", false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("a second init must refuse without --force, got %v", err)
	}
	if _, err := Init(dir, "vm-2", true); err != nil {
		t.Fatal(err)
	}
}

func TestCreatedDocumentsComposeIntoARepository(t *testing.T) {
	app, err := Application(ApplicationOptions{Name: "api", Image: "ghcr.io/you/api" + digest,
		Ports: []string{"8081:8080"}, Env: []string{"LOG_LEVEL=info"}, HealthPath: "/health"})
	if err != nil {
		t.Fatal(err)
	}
	pod, err := Pod(PodOptions{Name: "side", Image: "ghcr.io/you/side" + digest, Ports: []string{"8082:80"}})
	if err != nil {
		t.Fatal(err)
	}
	group, err := Group("web", []string{"api", "side"})
	if err != nil {
		t.Fatal(err)
	}
	env, err := Environment("prod", []string{"api"})
	if err != nil {
		t.Fatal(err)
	}
	host, err := Host(HostOptions{Name: "vm-1", Environment: "prod", Groups: []string{"web"}})
	if err != nil {
		t.Fatal(err)
	}
	names := resolve(t, "vm-1", map[string]string{
		"apps.yaml":  string(app) + "---\n" + string(pod),
		"infra.yaml": string(group) + "---\n" + string(env) + "---\n" + string(host),
	})
	if strings.Join(names, ",") != "api,side" {
		t.Fatalf("vm-1 should run api and side, got %v", names)
	}

	// Output is block-style YAML in declaration order, without the k8s noise.
	if !strings.Contains(string(app), "spec:\n  image: ") || strings.Contains(string(app), `"`) {
		t.Errorf("application should be block YAML with image first:\n%s", app)
	}
	for _, noise := range []string{"creationTimestamp", "status:", "resources: {}"} {
		if strings.Contains(string(pod), noise) {
			t.Errorf("pod output should not contain %q:\n%s", noise, pod)
		}
	}
}

func TestCreateEnforcesTheDigestRule(t *testing.T) {
	if _, err := Application(ApplicationOptions{Name: "x", Image: "nginx:alpine"}); err == nil || !strings.Contains(err.Error(), "not pinned") {
		t.Fatalf("an unpinned application image must be refused, got %v", err)
	}
	if _, err := Pod(PodOptions{Name: "x", Image: "nginx:alpine"}); err == nil {
		t.Fatal("an unpinned pod image must be refused")
	}
}

func TestCreateRejectsBadInputBeforeWriting(t *testing.T) {
	for _, o := range []ApplicationOptions{
		{Name: "x", Image: "i" + digest, Ports: []string{"eighty"}},
		{Name: "x", Image: "i" + digest, Env: []string{"NOEQUALS"}},
		{Name: "x", Image: "i" + digest, HealthPath: "/"}, // no port to probe
		{Name: "", Image: "i" + digest},
	} {
		if _, err := Application(o); err == nil {
			t.Errorf("%+v should be rejected", o)
		}
	}
}
