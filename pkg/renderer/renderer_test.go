package renderer

import (
	"strings"
	"testing"

	"github.com/podcd/podcd/pkg/model"
)

func testApp() model.Application {
	app := model.Application{
		Name:          "api",
		Kind:          model.KindKube,
		RestartPolicy: "always",
		Resources:     model.Resources{Memory: "512M", CPU: "150%"},
		Networks:      []string{"podman"},
	}
	app.SetManifest([]byte("apiVersion: v1\nkind: Pod\nmetadata:\n  name: api\nspec:\n  containers:\n    - name: api\n      image: example.com/api@sha256:aaaa\n"))
	return app
}

func testRenderer() *Renderer {
	return &Renderer{UnitDir: "/units", KubeDir: "/kube"}
}

func TestRenderIsDeterministic(t *testing.T) {
	r := testRenderer()
	app := testApp()

	first, err := r.Render(app)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		again, err := r.Render(app)
		if err != nil {
			t.Fatal(err)
		}
		if string(again.Content) != string(first.Content) {
			t.Fatalf("render is not deterministic:\n%s\n---\n%s", first.Content, again.Content)
		}
	}
}

func TestRenderedUnitContents(t *testing.T) {
	r := testRenderer()
	u, err := r.Render(testApp())
	if err != nil {
		t.Fatal(err)
	}
	got := string(u.Content)

	for _, want := range []string{
		"[Kube]",
		"Yaml=/kube/api.yaml",
		"Network=podman",
		"Restart=always",
		"MemoryMax=512M",
		"CPUQuota=150%",
		"[Install]",
		"WantedBy=default.target",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered unit is missing %q:\n%s", want, got)
		}
	}
	if u.ServiceName != "podcd-api.service" {
		t.Errorf("service name = %q", u.ServiceName)
	}
	if u.Path != "/units/podcd-api.kube" {
		t.Errorf("unit path = %q", u.Path)
	}
}

func TestMarkersRoundTrip(t *testing.T) {
	r := testRenderer()
	app := testApp()
	u, err := r.Render(app)
	if err != nil {
		t.Fatal(err)
	}
	m := ParseMarkers(u.Content)
	if !m.Managed {
		t.Error("managed marker was not written")
	}
	if m.App != "api" {
		t.Errorf("app marker = %q", m.App)
	}
	if m.SpecHash != u.SpecHash || m.SpecHash == "" {
		t.Errorf("spec hash marker = %q, want %q", m.SpecHash, u.SpecHash)
	}
	if m.Kind != model.KindKube {
		t.Errorf("kind marker = %q", m.Kind)
	}
	if m.Version != Version {
		t.Errorf("renderer version marker = %q", m.Version)
	}
}

func TestForeignUnitIsNotClaimed(t *testing.T) {
	m := ParseMarkers([]byte("[Container]\nImage=whatever\n"))
	if m.Managed {
		t.Error("a unit without podcd's header must never be treated as managed")
	}
	if _, ok := AppFromFileName("someone-elses.container"); ok {
		t.Error("a file without the podcd- prefix must not be claimed")
	}
	if app, ok := AppFromFileName("podcd-api.container"); !ok || app != "api" {
		t.Errorf("AppFromFileName(podcd-api.container) = %q, %v", app, ok)
	}
	if app, ok := AppFromFileName("podcd-api.kube"); !ok || app != "api" {
		t.Errorf("AppFromFileName(podcd-api.kube) = %q, %v", app, ok)
	}
}
