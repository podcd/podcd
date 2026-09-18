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

	"github.com/podcd/podcd/pkg/config"
	"github.com/podcd/podcd/pkg/model"
	"github.com/podcd/podcd/pkg/reconciler"
	"github.com/podcd/podcd/pkg/renderer"
)

const (
	podName   = "podcd-e2e-pod"
	podHost   = "podcd-e2e-kube-host"
	frontPort = 18097
	sidePort  = 18098
)

// TestPodEndToEnd plays a real two-container pod through Quadlet's .kube path.
//
// Both containers are nginx. The front one serves an index.html mounted from a
// ConfigMap and a token mounted from an ExternalSecret-provisioned Secret; the side one
// listens on 8081 with a server block mounted from a second ConfigMap. Then the
// ConfigMap changes, a strategic merge override patches the side container,
// the secret rotates, and the pod is removed again.
func TestPodEndToEnd(t *testing.T) {
	if os.Getenv("PODCD_E2E") != "1" {
		t.Skip("set PODCD_E2E=1 to run the end-to-end test (it starts real containers)")
	}
	requireTools(t)
	digest := imageDigest(t)
	t.Setenv("PODCD_E2E_SECRET", "first-secret")

	ctx := context.Background()
	repoDir := t.TempDir()
	unitDir := userUnitDir(t)
	t.Cleanup(func() { cleanupPod(unitDir) })

	writePodConfig(t, repoDir, digest, "hello", false)
	gitInit(t, repoDir)

	cfg := config.DefaultAgentConfig()
	cfg.Host = podHost
	cfg.StateDir = t.TempDir()
	cfg.UnitDir = unitDir
	cfg.Repository = config.RepositorySpec{Name: "infra", URL: repoDir, Revision: "main"}

	engine, err := reconciler.NewEngine(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	// 1. Pod, ConfigMaps and Secret come up through `podman kube play`.
	res, err := engine.Reconcile(ctx, reconciler.Options{})
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if len(res.Applied) != 1 || res.Applied[0].Type != model.ActionCreate {
		t.Fatalf("want one create, got %+v", res.Applied)
	}
	if !res.Health[0].OK() {
		t.Fatalf("the pod is not healthy: %+v", res.Health)
	}
	if body := get(t, frontPort, "/"); !strings.Contains(body, "greeting=hello") {
		t.Fatalf("the ConfigMap did not reach the front container: %q", body)
	}
	if body := get(t, frontPort, "/secret/token"); strings.TrimSpace(body) != "first-secret" {
		t.Fatalf("the Secret did not reach the front container: %q", body)
	}
	if body := get(t, sidePort, "/"); !strings.Contains(body, "side=v1") {
		t.Fatalf("the side container's ConfigMap config did not apply: %q", body)
	}
	if _, err := os.Stat(filepath.Join(unitDir, renderer.KubeFileName(podName))); err != nil {
		t.Fatalf("no .kube unit was written: %v", err)
	}
	info, err := os.Stat(filepath.Join(cfg.KubeDir(), podName+".yaml"))
	if err != nil {
		t.Fatalf("no manifest was written: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the manifest holds a resolved secret and must be 0600, got %v", info.Mode().Perm())
	}

	// 2. Idempotent.
	again, err := engine.Reconcile(ctx, reconciler.Options{})
	if err != nil || len(again.Applied) != 0 {
		t.Fatalf("second reconcile should change nothing: %v %+v", err, again.Applied)
	}

	// 3. ConfigMap change + strategic merge patch on the side container.
	writePodConfig(t, repoDir, digest, "changed", true)
	gitCommit(t, repoDir, "change config and patch the side container")
	changed, err := engine.Reconcile(ctx, reconciler.Options{})
	if err != nil {
		t.Fatalf("reconcile after change: %v", err)
	}
	if len(changed.Applied) != 1 || changed.Applied[0].Type != model.ActionUpdate {
		t.Fatalf("want one update, got %+v", changed.Applied)
	}
	if body := get(t, frontPort, "/"); !strings.Contains(body, "greeting=changed") {
		t.Fatalf("the ConfigMap change did not reach the pod: %q", body)
	}
	if env := runCmdOut(t, "podman", "inspect", podName+"-side", "--format", "{{range .Config.Env}}{{println .}}{{end}}"); !strings.Contains(env, "PATCHED=yes") {
		t.Fatalf("the strategic merge patch did not reach the side container: %s", env)
	}
	if body := get(t, sidePort, "/"); !strings.Contains(body, "side=v1") {
		t.Fatalf("the patch should have left the side container's config alone: %q", body)
	}

	// 4. Secret rotation restarts the pod; the plan never shows either value.
	t.Setenv("PODCD_E2E_SECRET", "second-secret")
	plan, err := engine.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Plan.Empty() {
		t.Fatal("a rotated secret must plan an update")
	}
	for _, a := range plan.Plan.Actions {
		for _, d := range a.Details {
			if strings.Contains(d, "second-secret") || strings.Contains(d, "first-secret") {
				t.Fatalf("a secret value leaked into the plan: %q", d)
			}
		}
	}
	if _, err := engine.Reconcile(ctx, reconciler.Options{}); err != nil {
		t.Fatalf("reconcile after rotation: %v", err)
	}
	if body := get(t, frontPort, "/secret/token"); strings.TrimSpace(body) != "second-secret" {
		t.Fatalf("the rotated secret did not reach the pod: %q", body)
	}

	// 5. Removal takes the pod, the containers, the unit, the manifest and the secret.
	writeEmptyKubeHost(t, repoDir)
	gitCommit(t, repoDir, "remove the pod")
	pruned, err := engine.Reconcile(ctx, reconciler.Options{})
	if err != nil {
		t.Fatalf("reconcile after removal: %v", err)
	}
	if len(pruned.Applied) != 1 || pruned.Applied[0].Type != model.ActionDelete {
		t.Fatalf("want one delete, got %+v", pruned.Applied)
	}
	if out := runCmdOut(t, "podman", "pod", "ps", "--filter", "name="+podName, "--format", "{{.Name}}"); strings.TrimSpace(out) != "" {
		t.Fatalf("the pod is still there: %q", out)
	}
	if _, err := os.Stat(filepath.Join(cfg.KubeDir(), podName+".yaml")); !os.IsNotExist(err) {
		t.Fatal("the manifest was not removed")
	}
}

// writePodConfig writes the pod repository. greeting is served by the front
// container from a ConfigMap; patchSide adds a host override that sets an
// environment variable on the side container through a strategic merge patch.
func writePodConfig(t *testing.T, dir, digest, greeting string, patchSide bool) {
	t.Helper()
	os.Remove(filepath.Join(dir, "host.yaml"))
	override := ""
	if patchSide {
		override = fmt.Sprintf(`  overrides:
    %s:
      spec:
        containers:
          - name: side
            env:
              - name: PATCHED
                value: "yes"
`, podName)
	}
	write(t, filepath.Join(dir, "pod.yaml"), fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %[1]s-html
data:
  index.html: "greeting=%[4]s\n"
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: %[1]s-side-conf
data:
  default.conf: |
    server { listen %[6]d; location / { return 200 "side=v1\n"; } }
---
apiVersion: external-secrets.io/v1beta1
kind: SecretStore
metadata:
  name: agent-env
spec:
  provider:
    env: {}
---
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: %[1]s-secret
spec:
  secretStoreRef:
    name: agent-env
  target:
    name: %[1]s-secret
  data:
    - secretKey: token
      remoteRef:
        key: PODCD_E2E_SECRET
---
apiVersion: v1
kind: Pod
metadata:
  name: %[1]s
spec:
  volumes:
    - name: html
      configMap:
        name: %[1]s-html
    - name: side-conf
      configMap:
        name: %[1]s-side-conf
    - name: secret
      secret:
        secretName: %[1]s-secret
  containers:
    - name: front
      image: %[2]s
      volumeMounts:
        - name: html
          mountPath: /usr/share/nginx/html
        - name: secret
          mountPath: /usr/share/nginx/html/secret
      ports:
        - containerPort: 80
          hostPort: %[5]d
          hostIP: 127.0.0.1
      readinessProbe:
        httpGet:
          path: /
          port: 80
    - name: side
      image: %[2]s
      volumeMounts:
        - name: side-conf
          mountPath: /etc/nginx/conf.d
      ports:
        - containerPort: %[6]d
          hostPort: %[6]d
          hostIP: 127.0.0.1
---
apiVersion: gitops.podcd.io/v1
kind: Host
metadata:
  name: %[3]s
spec:
  applications: [%[1]s]
%[7]s`, podName, digest, podHost, greeting, frontPort, sidePort, override))
}

func writeEmptyKubeHost(t *testing.T, dir string) {
	t.Helper()
	os.Remove(filepath.Join(dir, "pod.yaml"))
	write(t, filepath.Join(dir, "host.yaml"), fmt.Sprintf("apiVersion: gitops.podcd.io/v1\nkind: Host\nmetadata:\n  name: %s\nspec: {}\n", podHost))
}

func get(t *testing.T, port int, path string) string {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, path))
	if err != nil {
		t.Fatalf("GET :%d%s: %v", port, path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func cleanupPod(unitDir string) {
	_ = exec.Command("systemctl", "--user", "stop", renderer.ServiceName(podName)).Run()
	_ = os.Remove(filepath.Join(unitDir, renderer.KubeFileName(podName)))
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	_ = exec.Command("podman", "pod", "rm", "--force", "--time", "5", podName).Run()
	_ = exec.Command("podman", "secret", "rm", podName+"-secret").Run()
}
