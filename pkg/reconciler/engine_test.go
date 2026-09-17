package reconciler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/podcd/podcd/pkg/config"
	"github.com/podcd/podcd/pkg/model"
	"github.com/podcd/podcd/pkg/renderer"
	"github.com/podcd/podcd/pkg/secrets"
	"github.com/podcd/podcd/pkg/state"
)

// fakeRuntime stands in for podman so the engine's own behaviour - ordering,
// error handling, what gets recorded - can be tested without containers.
// The mutex matters only for the loop test, where the engine runs on its own
// goroutine while the test watches what it does.
type fakeRuntime struct {
	mu   sync.Mutex
	apps map[string]model.ActualApp

	applied  []string
	removed  []string
	restarts []string

	applyErr  error
	removeErr map[string]error
	unhealthy map[string]bool
	healthErr error
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{apps: map[string]model.ActualApp{}, unhealthy: map[string]bool{}}
}

func (f *fakeRuntime) Name() string                             { return "fake" }
func (f *fakeRuntime) Available(context.Context) (bool, string) { return true, "" }

func (f *fakeRuntime) Inspect(context.Context) (model.ActualState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	apps := make(map[string]model.ActualApp, len(f.apps))
	for k, v := range f.apps {
		apps[k] = v
	}
	return model.ActualState{Runtime: "fake", Apps: apps}, nil
}

func (f *fakeRuntime) Apply(_ context.Context, app model.Application) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.applyErr != nil {
		return f.applyErr
	}
	f.applied = append(f.applied, app.Name)
	r := &renderer.Renderer{UnitDir: "/units", KubeDir: "/kube"}
	u, err := r.Render(app)
	if err != nil {
		return err
	}
	f.apps[app.Name] = model.ActualApp{
		Name: app.Name, Managed: true, UnitFile: u.Path,
		UnitFileHash: model.HashBytes(u.Content), UnitContent: u.Content,
		SpecHash: u.SpecHash, UnitState: model.UnitActive,
	}
	return nil
}

func (f *fakeRuntime) Remove(_ context.Context, app string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.removeErr[app]; err != nil {
		return err
	}
	f.removed = append(f.removed, app)
	delete(f.apps, app)
	return nil
}

func (f *fakeRuntime) Restart(_ context.Context, app string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarts = append(f.restarts, app)
	cur := f.apps[app]
	cur.UnitState = model.UnitActive
	f.apps[app] = cur
	return nil
}

func (f *fakeRuntime) Health(_ context.Context, app model.Application) (model.Health, error) {
	if f.healthErr != nil {
		return model.Health{}, f.healthErr
	}
	if f.unhealthy[app.Name] {
		return model.Health{App: app.Name, Status: model.HealthUnhealthy, Message: "it is broken"}, nil
	}
	return model.Health{App: app.Name, Status: model.HealthHealthy}, nil
}

func (f *fakeRuntime) Logs(_ context.Context, app string, lines int) (string, error) {
	return fmt.Sprintf("%s:%d", app, lines), nil
}

// appliedCount is safe to call while the engine is running on another goroutine.
func (f *fakeRuntime) appliedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.applied)
}

// newTestEngine wires an engine around the fake runtime and a real local Git
// repository, so the whole path from a commit to an applied change is exercised.
func newTestEngine(t *testing.T, rt *fakeRuntime, docs string) (*Engine, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repoDir := t.TempDir()
	writeRepo(t, repoDir, docs)

	cfg := config.DefaultAgentConfig()
	cfg.Host = "vm-1"
	cfg.StateDir = t.TempDir()
	cfg.UnitDir = filepath.Join(t.TempDir(), "units")
	cfg.Repositories = []config.RepositorySpec{{Name: "infra", URL: repoDir, Revision: "main"}}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e := &Engine{
		cfg:   cfg,
		rt:    rt,
		rend:  &renderer.Renderer{UnitDir: "/units", KubeDir: "/kube"},
		store: state.NewFileStore(cfg.StatePath()),
		log:   log,
	}
	e.ident.Host = "vm-1"
	repos, tokenRefs, valuesFiles := ReposFromConfig(cfg)
	e.source = &Source{Repos: repos, TokenRefs: tokenRefs, ValuesFiles: valuesFiles, Host: "vm-1", Secrets: secrets.Default("", ""), Log: log}
	return e, repoDir
}

