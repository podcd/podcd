// Package e2e runs the whole agent against a real rootless Podman and a real
// systemd user manager: Git in, container out. nginx:alpine is the test image:
// small, answers HTTP on 80, and serves whatever is mounted into it.
//
// It is skipped unless PODCD_E2E=1, because it writes real unit files into the
// current user's ~/.config/containers/systemd and starts real containers. Run
// it with `make test-e2e` on a host (or any Linux box with rootless podman).
package e2e

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/podcd/podcd/pkg/config"
	"github.com/podcd/podcd/pkg/model"
	"github.com/podcd/podcd/pkg/reconciler"
	"github.com/podcd/podcd/pkg/renderer"
)

const (
	appName   = "podcd-e2e-nginx"
	hostName  = "podcd-e2e-host"
	hostPort  = 18099
	testImage = "docker.io/library/nginx:alpine"
)

func TestReconcileEndToEnd(t *testing.T) {
	if os.Getenv("PODCD_E2E") != "1" {
		t.Skip("set PODCD_E2E=1 to run the end-to-end test (it starts real containers)")
	}
	requireTools(t)
	digest := imageDigest(t)

	ctx := context.Background()
	repoDir := t.TempDir()
	stateDir := t.TempDir()
	unitDir := userUnitDir(t)

	writeConfig(t, repoDir, digest, true)
	gitInit(t, repoDir)

	cfg := config.DefaultAgentConfig()
	cfg.Host = hostName
	cfg.StateDir = stateDir
	cfg.UnitDir = unitDir
	cfg.Repositories = []config.RepositorySpec{{Name: "infra", URL: repoDir, Revision: "main"}}
	cfg.Path = "(test)"

	t.Cleanup(func() { cleanup(unitDir) })

	engine, err := reconciler.NewEngine(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("building the engine: %v", err)
	}
	if ok, why := engine.Runtime().Available(ctx); !ok {
		t.Skipf("podman runtime is unusable here: %s", why)
	}

	// 1. A clean host converges: pull Git, identify the host, render Quadlet,
	//    start the container, health OK.
	res, err := engine.Reconcile(ctx, reconciler.Options{})
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if len(res.Applied) != 1 || res.Applied[0].Type != model.ActionCreate {
		t.Fatalf("want one create, got %+v", res.Applied)
	}
	if len(res.Health) != 1 || !res.Health[0].OK() {
		t.Fatalf("application is not healthy: %+v", res.Health)
	}
	if body := httpGet(t, "/"); !strings.Contains(body, "nginx") {
		t.Fatalf("the container is not answering as expected: %q", body)
	}

	// 2. Running it again changes nothing.
	for i := 0; i < 2; i++ {
		again, err := engine.Reconcile(ctx, reconciler.Options{})
		if err != nil {
			t.Fatalf("repeat reconcile %d: %v", i, err)
		}
		if len(again.Applied) != 0 {
			t.Fatalf("reconcile %d was not idempotent: %+v", i, again.Applied)
		}
		if !again.Plan.Empty() {
			t.Fatalf("reconcile %d still planned changes: %+v", i, again.Plan.Changes())
		}
	}

	// 3. Git changes; the agent notices, plans, applies, and the app is healthy.
	writeConfig(t, repoDir, digest, false) // flips an environment variable
	gitCommit(t, repoDir, "change the configuration")

	changed, err := engine.Reconcile(ctx, reconciler.Options{})
	if err != nil {
		t.Fatalf("reconcile after the Git change: %v", err)
	}
	if len(changed.Applied) != 1 || changed.Applied[0].Type != model.ActionUpdate {
		t.Fatalf("want one update, got %+v", changed.Applied)
	}
	if !changed.Health[0].OK() {
		t.Fatalf("unhealthy after the update: %+v", changed.Health)
	}
	if env := containerEnv(t); !strings.Contains(env, "FEATURE_X=off") {
		t.Fatalf("the change did not reach the running container: %s", env)
	}

	// 4. Drift is repaired: stop the unit by hand, the agent puts it back.
	runCmd(t, "systemctl", "--user", "stop", renderer.ServiceName(appName))
	repaired, err := engine.Reconcile(ctx, reconciler.Options{})
	if err != nil {
		t.Fatalf("reconcile after manual stop: %v", err)
	}
	if len(repaired.Applied) != 1 || repaired.Applied[0].Type != model.ActionRestart {
		t.Fatalf("want a restart, got %+v", repaired.Applied)
	}

	// 5. Boot survival: the unit is wired into default.target, which is what
	//    starts it after a reboot. Stopping it and starting the target is the
	//    same path systemd takes at boot.
	runCmd(t, "systemctl", "--user", "stop", renderer.ServiceName(appName))
	runCmd(t, "systemctl", "--user", "start", "default.target")
	waitForHealthy(t, 30*time.Second)

	// 6. Removing it from Git removes it from the host.
	writeEmptyHost(t, repoDir)
	gitCommit(t, repoDir, "stop running the application here")

	pruned, err := engine.Reconcile(ctx, reconciler.Options{})
	if err != nil {
		t.Fatalf("reconcile after removal: %v", err)
	}
	if len(pruned.Applied) != 1 || pruned.Applied[0].Type != model.ActionDelete {
		t.Fatalf("want a delete, got %+v", pruned.Applied)
	}
	if _, err := os.Stat(filepath.Join(unitDir, renderer.FileName(appName))); !os.IsNotExist(err) {
		t.Fatalf("the unit file is still there: %v", err)
	}
	if out := runCmdOut(t, "podman", "ps", "--all", "--filter", "name="+renderer.ContainerName(appName), "--format", "{{.Names}}"); strings.TrimSpace(out) != "" {
		t.Fatalf("the container is still there: %q", out)
	}
}

