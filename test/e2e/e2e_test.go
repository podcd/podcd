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
	cfg.Repository = config.RepositorySpec{Name: "infra", URL: repoDir, Revision: "main"}
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
	if _, err := os.Stat(filepath.Join(unitDir, renderer.KubeFileName(appName))); !os.IsNotExist(err) {
		t.Fatalf("the unit file is still there: %v", err)
	}
	if out := runCmdOut(t, "podman", "pod", "ps", "--filter", "name="+appName, "--format", "{{.Name}}"); strings.TrimSpace(out) != "" {
		t.Fatalf("the pod is still there: %q", out)
	}
	if out := runCmdOut(t, "podman", "ps", "--all", "--filter", "label="+config.LabelApp+"="+appName, "--format", "{{.Names}}"); strings.TrimSpace(out) != "" {
		t.Fatalf("the containers are still there: %q", out)
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

	// A .kube unit sharing podcd's own prefix: recognised by name, but with no
	// podcd header, so it is somebody else's and must be left where it is.
	foreign := filepath.Join(unitDir, "podcd-e2e-foreign.kube")
	if err := os.WriteFile(foreign, []byte("[Kube]\nYaml=/nonexistent/foreign.yaml\n"), 0o644); err != nil {
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
	cfg.Repository = config.RepositorySpec{Name: "infra", URL: repoDir, Revision: "main"}

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
	requireNoOtherWorkloads(t)
}

// requireNoOtherWorkloads fails, rather than skips, when this user already
// runs podcd workloads. The suite reconciles hosts that declare only their
// own applications, and anything else podcd manages here - a unit in the
// shared unit directory, a labelled container - is exactly what prune
// removes. Refusing is the only safe answer.
func requireNoOtherWorkloads(t *testing.T) {
	t.Helper()
	var others []string
	if home, err := os.UserHomeDir(); err == nil {
		entries, _ := os.ReadDir(filepath.Join(home, ".config", "containers", "systemd"))
		for _, e := range entries {
			if name := e.Name(); strings.HasPrefix(name, "podcd-") && !strings.HasPrefix(name, "podcd-e2e-") {
				others = append(others, name)
			}
		}
	}
	out, _ := exec.Command("podman", "ps", "--all", "--filter", "label="+config.LabelManaged+"=true", "--format", "{{.Names}}").Output()
	for _, name := range strings.Fields(string(out)) {
		if !strings.Contains(name, "podcd-e2e-") && !strings.HasSuffix(name, "-infra") {
			others = append(others, name)
		}
	}
	if len(others) > 0 {
		t.Fatalf("this user already runs podcd workloads (%s); the end-to-end suite would prune them - run it on a host with none", strings.Join(others, ", "))
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
	write(t, filepath.Join(dir, "app.yaml"), fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
spec:
  restartPolicy: Always
  containers:
    - name: nginx
      image: %s
      ports:
        - containerPort: 80
          hostPort: %d
          hostIP: 127.0.0.1
      env:
        - name: FEATURE_X
          value: "%s"
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
`, appName, digest, hostPort, feature, appName, hostName))
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

// appContainer returns the one container of an application's pod.
//
// podman kube play derives the container name from both the pod and the
// container, so the name is asked for rather than assumed; the infra container
// carries the same labels and is not what a caller means.
func appContainer(t *testing.T, app string) string {
	t.Helper()
	out, err := exec.Command("podman", "ps", "--filter", "pod="+app, "--format", "{{.Names}}").Output()
	if err != nil {
		t.Fatalf("listing the containers of pod %s: %v", app, err)
	}
	var names []string
	for _, n := range strings.Fields(string(out)) {
		if !strings.HasSuffix(n, "-infra") {
			names = append(names, n)
		}
	}
	if len(names) != 1 {
		t.Fatalf("want exactly one container in pod %s, got %v", app, names)
	}
	return names[0]
}

func containerEnv(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("podman", "inspect", appContainer(t, appName),
		"--format", "{{range .Config.Env}}{{println .}}{{end}}").Output()
	if err != nil {
		t.Fatalf("inspecting the container of %s: %v", appName, err)
	}
	return string(out)
}

func httpGet(t *testing.T, path string) string {
	t.Helper()
	return get(t, hostPort, path)
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
// The unit directory is the real one, shared with every other test in this
// package, so a unit left behind here fails the next test rather than this one.
func cleanup(unitDir string) {
	_ = exec.Command("systemctl", "--user", "stop", renderer.ServiceName(appName)).Run()
	_ = os.Remove(filepath.Join(unitDir, renderer.KubeFileName(appName)))
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	_ = exec.Command("podman", "pod", "rm", "--force", "--time", "5", appName).Run()
}
