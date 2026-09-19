package e2e

// The docker runtime, against a real Docker daemon with Compose v2: the same
// Git in, container out story as the podman suite, read back through docker
// instead of podman and systemd. Run it with `make test-e2e-docker`, which
// uses the daemon on this machine when there is one and otherwise starts
// one inside a privileged container (scripts/e2e-docker.sh).

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/podcd/podcd/pkg/config"
	"github.com/podcd/podcd/pkg/model"
	"github.com/podcd/podcd/pkg/reconciler"
	"github.com/podcd/podcd/pkg/renderer"
)

const (
	dockerHost = "podcd-e2e-docker-host"
	dockerApp  = "podcd-e2e-docker-nginx"
	dockerPort = 18098
)

func TestDockerReconcileEndToEnd(t *testing.T) {
	requireDocker(t)
	digest := dockerImageDigest(t)

	ctx := context.Background()
	repoDir := t.TempDir()
	stateDir := dockerStateDir(t)
	writeDockerConfig(t, repoDir, digest, true)
	gitInit(t, repoDir)
	t.Cleanup(func() { cleanupDocker(dockerApp) })

	engine := dockerEngine(t, dockerHost, repoDir, stateDir)

	// 1. A clean host converges: pull Git, render the compose project, bring
	//    it up, health OK, and the port is published on the pod's infra.
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
	if body := get(t, dockerPort, "/"); !strings.Contains(body, "nginx") {
		t.Fatalf("the container is not answering as expected: %q", body)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "docker", renderer.KubeFileName(dockerApp))); err != nil {
		t.Fatalf("the record was not written under the state directory: %v", err)
	}
	if names := dockerNames(t, dockerApp); names != dockerApp+"-infra "+dockerApp+"-nginx" {
		t.Fatalf("want an infra and a workload container, got %q", names)
	}

	// 2. Running it again changes nothing.
	for i := 0; i < 2; i++ {
		again, err := engine.Reconcile(ctx, reconciler.Options{})
		if err != nil {
			t.Fatalf("repeat reconcile %d: %v", i, err)
		}
		if len(again.Applied) != 0 || !again.Plan.Empty() {
			t.Fatalf("reconcile %d was not idempotent: %+v", i, again.Plan.Changes())
		}
	}

	// 3. Git changes; the agent notices, plans, applies, and the change is
	//    in the running container.
	writeDockerConfig(t, repoDir, digest, false)
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
	if env := runCmdOut(t, "docker", "inspect", dockerApp+"-nginx", "--format", "{{range .Config.Env}}{{println .}}{{end}}"); !strings.Contains(env, "FEATURE_X=off") {
		t.Fatalf("the change did not reach the running container: %s", env)
	}

	// 4. Drift is repaired. A stopped workload container is seen as dead and
	//    the pod replayed; a project removed by hand is seen as inactive and
	//    brought back from the manifest on disk.
	runCmd(t, "docker", "stop", dockerApp+"-nginx")
	repaired, err := engine.Reconcile(ctx, reconciler.Options{})
	if err != nil {
		t.Fatalf("reconcile after manual stop: %v", err)
	}
	if len(repaired.Applied) != 1 || repaired.Applied[0].Type != model.ActionRestart {
		t.Fatalf("want a restart, got %+v", repaired.Applied)
	}
	runCmd(t, "docker", "compose", "--project-name", "podcd-"+dockerApp, "down")
	if names := dockerNames(t, dockerApp); names != "" {
		t.Fatalf("down should have removed the containers, got %q", names)
	}
	replayed, err := engine.Reconcile(ctx, reconciler.Options{})
	if err != nil {
		t.Fatalf("reconcile after down: %v", err)
	}
	if len(replayed.Applied) != 1 || replayed.Applied[0].Type != model.ActionRestart || !replayed.Health[0].OK() {
		t.Fatalf("want a restart back to healthy, got %+v %+v", replayed.Applied, replayed.Health)
	}
	if body := get(t, dockerPort, "/"); !strings.Contains(body, "nginx") {
		t.Fatalf("not answering after the replay: %q", body)
	}

	// 5. Removing it from Git removes it from the host: containers, the
	//    project's own network, the record and the manifest. Never volumes.
	writeEmptyDockerHost(t, repoDir)
	gitCommit(t, repoDir, "stop running the application here")
	pruned, err := engine.Reconcile(ctx, reconciler.Options{})
	if err != nil {
		t.Fatalf("reconcile after removal: %v", err)
	}
	if len(pruned.Applied) != 1 || pruned.Applied[0].Type != model.ActionDelete {
		t.Fatalf("want a delete, got %+v", pruned.Applied)
	}
	if names := dockerNames(t, dockerApp); names != "" {
		t.Fatalf("the containers are still there: %q", names)
	}
	if out := runCmdOut(t, "docker", "network", "ls", "--filter", "name=podcd-"+dockerApp, "--format", "{{.Name}}"); strings.TrimSpace(out) != "" {
		t.Fatalf("the project network is still there: %q", out)
	}
	for _, p := range []string{
		filepath.Join(stateDir, "docker", renderer.KubeFileName(dockerApp)),
		filepath.Join(stateDir, "docker", dockerApp),
		filepath.Join(stateDir, "kube", dockerApp+".yaml"),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s is still there", p)
		}
	}
}

