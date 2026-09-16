package config

import (
	"context"
	"strings"
	"testing"
)

func TestExamplesCompile(t *testing.T) {
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
		{"local", []string{"local"}},
	}

	for _, tc := range cases {
		got, err := ix.Resolve(context.Background(), ResolveOptions{Host: tc.host})
		if err != nil {
			t.Fatalf("%s: %v", tc.host, err)
		}
		if strings.Join(got.Names(), ",") != strings.Join(tc.apps, ",") {
			t.Errorf("%s runs %v, want %v", tc.host, got.Names(), tc.apps)
		}
	}

	// The minimal local example has a single nginx app.
	local, err := ix.Resolve(context.Background(), ResolveOptions{Host: "local"})
	if err != nil {
		t.Fatal(err)
	}
	app, ok := local.App("local")
	if !ok {
		t.Fatal("local application was not resolved")
	}
	if app.Image == "" {
		t.Fatal("local app image is empty")
	}
	if app.Healthcheck == nil || app.Healthcheck.HTTP == nil || app.Healthcheck.HTTP.Port != 8080 {
		t.Fatalf("local app health check is not configured for port 8080: %+v", app.Healthcheck)
	}
}
