package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree writes files into a temporary directory and returns its path.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func loadIndex(t *testing.T, files map[string]string) *Index {
	t.Helper()
	ix := NewIndex()
	if err := ix.LoadTree("test", writeTree(t, files)); err != nil {
		t.Fatalf("loading tree: %v", err)
	}
	return ix
}

const digest = "@sha256:1111111111111111111111111111111111111111111111111111111111111111"

func baseFiles() map[string]string {
	return map[string]string{
		"apps/api.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Application
metadata:
  name: api
spec:
  image: example.com/api` + digest + `
  ports:
    - host: 8080
      container: 8080
  env:
    APP_ENV: default
    LOG_LEVEL: info
  healthcheck:
    http:
      port: 8080
      path: /health
`,
		"apps/frontend.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Application
metadata:
  name: frontend
spec:
  image: example.com/frontend` + digest + `
  ports:
    - "8081:80"
`,
		"envs/production.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Environment
metadata:
  name: production
spec:
  overrides:
    api:
      env:
        APP_ENV: production
`,
		"groups/web.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Group
metadata:
  name: web
spec:
  applications: [api, frontend]
  overrides:
    api:
      env:
        LOG_LEVEL: warn
`,
		"hosts/prod-web-01.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Host
metadata:
  name: prod-web-01
spec:
  environment: production
  groups: [web]
`,
	}
}

func TestResolveInheritance(t *testing.T) {
	ix := loadIndex(t, baseFiles())
	got, err := ix.Resolve(context.Background(), ResolveOptions{Host: "prod-web-01"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(got.Applications) != 2 {
		t.Fatalf("want 2 applications, got %d", len(got.Applications))
	}
	// Sorted output, always.
	if got.Applications[0].Name != "api" || got.Applications[1].Name != "frontend" {
		t.Fatalf("applications are not sorted: %v", got.Names())
	}
	api := got.Applications[0]
	// Environment beats the Application default; the group beats the environment.
	if api.Env["APP_ENV"] != "production" {
		t.Errorf("APP_ENV = %q, want production (environment override)", api.Env["APP_ENV"])
	}
	if api.Env["LOG_LEVEL"] != "warn" {
		t.Errorf("LOG_LEVEL = %q, want warn (group override beats environment)", api.Env["LOG_LEVEL"])
	}
	if api.RestartPolicy != "always" {
		t.Errorf("restart policy = %q, want the always default", api.RestartPolicy)
	}
	// Port shorthand "8081:80".
	fe := got.Applications[1]
	if len(fe.Ports) != 1 || fe.Ports[0].Host != 8081 || fe.Ports[0].Container != 80 || fe.Ports[0].Protocol != "tcp" {
		t.Errorf("frontend ports = %+v, want 8081->80/tcp", fe.Ports)
	}
}

func TestHostOverrideBeatsGroup(t *testing.T) {
	files := baseFiles()
	files["hosts/prod-web-01.yaml"] = `
apiVersion: gitops.podcd.io/v1
kind: Host
metadata:
  name: prod-web-01
spec:
  environment: production
  groups: [web]
  overrides:
    api:
      env:
        LOG_LEVEL: debug
  excludeApplications: [frontend]
`
	ix := loadIndex(t, files)
	got, err := ix.Resolve(context.Background(), ResolveOptions{Host: "prod-web-01"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(got.Applications) != 1 {
		t.Fatalf("exclude did not drop frontend: %v", got.Names())
	}
	if got.Applications[0].Env["LOG_LEVEL"] != "debug" {
		t.Errorf("host override did not win: %v", got.Applications[0].Env)
	}
}

func TestGroupOrderIsDeterministic(t *testing.T) {
	files := baseFiles()
	files["groups/edge.yaml"] = `
apiVersion: gitops.podcd.io/v1
kind: Group
metadata:
  name: edge
spec:
  overrides:
    api:
      env:
        LOG_LEVEL: trace
`
	files["hosts/prod-web-01.yaml"] = `
apiVersion: gitops.podcd.io/v1
kind: Host
metadata:
  name: prod-web-01
spec:
  environment: production
  groups: [web, edge]
`
	ix := loadIndex(t, files)
	for i := 0; i < 5; i++ {
		got, err := ix.Resolve(context.Background(), ResolveOptions{Host: "prod-web-01"})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		// The later group in the host's list wins, every time.
		if got.Applications[0].Env["LOG_LEVEL"] != "trace" {
			t.Fatalf("run %d: LOG_LEVEL = %q, want trace", i, got.Applications[0].Env["LOG_LEVEL"])
		}
	}
}

func TestImageMayBeATag(t *testing.T) {
	files := baseFiles()
	files["apps/api.yaml"] = `
apiVersion: gitops.podcd.io/v1
kind: Application
metadata:
  name: api
spec:
  image: example.com/api:latest
`
	ix := loadIndex(t, files)
	got, err := ix.Resolve(context.Background(), ResolveOptions{Host: "prod-web-01"})
	if err != nil {
		t.Fatalf("a tag is an acceptable image reference: %v", err)
	}
	if got.Applications[0].Image != "example.com/api:latest" {
		t.Errorf("image = %q", got.Applications[0].Image)
	}
}

func TestAllowMutableImageIsNotAField(t *testing.T) {
	// The field that used to relax the digest rule is gone with the rule, and
	// is rejected like any other unknown field.
	files := baseFiles()
	files["apps/api.yaml"] = `
apiVersion: gitops.podcd.io/v1
kind: Application
metadata:
  name: api
spec:
  image: example.com/api:latest
  allowMutableImage: true
`
	dir := writeTree(t, files)
	if err := NewIndex().LoadTree("repo", dir); err == nil || !strings.Contains(err.Error(), "allowMutableImage") {
		t.Fatalf("allowMutableImage should be rejected as an unknown field, got %v", err)
	}
}

func TestDuplicateApplicationIsAnError(t *testing.T) {
	ix := NewIndex()
	dir := writeTree(t, baseFiles())
	if err := ix.LoadTree("repo-a", dir); err != nil {
		t.Fatal(err)
	}
	err := ix.LoadTree("repo-b", dir)
	if err == nil {
		t.Fatal("the same Application in two repositories must be an error")
	}
	if !strings.Contains(err.Error(), "defined twice") {
		t.Errorf("error should name the conflict, got: %v", err)
	}
}

func TestUnknownHostIsAnError(t *testing.T) {
	ix := loadIndex(t, baseFiles())
	_, err := ix.Resolve(context.Background(), ResolveOptions{Host: "not-a-host"})
	if err == nil || !strings.Contains(err.Error(), "no Host document") {
		t.Fatalf("want a clear unknown-host error, got: %v", err)
	}
}

func TestMissingApplicationDefinitionIsAnError(t *testing.T) {
	files := baseFiles()
	delete(files, "apps/frontend.yaml")
	ix := loadIndex(t, files)
	_, err := ix.Resolve(context.Background(), ResolveOptions{Host: "prod-web-01"})
	if err == nil || !strings.Contains(err.Error(), "never defined") {
		t.Fatalf("want an error about the undefined application, got: %v", err)
	}
}

func TestHostPortConflictIsAnError(t *testing.T) {
	files := baseFiles()
	files["apps/frontend.yaml"] = `
apiVersion: gitops.podcd.io/v1
kind: Application
metadata:
  name: frontend
spec:
  image: example.com/frontend` + digest + `
  ports:
    - "8080:80"
`
	ix := loadIndex(t, files)
	_, err := ix.Resolve(context.Background(), ResolveOptions{Host: "prod-web-01"})
	if err == nil || !strings.Contains(err.Error(), "both publish host port 8080") {
		t.Fatalf("want a port conflict error, got: %v", err)
	}
}

func TestUnknownFieldIsRejected(t *testing.T) {
	ix := NewIndex()
	err := ix.LoadTree("test", writeTree(t, map[string]string{
		"a.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Application
metadata:
  name: api
spec:
  imagee: example.com/api` + digest + `
`}))
	if err == nil || !strings.Contains(err.Error(), "imagee") {
		t.Fatalf("a misspelled field must fail loudly, got: %v", err)
	}
}