// TestDockerNetworkEndToEnd is the Network story on docker: the same
// repository as the podman network test, with the pods finding each other
// by pod name through the alias the infra container carries.
func TestDockerNetworkEndToEnd(t *testing.T) {
	requireDocker(t)
	digest := dockerImageDigest(t)
	t.Setenv("PODCD_E2E_NET_TOKEN", "net-secret")

	ctx := context.Background()
	repoDir := t.TempDir()
	stateDir := dockerStateDir(t)
	t.Cleanup(func() {
		cleanupDocker(clientName, serverName)
		_ = exec.Command("docker", "network", "rm", netName).Run()
	})
	writeNetworkConfig(t, repoDir, digest, "10.97.0.0/24", true)
	gitInit(t, repoDir)

	engine := dockerEngine(t, netHost, repoDir, stateDir)

	// 1. Network first, then the two pods; the pods find each other by name.
	res, err := engine.Reconcile(ctx, reconciler.Options{})
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if got := appliedSummary(res); got != "create network e2e-net, create podcd-e2e-client, create podcd-e2e-server" {
		t.Fatalf("got %q", got)
	}
	for _, h := range res.Health {
		if !h.OK() {
			t.Fatalf("not healthy: %+v", res.Health)
		}
	}
	if subnet := dockerNetworkSubnet(t); subnet != "10.97.0.0/24" {
		t.Fatalf("docker has the network with subnet %q, want 10.97.0.0/24", subnet)
	}
	if labels := runCmdOut(t, "docker", "network", "inspect", netName, "--format", "{{.Labels}}"); !strings.Contains(labels, "io.podcd.network:"+netName) {
		t.Fatalf("the network is not labelled as podcd's: %s", labels)
	}
	if body := dockerCrossPodGet(t, "/"); !strings.Contains(body, "on the network") {
		t.Fatalf("the client could not reach the server by pod name over the managed network: %q", body)
	}
	if body := dockerCrossPodGet(t, "/secret/token"); strings.TrimSpace(body) != "net-secret" {
		t.Fatalf("the templated ExternalSecret did not reach the server: %q", body)
	}

	// 2. Idempotent, network included.
	again, err := engine.Reconcile(ctx, reconciler.Options{})
	if err != nil || len(again.Applied) != 0 {
		t.Fatalf("second reconcile should change nothing: %v %+v", err, again.Applied)
	}

	// 3. A new subnet: the network is recreated under the pods, which are
	//    restarted onto it.
	writeNetworkConfig(t, repoDir, digest, "10.98.0.0/24", true)
	gitCommit(t, repoDir, "move the network")
	moved, err := engine.Reconcile(ctx, reconciler.Options{})
	if err != nil {
		t.Fatalf("reconcile after the subnet change: %v", err)
	}
	if got := appliedSummary(moved); got != "update network e2e-net, restart podcd-e2e-client, restart podcd-e2e-server" {
		t.Fatalf("got %q", got)
	}
	if subnet := dockerNetworkSubnet(t); subnet != "10.98.0.0/24" {
		t.Fatalf("network still has subnet %q after the change", subnet)
	}
	if body := dockerCrossPodGet(t, "/"); !strings.Contains(body, "on the network") {
		t.Fatalf("after the recreate the pods no longer reach each other: %q", body)
	}
	for _, h := range moved.Health {
		if !h.OK() {
			t.Fatalf("not healthy after the recreate: %+v", moved.Health)
		}
	}

	// 4. The client leaves; the network stays for the server.
	writeNetworkConfig(t, repoDir, digest, "10.98.0.0/24", false)
	gitCommit(t, repoDir, "drop the client")
	one, err := engine.Reconcile(ctx, reconciler.Options{})
	if err != nil {
		t.Fatalf("reconcile after dropping the client: %v", err)
	}
	if got := appliedSummary(one); got != "delete podcd-e2e-client" {
		t.Fatalf("got %q", got)
	}
	if !dockerNetworkExists() {
		t.Fatal("the network must stay while a pod is on it")
	}

	// 5. The last pod leaves; the network goes after it.
	write(t, filepath.Join(repoDir, "host.yaml"), fmt.Sprintf("apiVersion: gitops.podcd.io/v1\nkind: Host\nmetadata:\n  name: %s\nspec: {}\n", netHost))
	gitCommit(t, repoDir, "drop the server")
	gone, err := engine.Reconcile(ctx, reconciler.Options{})
	if err != nil {
		t.Fatalf("reconcile after dropping the server: %v", err)
	}
	if got := appliedSummary(gone); got != "delete podcd-e2e-server, delete network e2e-net" {
		t.Fatalf("got %q", got)
	}
	if dockerNetworkExists() {
		t.Fatal("the network is still there")
	}
	after, err := engine.Runtime().Inspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Networks) != 0 || len(after.Apps) != 0 {
		t.Fatalf("Inspect still reports something: %+v %+v", after.Apps, after.Networks)
	}
}

