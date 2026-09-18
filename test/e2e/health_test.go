package e2e

// The failure matrix: what podcd reports, and does, when a workload is not
// simply running. Every case here was first observed by hand against podman
// 5.3; the assertions are what `podcd health` and `podcd reconcile` must keep
// saying about them. All pods use nginx:alpine, already required by the suite,
// with the command overridden where the container is meant to misbehave.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
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

const matrixHost = "podcd-e2e-matrix"

// matrixEngine wires an engine at a repository holding one pod manifest for
// this host, and removes the pod again when the test ends.
func matrixEngine(t *testing.T, name, manifest string) *reconciler.Engine {
	t.Helper()
	unitDir := userUnitDir(t)
	repoDir := t.TempDir()
	write(t, filepath.Join(repoDir, "app.yaml"), manifest+fmt.Sprintf(`---
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: %s}
spec: {applications: [%s]}
`, matrixHost, name))
	gitInit(t, repoDir)
	t.Cleanup(func() {
		_ = exec.Command("systemctl", "--user", "stop", renderer.ServiceName(name)).Run()
		_ = os.Remove(filepath.Join(unitDir, renderer.KubeFileName(name)))
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
		_ = exec.Command("podman", "pod", "rm", "--force", "--time", "5", name).Run()
	})

	cfg := config.DefaultAgentConfig()
	cfg.Host = matrixHost
	cfg.StateDir = t.TempDir()
	cfg.UnitDir = unitDir
	cfg.Repository = config.RepositorySpec{Name: "infra", URL: repoDir, Revision: "main"}
	engine, err := reconciler.NewEngine(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func requireE2E(t *testing.T) string {
	t.Helper()
	if os.Getenv("PODCD_E2E") != "1" {
		t.Skip("set PODCD_E2E=1 to run the end-to-end test (it starts real containers)")
	}
	requireTools(t)
	return imageDigest(t)
}

// healthOf keeps asking until the verdict settles on something other than
// unknown, or the deadline passes. A probe needs a few periods to conclude.
func healthOf(t *testing.T, e *reconciler.Engine, within time.Duration) model.Health {
	t.Helper()
	deadline := time.Now().Add(within)
	var last model.Health
	for time.Now().Before(deadline) {
		hs, err := e.Health(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		last = hs[0]
		if last.Status != model.HealthUnknown {
			return last
		}
		time.Sleep(2 * time.Second)
	}
	return last
}

func pod(name, digest, body string) string {
	return fmt.Sprintf("apiVersion: v1\nkind: Pod\nmetadata:\n  name: %s\nspec:\n%s", name, strings.ReplaceAll(body, "IMAGE", digest))
}

// A container that exits non-zero under restartPolicy Never: the pod's unit
// stops and `kube down` takes the containers with it, so the verdict has no
// container to point at and quotes what the container last printed instead.
func TestMatrixExitedContainerIsUnhealthyWithItsExitCode(t *testing.T) {
	digest := requireE2E(t)
	const name = "podcd-e2e-exit"
	e := matrixEngine(t, name, pod(name, digest, `  restartPolicy: Never
  containers:
    - name: app
      image: IMAGE
      command: ["sh", "-c", "echo fatal: config missing >&2; exit 3"]
`))
	// Whether the reconcile itself fails depends on whether its health poll
	// catches the container in its few hundred milliseconds of running - the
	// verdict is podman's at that instant - so what must hold is where the
	// host settles: unhealthy, with no container left and the last output.
	_, _ = e.Reconcile(context.Background(), reconciler.Options{})
	h := settlesUnhealthy(t, e, 30*time.Second, "fatal: config missing")
	if !strings.Contains(h.Message, "no containers") {
		t.Errorf("message should say %q: %s", "no containers", h.Message)
	}
	// A further reconcile restarts the unit (the plan wants it running) and
	// the container exits again, so the host settles the same way.
	_, _ = e.Reconcile(context.Background(), reconciler.Options{})
	settlesUnhealthy(t, e, 30*time.Second, "fatal: config missing")
}

// settlesUnhealthy keeps asking until the verdict is unhealthy for the given
// reason, or the deadline passes and the test fails with the last verdict.
func settlesUnhealthy(t *testing.T, e *reconciler.Engine, within time.Duration, reason string) model.Health {
	t.Helper()
	deadline := time.Now().Add(within)
	var h model.Health
	for time.Now().Before(deadline) {
		hs, err := e.Health(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if h = hs[0]; h.Status == model.HealthUnhealthy && strings.Contains(h.Message, reason) {
			return h
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("the host must settle unhealthy with %q: %+v", reason, h)
	return h
}

// Under restartPolicy Always the same container crash-loops; the restart
// count is what tells the operator so.
func TestMatrixCrashLoopShowsRestarts(t *testing.T) {
	digest := requireE2E(t)
	const name = "podcd-e2e-crashloop"
	e := matrixEngine(t, name, pod(name, digest, `  restartPolicy: Always
  containers:
    - name: app
      image: IMAGE
      command: ["sh", "-c", "sleep 1; exit 7"]
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
		if strings.Contains(h.Message, "restarted") {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !strings.Contains(h.Message, "restarted") {
		t.Fatalf("a crash loop should be visible through its restart count: %+v", h)
	}
}

// A liveness probe that cannot pass. What podman does about it depends on its version:
// 5.x restarts the container every failureThreshold, it is "starting" again with a climbing restart count;
// 4.x marks it "unhealthy" and leaves it.
// Either way the verdict must never read healthy, must name the container, and must quote the probe's own output.
func TestMatrixFailingLivenessProbeIsExplained(t *testing.T) {
	digest := requireE2E(t)
	const name = "podcd-e2e-probe"
	e := matrixEngine(t, name, pod(name, digest, `  restartPolicy: Always
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
	for _, want := range []string{name + "-app", "check failure", "last check: probe says no"} {
		if !strings.Contains(h.Message, want) {
			t.Errorf("message should say %q: %s", want, h.Message)
		}
	}
	if !strings.Contains(h.Message, "healthcheck failing") && !strings.Contains(h.Message, "healthcheck not passed yet") {
		t.Errorf("the verdict should be one of podman's two answers to a failing probe: %s", h.Message)
	}
}

// A passing liveness probe: starting first, then healthy, never anything else.
func TestMatrixPassingLivenessProbeBecomesHealthy(t *testing.T) {
	digest := requireE2E(t)
	const name = "podcd-e2e-probe-ok"
	e := matrixEngine(t, name, pod(name, digest, `  containers:
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
	if out := runCmdOut(t, "podman", "inspect", name+"-app", "--format", "{{.State.Health.Status}}"); strings.TrimSpace(out) != "healthy" {
		t.Fatalf("podman should have run the probe and be satisfied, got %q", out)
	}
}

// Init containers run to completion and vanish (podman removes `once` init
// containers). A vanished init container is a finished one; the pod is healthy.
func TestMatrixCompletedInitContainerIsNotReportedMissing(t *testing.T) {
	digest := requireE2E(t)
	const name = "podcd-e2e-init"
	e := matrixEngine(t, name, pod(name, digest, `  initContainers:
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
}

// An init container that fails stops the pod from starting: the unit fails,
// the pod is taken down, and the verdict quotes what the init container said.
func TestMatrixFailedInitContainerIsNamed(t *testing.T) {
	digest := requireE2E(t)
	const name = "podcd-e2e-init-fail"
	e := matrixEngine(t, name, pod(name, digest, `  restartPolicy: Never
  initContainers:
    - name: setup
      image: IMAGE
      command: ["sh", "-c", "echo setup went wrong >&2; exit 9"]
  containers:
    - name: app
      image: IMAGE
`))
	_, _ = e.Reconcile(context.Background(), reconciler.Options{})
	h := healthOf(t, e, 30*time.Second)
	if h.Status != model.HealthUnhealthy {
		t.Fatalf("got %+v", h)
	}
	for _, want := range []string{"no containers", "last output: setup went wrong"} {
		if !strings.Contains(h.Message, want) {
			t.Errorf("message should say %q: %s", want, h.Message)
		}
	}
}

// A container killed by hand while its unit stays active: the next reconcile
// notices the dead container and restarts the unit.
func TestMatrixKilledContainerIsRestartedByReconcile(t *testing.T) {
	digest := requireE2E(t)
	const name = "podcd-e2e-killed"
	e := matrixEngine(t, name, pod(name, digest, `  restartPolicy: Never
  containers:
    - name: app
      image: IMAGE
`))
	if res, err := e.Reconcile(context.Background(), reconciler.Options{}); err != nil {
		t.Fatalf("%v %+v", err, res.Health)
	}
	runIn(t, ".", "podman", "kill", name+"-app")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if out := runCmdOut(t, "podman", "inspect", name+"-app", "--format", "{{.State.Status}}"); strings.TrimSpace(out) == "exited" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	plan, err := e.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// With restartPolicy Never and its only container dead, the pod's exit
	// policy stops it and the unit follows; depending on when the plan looks,
	// it sees the exited container or the unit already on its way down.
	// Either way it must want a restart.
	changes := plan.Plan.Changes()
	if len(changes) != 1 || changes[0].Type != model.ActionRestart {
		t.Fatalf("the plan should want a restart because the container is dead: %+v", changes)
	}
	res, err := e.Reconcile(context.Background(), reconciler.Options{})
	if err != nil {
		t.Fatalf("%v %+v", err, res.Health)
	}
	if len(res.Applied) != 1 || res.Applied[0].Type != model.ActionRestart || !res.Health[0].OK() {
		t.Fatalf("got applied=%+v health=%+v", res.Applied, res.Health)
	}
	if out := runCmdOut(t, "podman", "inspect", name+"-app", "--format", "{{.State.Status}}"); strings.TrimSpace(out) != "running" {
		t.Fatalf("the container should be running again, got %q", out)
	}
}

// A pod whose containers never appear (an image that cannot be pulled): the
// unit fails to start, reconcile fails, and the error quotes the journal.
func TestMatrixUnpullableImageFailsTheApply(t *testing.T) {
	requireE2E(t)
	const name = "podcd-e2e-nopull"
	e := matrixEngine(t, name, pod(name, "", `  containers:
    - name: app
      image: localhost/podcd-e2e/does-not-exist@sha256:0000000000000000000000000000000000000000000000000000000000000000
`))
	_, err := e.Reconcile(context.Background(), reconciler.Options{})
	if err == nil {
		t.Fatal("an image that cannot be pulled must fail the reconcile")
	}
	if !strings.Contains(err.Error(), "starting "+renderer.ServiceName(name)) {
		t.Fatalf("the error should name the unit that failed to start: %v", err)
	}
}
