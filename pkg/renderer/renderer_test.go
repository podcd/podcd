package renderer

import (
	"strings"
	"testing"

	"github.com/podcd/podcd/pkg/model"
)

func testApp() model.Application {
	return model.Application{
		Name:  "api",
		Image: "example.com/api@sha256:aaaa",
		Env:   map[string]string{"LOG_LEVEL": "info", "APP_ENV": "production"},
		Ports: []model.Port{
			{Host: 8080, Container: 8080, Protocol: "tcp"},
			{Host: 9090, Container: 9090, Protocol: "udp", HostIP: "127.0.0.1"},
		},
		Volumes:       []model.Volume{{Source: "/srv/data", Destination: "/data", Options: "ro"}},
		RestartPolicy: "always",
		Resources:     model.Resources{Memory: "512M", CPU: "150%"},
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	r := &Renderer{UnitDir: "/units", EnvDir: "/env"}
	app := testApp()

	first, err := r.Render(app)
	if err != nil {
		t.Fatal(err)
	}
	// Map iteration order must not leak into the output: if it did, every
	// reconcile would see a change and restart every application.
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
	r := &Renderer{UnitDir: "/units", EnvDir: "/env"}
	u, err := r.Render(testApp())
	if err != nil {
		t.Fatal(err)
	}
	got := string(u.Content)

	for _, want := range []string{
		"[Container]",
		"ContainerName=podcd-api",
		"Image=example.com/api@sha256:aaaa",
		"Label=io.podcd.managed=true",
		"Label=io.podcd.app=api",
		"Environment=APP_ENV=production",
		"Environment=LOG_LEVEL=info",
		"PublishPort=8080:8080",
		"PublishPort=127.0.0.1:9090:9090/udp",
		"Volume=/srv/data:/data:ro",
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
	if u.Path != "/units/podcd-api.container" {
		t.Errorf("unit path = %q", u.Path)
	}
	// Env ordering is alphabetical, not map order.
	if strings.Index(got, "APP_ENV") > strings.Index(got, "LOG_LEVEL") {
		t.Error("environment variables are not sorted")
	}
}

func TestMarkersRoundTrip(t *testing.T) {
	r := &Renderer{UnitDir: "/units", EnvDir: "/env"}
	app := testApp()
	app.SecretEnv = map[string]string{"TOKEN": "s3cret"}
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
	if m.SecretsHash != u.SecretsHash || m.SecretsHash == "" {
		t.Errorf("secrets hash marker = %q, want %q", m.SecretsHash, u.SecretsHash)
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
}

func TestSecretsStayOutOfTheUnitFile(t *testing.T) {
	r := &Renderer{UnitDir: "/units", EnvDir: "/env"}
	app := testApp()
	app.SecretEnv = map[string]string{"TOKEN": "s3cret-value"}
	u, err := r.Render(app)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(u.Content), "s3cret-value") {
		t.Fatal("a secret value leaked into the unit file")
	}
	if !strings.Contains(string(u.Content), "EnvironmentFile=/env/api.env") {
		t.Error("the unit does not reference the secret env file")
	}
	if !strings.Contains(string(u.EnvFile), `TOKEN="s3cret-value"`) {
		t.Errorf("env file = %q", u.EnvFile)
	}
}

func TestSecretRotationChangesTheUnit(t *testing.T) {
	r := &Renderer{UnitDir: "/units", EnvDir: "/env"}
	app := testApp()
	app.SecretEnv = map[string]string{"TOKEN": "old"}
	before, err := r.Render(app)
	if err != nil {
		t.Fatal(err)
	}
	app.SecretEnv = map[string]string{"TOKEN": "new"}
	after, err := r.Render(app)
	if err != nil {
		t.Fatal(err)
	}
	if before.SpecHash == after.SpecHash {
		t.Fatal("rotating a secret must change the spec hash, or the app never restarts")
	}
	if string(before.Content) == string(after.Content) {
		t.Fatal("rotating a secret must change the unit file")
	}
}

func TestValuesWithSpacesAreQuoted(t *testing.T) {
	r := &Renderer{UnitDir: "/units"}
	app := testApp()
	app.Env = map[string]string{"GREETING": "hello world"}
	app.Command = []string{"/bin/sh", "-c", "echo hi"}
	u, err := r.Render(app)
	if err != nil {
		t.Fatal(err)
	}
	got := string(u.Content)
	if !strings.Contains(got, `Environment="GREETING=hello world"`) {
		t.Errorf("value with a space was not quoted:\n%s", got)
	}
	if !strings.Contains(got, `Exec=/bin/sh -c "echo hi"`) {
		t.Errorf("command arguments were not quoted:\n%s", got)
	}
}

func TestNewlineInEnvIsRejected(t *testing.T) {
	r := &Renderer{UnitDir: "/units"}
	app := testApp()
	app.Env = map[string]string{"BAD": "one\ntwo"}
	if _, err := r.Render(app); err == nil {
		t.Fatal("a newline in a value must be refused, not silently truncated")
	}
}

func TestNonASCIIValuesSurvive(t *testing.T) {
	r := &Renderer{UnitDir: "/units"}
	app := testApp()
	app.Env = map[string]string{"CITY": "Köln am Rhein"}
	u, err := r.Render(app)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(u.Content), `Environment="CITY=Köln am Rhein"`) {
		t.Errorf("non-ASCII value was mangled:\n%s", u.Content)
	}
}