// requireDocker skips unless PODCD_E2E is set and a real Docker daemon with
// Compose v2 answers. podman's docker shim is not that daemon.
func requireDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("PODCD_E2E") != "1" {
		t.Skip("set PODCD_E2E=1 to run the end-to-end test (it starts real containers)")
	}
	for _, bin := range []string{"docker", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not installed", bin)
		}
	}
	out, err := exec.Command("docker", "version").CombinedOutput()
	if err != nil {
		t.Skipf("no docker daemon here: %s", out)
	}
	if strings.Contains(strings.ToLower(string(out)), "podman") {
		t.Skip("docker here is podman's shim, not a Docker daemon; run make test-e2e-docker")
	}
	// Compose v1 (the python docker-compose) cannot wait for a service to complete;
	// anything from v2 on can.
	if out, err := exec.Command("docker", "compose", "version", "--short").CombinedOutput(); err != nil || strings.HasPrefix(strings.TrimSpace(string(out)), "1.") {
		t.Skipf("docker compose v2 or later is not installed: %s", out)
	}
	// Anything else podcd manages on this daemon is what prune would remove.
	list, _ := exec.Command("docker", "ps", "--all", "--filter", "label="+config.LabelManaged+"=true", "--format", "{{.Names}}").Output()
	var others []string
	for _, name := range strings.Fields(string(list)) {
		if !strings.Contains(name, "podcd-e2e-") {
			others = append(others, name)
		}
	}
	if len(others) > 0 {
		t.Fatalf("this daemon already runs podcd workloads (%s); the end-to-end suite would prune them", strings.Join(others, ", "))
	}
}

// dockerStateDir is a state directory the containers' own users can reach:
// mounted ConfigMaps and Secrets live under it, and t.TempDir is 0700.
func dockerStateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "podcd-e2e-docker-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// dockerEngine wires an engine on the docker runtime. The unit directory is
// a throwaway: the docker runtime keeps its records under the state directory.
func dockerEngine(t *testing.T, host, repoDir, stateDir string) *reconciler.Engine {
	t.Helper()
	cfg := config.DefaultAgentConfig()
	cfg.Runtime = "docker"
	cfg.Host = host
	cfg.StateDir = stateDir
	cfg.UnitDir = t.TempDir()
	cfg.Repository = config.RepositorySpec{Name: "infra", URL: repoDir, Revision: "main"}
	cfg.Path = "(test)"
	engine, err := reconciler.NewEngine(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("building the engine: %v", err)
	}
	if ok, why := engine.Runtime().Available(context.Background()); !ok {
		t.Skipf("docker runtime is unusable here: %s", why)
	}
	return engine
}