func writeRepo(t *testing.T, dir, docs string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(docs), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); os.IsNotExist(err) {
		runGit(t, dir, "init", "--quiet", "--initial-branch=main")
		runGit(t, dir, "config", "user.email", "t@e.x")
		runGit(t, dir, "config", "user.name", "t")
	}
	runGit(t, dir, "add", "--all")
	runGit(t, dir, "commit", "--quiet", "-m", "update")
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

const twoApps = `apiVersion: v1
kind: Pod
metadata:
  name: api
spec:
  containers:
    - name: api
      image: example.com/api@sha256:aaaa
---
apiVersion: v1
kind: Pod
metadata:
  name: web
spec:
  containers:
    - name: web
      image: example.com/web@sha256:bbbb
---
apiVersion: gitops.podcd.io/v1
kind: Host
metadata:
  name: vm-1
spec:
  applications: [api, web]
`

const oneApp = `apiVersion: v1
kind: Pod
metadata:
  name: api
spec:
  containers:
    - name: api
      image: example.com/api@sha256:aaaa
---
apiVersion: gitops.podcd.io/v1
kind: Host
metadata:
  name: vm-1
spec:
  applications: [api]
`

const appWithUnavailableExternalSecret = `apiVersion: external-secrets.io/v1beta1
kind: SecretStore
metadata: {name: host-env}
spec:
  provider:
    env: {}
---
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata: {name: unavailable}
spec:
  secretStoreRef: {name: host-env}
  target: {name: unavailable}
  data:
    - secretKey: token
      remoteRef: {key: PODCD_MISSING_EXTERNAL_SECRET}
---
apiVersion: v1
kind: Pod
metadata: {name: needs-secret}
spec:
  containers:
    - name: app
      image: example.com/needs-secret@sha256:aaaa
      envFrom:
        - secretRef: {name: unavailable}
---
apiVersion: v1
kind: Pod
metadata: {name: independent}
spec:
  containers:
    - name: app
      image: example.com/independent@sha256:bbbb
---
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: vm-1}
spec:
  applications: [needs-secret, independent]
`

func TestReconcileAppliesAndRecords(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, twoApps)

	res, err := e.Reconcile(context.Background(), Options{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if strings.Join(rt.applied, ",") != "api,web" {
		t.Fatalf("applied %v, want api then web", rt.applied)
	}
	if len(res.Health) != 2 {
		t.Fatalf("every application should be probed, got %d", len(res.Health))
	}

	st, err := e.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.LastSuccess == nil || st.LastFailure != nil {
		t.Fatalf("state should record one success and no failure: %+v", st)
	}
	if st.FailureCount != 0 {
		t.Errorf("failure count = %d", st.FailureCount)
	}
	if st.Applications["api"].Image != "example.com/api@sha256:aaaa" {
		t.Errorf("application record: %+v", st.Applications["api"])
	}
	if st.Applications["api"].Health != "healthy" {
		t.Errorf("health was not recorded: %+v", st.Applications["api"])
	}

	// Second run: nothing to do.
	again, err := e.Reconcile(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Applied) != 0 {
		t.Fatalf("the second reconcile was not a no-op: %+v", again.Applied)
	}
}

func TestReconcileAppliesIndependentAppsWhenSecretProvisioningFails(t *testing.T) {
	rt := newFakeRuntime()
	// A failed provision must not turn this existing app into a prune target.
	rt.apps["needs-secret"] = model.ActualApp{Name: "needs-secret", Managed: true}
	e, _ := newTestEngine(t, rt, appWithUnavailableExternalSecret)

	res, err := e.Reconcile(context.Background(), Options{SkipHealth: true})
	if err == nil || !strings.Contains(err.Error(), "needs-secret") {
		t.Fatalf("want the provisioning error for needs-secret, got %v", err)
	}
	if len(rt.applied) != 1 || rt.applied[0] != "independent" {
		t.Fatalf("applied = %v, want only independent", rt.applied)
	}
	if len(rt.removed) != 0 {
		t.Fatalf("a blocked app must not be pruned, removed = %v", rt.removed)
	}
	for _, action := range res.Plan.Actions {
		if action.App == "needs-secret" && action.Type == model.ActionDelete {
			t.Fatalf("blocked app was scheduled for deletion: %+v", action)
		}
	}
}

func TestReconcilePrunesWhatGitDropped(t *testing.T) {
	rt := newFakeRuntime()
	e, repoDir := newTestEngine(t, rt, twoApps)
	if _, err := e.Reconcile(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}

	writeRepo(t, repoDir, oneApp)
	res, err := e.Reconcile(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rt.removed, ",") != "web" {
		t.Fatalf("removed %v, want web", rt.removed)
	}
	if len(res.Applied) != 1 || res.Applied[0].Type != model.ActionDelete {
		t.Fatalf("want one delete, got %+v", res.Applied)
	}

	st, _ := e.store.Load()
	if _, ok := st.Applications["web"]; ok {
		t.Error("the removed application is still in the local state")
	}
}

func TestPruneCanBeDisabledPerRun(t *testing.T) {
	rt := newFakeRuntime()
	e, repoDir := newTestEngine(t, rt, twoApps)
	if _, err := e.Reconcile(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}

	writeRepo(t, repoDir, oneApp)
	off := false
	res, err := e.Reconcile(context.Background(), Options{Prune: &off})
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.removed) != 0 {
		t.Fatalf("nothing should have been removed, got %v", rt.removed)
	}
	// The orphan is still reported, so it cannot be forgotten about.
	found := false
	for _, a := range res.Plan.Actions {
		if a.App == "web" && strings.Contains(a.Reason, "pruning is disabled") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the orphan was not reported: %+v", res.Plan.Actions)
	}
}

