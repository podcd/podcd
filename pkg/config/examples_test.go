package config

import (
	"context"
	"strings"
	"testing"

	"github.com/podcd/podcd/pkg/secrets"
)

// TestExamplesCompile keeps the documentation honest: the example repositories
// under examples/ are loaded and resolved exactly as the agent would, so an
// example that stops being valid fails the build instead of misleading someone.
func TestExamplesCompile(t *testing.T) {
	// The example api application reads its database password from the agent's
	// environment. That value is not in Git - which is the point - so the test
	// supplies it the way the VM would.
	t.Setenv("API_DATABASE_PASSWORD", "not-a-real-password")
	t.Setenv("METRICS_PUSH_TOKEN", "not-a-real-token")

	ix := NewIndex()
	if err := ix.LoadTree("infrastructure", "../../examples/infrastructure"); err != nil {
		t.Fatalf("loading the example infrastructure repository: %v", err)
	}
	if err := ix.LoadTree("applications", "../../examples/applications"); err != nil {
		t.Fatalf("loading the example applications repository: %v", err)
	}

	cases := []struct {
		host string
		apps []string
	}{
		// Both web hosts are in the same group, so both get the same applications
		// plus what the environment adds.
		{"prod-web-01", []string{"api", "frontend", "node-exporter"}},
		{"prod-web-02", []string{"api", "frontend", "metrics", "node-exporter"}},
		// Staging excludes the frontend.
		{"staging-web-01", []string{"api", "node-exporter"}},
	}

	for _, tc := range cases {
		got, err := ix.Resolve(context.Background(), ResolveOptions{Host: tc.host, Secrets: fakeSecrets()})
		if err != nil {
			t.Fatalf("%s: %v", tc.host, err)
		}
		if strings.Join(got.Names(), ",") != strings.Join(tc.apps, ",") {
			t.Errorf("%s runs %v, want %v", tc.host, got.Names(), tc.apps)
		}
	}

	// The canary host overrides one value and nothing else.
	one, err := ix.Resolve(context.Background(), ResolveOptions{Host: "prod-web-01", Secrets: fakeSecrets()})
	if err != nil {
		t.Fatal(err)
	}
	two, err := ix.Resolve(context.Background(), ResolveOptions{Host: "prod-web-02", Secrets: fakeSecrets()})
	if err != nil {
		t.Fatal(err)
	}
	api1, _ := one.App("api")
	api2, _ := two.App("api")
	if api1.Env["LOG_LEVEL"] != "warn" {
		t.Errorf("prod-web-01 LOG_LEVEL = %q, want warn from the environment", api1.Env["LOG_LEVEL"])
	}
	if api2.Env["LOG_LEVEL"] != "debug" {
		t.Errorf("prod-web-02 LOG_LEVEL = %q, want debug from the host override", api2.Env["LOG_LEVEL"])
	}
	if api1.Image != api2.Image {
		t.Error("the two hosts should be running the same image")
	}
	if api1.Resources.Memory != "1G" {
		t.Errorf("group override lost: memory = %q", api1.Resources.Memory)
	}

	// The example pod compiles to a played manifest with its ConfigMap, its
	// resolved Secret and the host's strategic merge patch applied.
	metrics, ok := two.App("metrics")
	if !ok || !metrics.IsKube() {
		t.Fatalf("prod-web-02 should run the metrics pod, got %+v", metrics)
	}
	manifest := string(metrics.Manifest)
	for _, want := range []string{"kind: ConfigMap", "kind: Secret", "kind: Pod", "SHIP_VERBOSE", "name: collector"} {
		if !strings.Contains(manifest, want) {
			t.Errorf("metrics manifest is missing %q", want)
		}
	}
	if strings.Contains(manifest, "not-a-real-token") {
		// base64 in data:, never plaintext - but this test only proves the
		// reference was replaced; the renderer test covers redaction.
		t.Error("the resolved secret should be base64 in data, not plaintext")
	}
	if metrics.Healthcheck == nil || metrics.Healthcheck.HTTP == nil || metrics.Healthcheck.HTTP.Port != 9200 {
		t.Errorf("the readiness probe should become the health check: %+v", metrics.Healthcheck)
	}
}

// fakeSecrets is the provider set the agent uses by default: environment and
// files on the host, never anything read out of Git.
func fakeSecrets() *secrets.Resolver { return secrets.Default("") }