// dockerImageDigest pulls the test image and returns it pinned by digest,
// the way the podman suite pins it.
func dockerImageDigest(t *testing.T) string {
	t.Helper()
	if out, err := exec.Command("docker", "pull", "--quiet", testImage).CombinedOutput(); err != nil {
		t.Fatalf("docker pull %s: %v\n%s", testImage, err, out)
	}
	out, err := exec.Command("docker", "image", "inspect", testImage, "--format", "{{index .RepoDigests 0}}").Output()
	if err != nil {
		t.Fatalf("inspecting %s: %v", testImage, err)
	}
	return strings.TrimSpace(string(out))
}

// writeDockerConfig is the podman suite's repository, for the docker host
// and port.
func writeDockerConfig(t *testing.T, dir, digest string, featureOn bool) {
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
kind: Host
metadata:
  name: %s
spec:
  applications: [%s]
`, dockerApp, digest, dockerPort, feature, dockerHost, dockerApp))
}

func writeEmptyDockerHost(t *testing.T, dir string) {
	t.Helper()
	os.Remove(filepath.Join(dir, "app.yaml"))
	write(t, filepath.Join(dir, "host.yaml"), fmt.Sprintf("apiVersion: gitops.podcd.io/v1\nkind: Host\nmetadata:\n  name: %s\nspec: {}\n", dockerHost))
}

// dockerNames lists an application's containers by name, sorted, infra
// included, as one space-separated string.
func dockerNames(t *testing.T, app string) string {
	t.Helper()
	out := runCmdOut(t, "docker", "ps", "--all", "--filter", "label="+config.LabelApp+"="+app, "--format", "{{.Names}}")
	names := strings.Fields(out)
	slices.Sort(names)
	return strings.Join(names, " ")
}

// dockerCrossPodGet fetches a path from the server, from inside the client
// pod, by the server's pod name.
func dockerCrossPodGet(t *testing.T, path string) string {
	t.Helper()
	var out string
	var err error
	for i := 0; i < 20; i++ {
		var b []byte
		b, err = exec.Command("docker", "exec", clientName+"-client", "wget", "-qO-", "-T", "3", "http://"+serverName+path).CombinedOutput()
		out = string(b)
		if err == nil {
			return out
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("wget from the client pod: %v\n%s", err, out)
	return ""
}

func dockerNetworkExists() bool {
	return exec.Command("docker", "network", "inspect", netName).Run() == nil
}

func dockerNetworkSubnet(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(runCmdOut(t, "docker", "network", "inspect", netName, "--format", "{{(index .IPAM.Config 0).Subnet}}"))
}

// cleanupDocker removes what a test leaves behind if it fails half way:
// the applications' containers by label, and the projects' own networks.
func cleanupDocker(apps ...string) {
	for _, app := range apps {
		out, _ := exec.Command("docker", "ps", "--all", "--quiet", "--filter", "label="+config.LabelApp+"="+app).Output()
		if ids := strings.Fields(string(out)); len(ids) > 0 {
			_ = exec.Command("docker", append([]string{"rm", "--force"}, ids...)...).Run()
		}
		_ = exec.Command("docker", "network", "rm", "podcd-"+app+"_default").Run()
	}
}

// --- the failure matrix, on docker ---------------------------------------
//
// The verdicts the podman matrix pins down, read back through docker: a
// probe's log and failing streak come from `docker inspect`, init containers
// stay around exited (compose does not remove them), and a failed one stops
// `up` before the workload starts.

const dockerMatrixHost = "podcd-e2e-docker-matrix"

func dockerMatrixEngine(t *testing.T, name, manifest string) *reconciler.Engine {
	t.Helper()
	requireDocker(t)
	repoDir := t.TempDir()
	write(t, filepath.Join(repoDir, "app.yaml"), manifest+fmt.Sprintf(`---
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: %s}
spec: {applications: [%s]}
`, dockerMatrixHost, name))
	gitInit(t, repoDir)
	t.Cleanup(func() { cleanupDocker(name) })
	return dockerEngine(t, dockerMatrixHost, repoDir, dockerStateDir(t))
}

func TestDockerMatrixFailingLivenessProbeIsExplained(t *testing.T) {
	requireDocker(t)
	digest := dockerImageDigest(t)
	const name = "podcd-e2e-docker-probe"
	e := dockerMatrixEngine(t, name, pod(name, digest, `  restartPolicy: Always
  containers:
    - name: app
      image: IMAGE
      livenessProbe:
        exec:
          command: ["sh", "-c", "echo probe says no; exit 1"]
        periodSeconds: 1
        failureThreshold: 2
