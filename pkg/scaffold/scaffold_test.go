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
	if !strings.Contains(app.Image, "@sha256:") || app.Ports[0].Host != 8080 {
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
	app, err := Pod(PodOptions{Name: "api", Image: "ghcr.io/you/api" + digest,
		Ports: []string{"8081:8080"}, Env: []string{"LOG_LEVEL=info"}})
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

	// Output is block-style YAML without the k8s noise.
	for _, noise := range []string{"creationTimestamp", "status:", "resources: {}"} {
		for _, doc := range [][]byte{app, pod} {
			if strings.Contains(string(doc), noise) {
				t.Errorf("output should not contain %q:\n%s", noise, doc)
			}
		}
	}
}

func TestCreateAcceptsATag(t *testing.T) {
	if _, err := Pod(PodOptions{Name: "x", Image: "nginx:alpine"}); err != nil {
		t.Fatalf("a tag must be accepted: %v", err)
	}
	if _, err := Pod(PodOptions{Name: "x"}); err == nil {
		t.Fatal("--image is required")
	}
}

func TestCreateRejectsBadInputBeforeWriting(t *testing.T) {
	for _, o := range []PodOptions{
		{Name: "x", Image: "i" + digest, Ports: []string{"eighty"}},
		{Name: "x", Image: "i" + digest, Env: []string{"NOEQUALS"}},
		{Name: "", Image: "i" + digest},
	} {
		if _, err := Pod(o); err == nil {
			t.Errorf("%+v should be rejected", o)
		}
	}
}

func TestCreateNetworkRendersAValidDocument(t *testing.T) {
	doc, err := Network(NetworkOptions{Name: "backend", Subnet: "10.90.0.0/24", Gateway: "10.90.0.1", Internal: true, DNS: []string{"10.90.0.53"}, Options: []string{"mtu=1400"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"kind: Network", "name: backend", "subnet: 10.90.0.0/24", "gateway: 10.90.0.1", "internal: true", "- 10.90.0.53", `mtu: "1400"`} {
		if !strings.Contains(string(doc), want) {
			t.Errorf("missing %q:\n%s", want, doc)
		}
	}
	empty, err := Network(NetworkOptions{Name: "ai"})
	if err != nil || !strings.Contains(string(empty), "spec: {}") {
		t.Fatalf("an empty spec is a valid network: %v\n%s", err, empty)
	}
	for _, o := range []NetworkOptions{
		{Name: ""},
		{Name: "x", Gateway: "10.0.0.1"},
		{Name: "x", IPRange: "10.0.0.0/28"},
		{Name: "x", Options: []string{"nope"}},
	} {
		if _, err := Network(o); err == nil {
			t.Errorf("%+v should be rejected", o)
		}
	}
}