// TestUnmanagedUnitsAreLeftAlone proves podcd will not touch a Quadlet unit
// somebody else wrote, even one sitting in the same directory.
func TestUnmanagedUnitsAreLeftAlone(t *testing.T) {
	if os.Getenv("PODCD_E2E") != "1" {
		t.Skip("set PODCD_E2E=1 to run the end-to-end test")
	}
	requireTools(t)
	unitDir := userUnitDir(t)

	foreign := filepath.Join(unitDir, "podcd-e2e-foreign.container")
	if err := os.WriteFile(foreign, []byte("[Container]\nImage=docker.io/library/busybox\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(foreign) })

	ctx := context.Background()
	cfg := config.DefaultAgentConfig()
	cfg.Host = hostName
	cfg.StateDir = t.TempDir()
	cfg.UnitDir = unitDir
	repoDir := t.TempDir()
	writeEmptyHost(t, repoDir)
	gitInit(t, repoDir)
	cfg.Repositories = []config.RepositorySpec{{Name: "infra", URL: repoDir, Revision: "main"}}

	engine, err := reconciler.NewEngine(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	res, err := engine.Reconcile(ctx, reconciler.Options{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(res.Applied) != 0 {
		t.Fatalf("podcd touched something it does not own: %+v", res.Applied)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("the foreign unit was removed: %v", err)
	}
}

func requireTools(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"podman", "systemctl", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not installed", bin)
		}
	}
	if out, err := exec.Command("systemctl", "--user", "is-system-running").CombinedOutput(); err != nil &&
		!strings.Contains(string(out), "degraded") && !strings.Contains(string(out), "running") {
		t.Skipf("no systemd user manager here: %s", out)
	}
}

func imageDigest(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("podman", "image", "inspect", testImage, "--format", "{{index .RepoDigests 0}}").Output()
	if err != nil {
		t.Skipf("pull %s first (podman pull %s)", testImage, testImage)
	}
	return strings.TrimSpace(string(out))
}

func userUnitDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	dir := filepath.Join(home, ".config", "containers", "systemd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeConfig writes the test repository's YAML. featureOn flips one value so
// the test can prove a Git change reaches the running container.
func writeConfig(t *testing.T, dir, digest string, featureOn bool) {
	t.Helper()
	feature := "off"
	if featureOn {
		feature = "on"
	}
	write(t, filepath.Join(dir, "app.yaml"), fmt.Sprintf(`apiVersion: gitops.podcd.io/v1
kind: Application
metadata:
  name: %s
spec:
  image: %s
  ports:
    - host: %d
      container: 80
      hostIP: 127.0.0.1
  env:
    FEATURE_X: "%s"
  healthcheck:
    retries: 30
    interval: 1s
    http:
      port: %d
      path: /
---
apiVersion: gitops.podcd.io/v1
kind: Group
metadata:
  name: e2e
spec:
  applications: [%s]
---
apiVersion: gitops.podcd.io/v1
kind: Host
metadata:
  name: %s
spec:
  environment: e2e
  groups: [e2e]
---
apiVersion: gitops.podcd.io/v1
kind: Environment
metadata:
  name: e2e
spec: {}
`, appName, digest, hostPort, feature, hostPort, appName, hostName))
}

// writeEmptyHost leaves the host defined but running nothing.
func writeEmptyHost(t *testing.T, dir string) {
	t.Helper()
	for _, f := range []string{"app.yaml"} {
		os.Remove(filepath.Join(dir, f))
	}
	write(t, filepath.Join(dir, "host.yaml"), fmt.Sprintf(`apiVersion: gitops.podcd.io/v1
kind: Host
metadata:
  name: %s
spec: {}
`, hostName))
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	runIn(t, dir, "git", "init", "--quiet", "--initial-branch=main")
	runIn(t, dir, "git", "config", "user.email", "e2e@podcd.test")
	runIn(t, dir, "git", "config", "user.name", "podcd e2e")
	gitCommit(t, dir, "initial")
}

func gitCommit(t *testing.T, dir, message string) {
	t.Helper()
	runIn(t, dir, "git", "add", "--all")
	runIn(t, dir, "git", "commit", "--quiet", "-m", message)
}

func runIn(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func runCmd(t *testing.T, name string, args ...string) {
	t.Helper()
	// systemctl stop of a crashed container exits non-zero; the state that
	// matters is asserted afterwards, so failures here are not fatal.
	_ = exec.Command(name, args...).Run()
}

func runCmdOut(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, _ := exec.Command(name, args...).Output()
	return string(out)
}

func containerEnv(t *testing.T) string {
	t.Helper()
	return runCmdOut(t, "podman", "inspect", renderer.ContainerName(appName),
		"--format", "{{range .Config.Env}}{{println .}}{{end}}")
}

func httpGet(t *testing.T, path string) string {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s", hostPort, path))
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func waitForHealthy(t *testing.T, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", hostPort))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("the application did not come back within %s", within)
}

// cleanup removes anything this test left on the host, whether it passed or not.
func cleanup(unitDir string) {
	_ = exec.Command("systemctl", "--user", "stop", renderer.ServiceName(appName)).Run()
	_ = os.Remove(filepath.Join(unitDir, renderer.FileName(appName)))
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	_ = exec.Command("podman", "rm", "--force", "--time", "5", renderer.ContainerName(appName)).Run()
}