func TestOverrideForApplicationNotOnHostIsAnError(t *testing.T) {
	files := baseFiles()
	files["groups/web.yaml"] = `
apiVersion: gitops.podcd.io/v1
kind: Group
metadata:
  name: web
spec:
  applications: [api]
  overrides:
    frontend:
      env:
        X: "1"
`
	ix := loadIndex(t, files)
	_, err := ix.Resolve(context.Background(), ResolveOptions{Host: "prod-web-01"})
	if err == nil || !strings.Contains(err.Error(), "does not run") {
		t.Fatalf("a stale override should be reported, got: %v", err)
	}
}


func TestAgentConfigDefaultsAndValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(path, []byte(`
host: prod-web-01
interval: 30s
repositories:
  - name: infrastructure
    url: https://example.com/infra.git
    revision: main
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadAgentConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Interval.String() != "30s" {
		t.Errorf("interval = %v, want 30s (durations must parse from YAML)", cfg.Interval)
	}
	if cfg.Runtime != "podman" {
		t.Errorf("runtime default = %q, want podman", cfg.Runtime)
	}
	if !cfg.PruneEnabled() {
		t.Error("prune should default to on")
	}
	if cfg.StateDir == "" || cfg.UnitDir == "" {
		t.Error("state and unit directories must have defaults")
	}
}

func TestAgentConfigRejectsEmptyRepositories(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(path, []byte("host: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAgentConfig(path); err == nil || !strings.Contains(err.Error(), "no repositories") {
		t.Fatalf("want an error about missing repositories, got: %v", err)
	}
}

func TestAgentConfigRejectsBadValuesPaths(t *testing.T) {
	base := AgentConfig{
		Runtime:      "podman",
		LogFormat:    "text",
		Repositories: []RepositorySpec{{Name: "infra", URL: "https://example.com/infra.git"}},
	}
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"absolute", "/etc/passwd"},
		{"escaping", "../../secrets.yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			cfg.Repositories = []RepositorySpec{{Name: "infra", URL: "https://example.com/infra.git", Values: []string{tc.value}}}
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "values file") {
				t.Fatalf("want a values-file error for %q, got: %v", tc.value, err)
			}
		})
	}

	cfg := base
	cfg.Repositories = []RepositorySpec{{Name: "infra", URL: "https://example.com/infra.git", Values: []string{"values/prod.yaml"}}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a relative values path should validate cleanly: %v", err)
	}
}
