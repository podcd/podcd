package reconciler

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/podcd/podcd/pkg/config"
	"github.com/podcd/podcd/pkg/renderer"
	"github.com/podcd/podcd/pkg/secrets"
	"github.com/podcd/podcd/pkg/state"
)

// TestValuesFilesTemplateApplicationsForThisHost is an end-to-end check that
// a host's own agent.yaml (repositories[].values) picks which values file
// templates the shared repository documents - the same document compiles to
// a different image tag depending only on which host's agent loaded it.
func TestValuesFilesTemplateApplicationsForThisHost(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repoDir := t.TempDir()
	files := map[string]string{
		"app.yaml.tpl": `
apiVersion: v1
kind: Pod
metadata: {name: web}
spec:
  containers:
    - name: web
      image: "{{ .Values.image.repository }}:{{ .Values.image.tag }}"
`,
		"host.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: vm-1}
spec: {applications: [web]}
`,
		"values/prod.yaml": "image:\n  repository: example.com/web\n  tag: \"2.0\"\n",
		"values/dev.yaml":  "image:\n  repository: example.com/web\n  tag: \"dev\"\n",
	}
	for name, content := range files {
		full := filepath.Join(repoDir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, repoDir, "init", "--quiet", "--initial-branch=main")
	runGit(t, repoDir, "config", "user.email", "t@e.x")
	runGit(t, repoDir, "config", "user.name", "t")
	runGit(t, repoDir, "add", "--all")
	runGit(t, repoDir, "commit", "--quiet", "-m", "initial")

	buildEngine := func(t *testing.T, valuesFile string) *Engine {
		t.Helper()
		cfg := config.DefaultAgentConfig()
		cfg.Host = "vm-1"
		cfg.StateDir = t.TempDir()
		cfg.UnitDir = filepath.Join(t.TempDir(), "units")
		cfg.Repositories = []config.RepositorySpec{{Name: "infra", URL: repoDir, Revision: "main", Values: []string{valuesFile}}}

		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		e := &Engine{
			cfg:   cfg,
			rt:    newFakeRuntime(),
			rend:  &renderer.Renderer{UnitDir: "/units", KubeDir: "/kube"},
			store: state.NewFileStore(cfg.StatePath()),
			log:   log,
		}
		e.ident.Host = "vm-1"
		repos, tokenRefs, valuesFiles := ReposFromConfig(cfg)
		e.source = &Source{Repos: repos, TokenRefs: tokenRefs, ValuesFiles: valuesFiles, Host: "vm-1", Secrets: secrets.Default("", ""), Log: log}
		return e
	}

	prod, err := buildEngine(t, "values/prod.yaml").Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if app, ok := prod.Desired.App("web"); !ok || app.Image != "example.com/web:2.0" {
		t.Fatalf("prod values should resolve to the 2.0 tag: %+v", app)
	}

	dev, err := buildEngine(t, "values/dev.yaml").Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if app, ok := dev.Desired.App("web"); !ok || app.Image != "example.com/web:dev" {
		t.Fatalf("dev values should resolve to the dev tag: %+v", app)
	}
}