func TestUnhealthyApplicationFailsTheReconcile(t *testing.T) {
	rt := newFakeRuntime()
	rt.unhealthy["web"] = true
	e, _ := newTestEngine(t, rt, twoApps)

	_, err := e.Reconcile(context.Background(), Options{})
	if err == nil || !strings.Contains(err.Error(), "unhealthy") {
		t.Fatalf("an application that does not come up must fail the reconcile, got: %v", err)
	}

	st, _ := e.store.Load()
	if st.LastFailure == nil {
		t.Fatal("the failure was not recorded")
	}
	if st.FailureCount != 1 {
		t.Errorf("failure count = %d, want 1", st.FailureCount)
	}
	if st.Applications["web"].Health != "unhealthy" {
		t.Errorf("the unhealthy result was not recorded: %+v", st.Applications["web"])
	}
}

func TestApplyFailureIsRecordedAndStopsTheRun(t *testing.T) {
	rt := newFakeRuntime()
	rt.applyErr = errors.New("image pull failed")
	e, _ := newTestEngine(t, rt, twoApps)

	res, err := e.Reconcile(context.Background(), Options{})
	if err == nil || !strings.Contains(err.Error(), "image pull failed") {
		t.Fatalf("want the underlying failure, got: %v", err)
	}
	if len(res.Applied) != 0 {
		t.Fatalf("nothing succeeded, so nothing should be reported as applied: %+v", res.Applied)
	}
	st, _ := e.store.Load()
	if st.LastFailure == nil || !strings.Contains(st.LastFailure.Error, "image pull failed") {
		t.Fatalf("the failure was not recorded: %+v", st.LastFailure)
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, twoApps)

	res, err := e.Reconcile(context.Background(), Options{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Plan.Changes()) != 2 {
		t.Fatalf("the plan should still describe the work: %+v", res.Plan.Actions)
	}
	if len(rt.applied) != 0 || len(rt.removed) != 0 {
		t.Fatalf("a dry run touched the host: applied=%v removed=%v", rt.applied, rt.removed)
	}
}

func TestReconcileRefusesToRunTwiceAtOnce(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, twoApps)

	lock, err := Acquire(e.cfg.LockPath())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	if _, err := e.Reconcile(context.Background(), Options{}); err == nil ||
		!strings.Contains(err.Error(), "another reconcile") {
		t.Fatalf("a second reconcile must refuse to start, got: %v", err)
	}
}

func TestBrokenConfigDoesNotTouchTheHost(t *testing.T) {
	rt := newFakeRuntime()
	e, repoDir := newTestEngine(t, rt, twoApps)
	if _, err := e.Reconcile(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	before := len(rt.applied)

	// A misspelled field: the whole commit is rejected, and the host is left
	writeRepo(t, repoDir, strings.Replace(twoApps, "image:", "imagee:", 1))
	if _, err := e.Reconcile(context.Background(), Options{}); err == nil {
		t.Fatal("a bad commit must fail the reconcile")
	}
	if len(rt.applied) != before || len(rt.removed) != 0 {
		t.Fatalf("a bad commit changed the host: applied=%v removed=%v", rt.applied, rt.removed)
	}
}