`))
	_, _ = e.Reconcile(context.Background(), reconciler.Options{})
	deadline := time.Now().Add(40 * time.Second)
	var h model.Health
	for time.Now().Before(deadline) {
		hs, err := e.Health(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		h = hs[0]
		if strings.Contains(h.Message, "probe says no") {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if h.Status == model.HealthHealthy {
		t.Fatalf("a failing liveness probe must never read as healthy: %+v", h)
	}
	for _, want := range []string{"healthcheck failing: " + name + "-app", "check failure", "last check: probe says no"} {
		if !strings.Contains(h.Message, want) {
			t.Errorf("message should say %q: %s", want, h.Message)
		}
	}
}

func TestDockerMatrixPassingLivenessProbeBecomesHealthy(t *testing.T) {
	requireDocker(t)
	digest := dockerImageDigest(t)
	const name = "podcd-e2e-docker-probe-ok"
	e := dockerMatrixEngine(t, name, pod(name, digest, `  containers:
    - name: app
      image: IMAGE
      livenessProbe:
        exec:
          command: ["true"]
        periodSeconds: 1
`))
	res, err := e.Reconcile(context.Background(), reconciler.Options{})
	if err != nil {
		t.Fatalf("%v %+v", err, res.Health)
	}
	if !res.Health[0].OK() {
		t.Fatalf("got %+v", res.Health[0])
	}
	if out := runCmdOut(t, "docker", "inspect", name+"-app", "--format", "{{.State.Health.Status}}"); strings.TrimSpace(out) != "healthy" {
		t.Fatalf("docker should have run the probe and be satisfied, got %q", out)
	}
}

func TestDockerMatrixCompletedInitContainerIsNotReportedMissing(t *testing.T) {
	requireDocker(t)
	digest := dockerImageDigest(t)
	const name = "podcd-e2e-docker-init"
	e := dockerMatrixEngine(t, name, pod(name, digest, `  initContainers:
    - name: setup
      image: IMAGE
      command: ["sh", "-c", "echo ready > /tmp/marker"]
  containers:
    - name: app
      image: IMAGE
`))
	res, err := e.Reconcile(context.Background(), reconciler.Options{})
	if err != nil {
		t.Fatalf("%v %+v", err, res.Health)
	}
	h := healthOf(t, e, 20*time.Second)
	if h.Status != model.HealthHealthy {
		t.Fatalf("a completed init container must not count against the pod: %+v", h)
	}
	if strings.Contains(h.Message, "setup") {
		t.Fatalf("nothing about the finished init container should be in the verdict: %s", h.Message)
	}
	// It ran, once, before the workload: compose keeps it around, exited 0.
	if out := runCmdOut(t, "docker", "inspect", name+"-setup", "--format", "{{.State.Status}} {{.State.ExitCode}}"); strings.TrimSpace(out) != "exited 0" {
		t.Fatalf("init container should be exited 0, got %q", out)
	}
	// A second reconcile changes nothing: an exited init container is not a
	// dead workload container.
	again, err := e.Reconcile(context.Background(), reconciler.Options{})
	if err != nil || len(again.Applied) != 0 {
		t.Fatalf("second reconcile should change nothing: %v %+v", err, again.Applied)
	}
}

func TestDockerMatrixFailedInitContainerIsNamed(t *testing.T) {
	requireDocker(t)
	digest := dockerImageDigest(t)
	const name = "podcd-e2e-docker-init-fail"
	e := dockerMatrixEngine(t, name, pod(name, digest, `  restartPolicy: Never
  initContainers:
    - name: setup
      image: IMAGE
      command: ["sh", "-c", "echo setup went wrong >&2; exit 9"]
  containers:
    - name: app
      image: IMAGE
`))
	// compose refuses to start the workload behind a failed dependency, so
	// the apply itself fails and says so, with the init container's output.
	_, err := e.Reconcile(context.Background(), reconciler.Options{})
	if err == nil || !strings.Contains(err.Error(), "setup went wrong") {
		t.Fatalf("the apply should fail and quote the init container: %v", err)
	}
	h := healthOf(t, e, 30*time.Second)
	if h.Status != model.HealthUnhealthy || !strings.Contains(h.Message, "init container failed: "+name+"-setup exited with code 9") {
		t.Fatalf("got %+v", h)
	}
}
