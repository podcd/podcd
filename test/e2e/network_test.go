package e2e

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

	"github.com/podcd/podcd/pkg/config"
	"github.com/podcd/podcd/pkg/reconciler"
	"github.com/podcd/podcd/pkg/renderer"
)

const (
	netName    = "e2e-net" // unit podcd-e2e-net.network, so requireNoOtherWorkloads knows it
	netHost    = "podcd-e2e-net-host"
	serverName = "podcd-e2e-server"
	clientName = "podcd-e2e-client"
	serverPort = 18097
)

// TestNetworkEndToEnd proves a Network document becomes a podman network the
// pods on it resolve each other through, that changing it recreates the
// network and brings the pods back, and that dropping the last pod removes
// it. The ExternalSecret the server reads is a template, rendered with a
// value from the Host's own values file.
func TestNetworkEndToEnd(t *testing.T) {
	if os.Getenv("PODCD_E2E") != "1" {
		t.Skip("set PODCD_E2E=1 to run the end-to-end test (it starts real containers)")
	}
	requireTools(t)
	digest := imageDigest(t)
	t.Setenv("PODCD_E2E_NET_TOKEN", "net-secret")

	ctx := context.Background()
	repoDir := t.TempDir()
	unitDir := userUnitDir(t)
	t.Cleanup(func() { cleanupNetwork(unitDir) })

	writeNetworkConfig(t, repoDir, digest, "10.97.0.0/24", true)
	gitInit(t, repoDir)

	cfg := config.DefaultAgentConfig()
	cfg.Host = netHost
	cfg.StateDir = t.TempDir()
	cfg.UnitDir = unitDir
	cfg.Repository = config.RepositorySpec{Name: "infra", URL: repoDir, Revision: "main"}

	engine, err := reconciler.NewEngine(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

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
	if _, err := os.Stat(filepath.Join(unitDir, renderer.NetworkFileName(netName))); err != nil {
		t.Fatalf("no .network unit was written: %v", err)
	}
	if subnet := networkSubnet(t); subnet != "10.97.0.0/24" {
		t.Fatalf("podman has the network with subnet %q, want 10.97.0.0/24", subnet)
	}
	if labels := runCmdOut(t, "podman", "network", "inspect", netName, "--format", "{{.Labels}}"); !strings.Contains(labels, "io.podcd.network:"+netName) {
		t.Fatalf("the network is not labelled as podcd's: %s", labels)
	}
	if state := strings.TrimSpace(runCmdOut(t, "systemctl", "--user", "is-active", renderer.NetworkServiceName(netName))); state != "active" {
		t.Fatalf("the network's oneshot should stay active, is %q", state)
	}
	if body := crossPodGet(t, "/"); !strings.Contains(body, "on the network") {
		t.Fatalf("the client could not reach the server by pod name over the managed network: %q", body)
	}
	if body := crossPodGet(t, "/secret/token"); strings.TrimSpace(body) != "net-secret" {
		t.Fatalf("the templated ExternalSecret did not reach the server: %q", body)
	}

	// 2. Idempotent, network included.
	again, err := engine.Reconcile(ctx, reconciler.Options{})
	if err != nil || len(again.Applied) != 0 {
		t.Fatalf("second reconcile should change nothing: %v %+v", err, again.Applied)
	}

	// 3. A new subnet: Quadlet cannot change a network, so it is recreated
	//    and both pods come back on it.
	writeNetworkConfig(t, repoDir, digest, "10.98.0.0/24", true)
	gitCommit(t, repoDir, "move the network")
	moved, err := engine.Reconcile(ctx, reconciler.Options{})
	if err != nil {
		t.Fatalf("reconcile after the subnet change: %v", err)
	}
	if got := appliedSummary(moved); got != "update network e2e-net, restart podcd-e2e-client, restart podcd-e2e-server" {
		t.Fatalf("got %q", got)
	}
	if subnet := networkSubnet(t); subnet != "10.98.0.0/24" {
		t.Fatalf("network still has subnet %q after the change", subnet)
	}
	if body := crossPodGet(t, "/"); !strings.Contains(body, "on the network") {
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
	if !networkExists() {
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
	if networkExists() {
		t.Fatal("the network is still there")
	}
	if _, err := os.Stat(filepath.Join(unitDir, renderer.NetworkFileName(netName))); !os.IsNotExist(err) {
		t.Fatal("the .network unit is still there")
	}
	if len(gone.Actual.Networks) != 0 {
		// Actual is what Inspect saw before this reconcile; check afresh.
		after, _ := engine.Runtime().Inspect(ctx)
		if len(after.Networks) != 0 {
			t.Fatalf("Inspect still reports networks: %+v", after.Networks)
		}
	}
}

// writeNetworkConfig writes a Network, a server pod on it that serves a page
// and a templated secret, and - with client - a second pod on it to call the
// server from. The Host's values file carries the secret's variable name.
func writeNetworkConfig(t *testing.T, dir, digest, subnet string, client bool) {
	t.Helper()
	apps := "[" + serverName + "]"
	if client {
		apps = "[" + serverName + ", " + clientName + "]"
	}
	write(t, filepath.Join(dir, "values.yaml"), "tokenVar: PODCD_E2E_NET_TOKEN\n")
	write(t, filepath.Join(dir, "host.yaml"), fmt.Sprintf(`apiVersion: gitops.podcd.io/v1
kind: Host
metadata:
  name: %s
spec:
  applications: %s
  values: [values.yaml]
`, netHost, apps))
	write(t, filepath.Join(dir, "network.yaml"), fmt.Sprintf(`apiVersion: gitops.podcd.io/v1
kind: Network
metadata:
  name: %s
spec:
  subnet: %s
`, netName, subnet))
	// The store decides nothing per host and stays a plain document; the
	// ExternalSecret is the template.
	write(t, filepath.Join(dir, "store.yaml"), `apiVersion: external-secrets.io/v1beta1
kind: SecretStore
metadata:
  name: net-env
spec:
  provider:
    env: {}
`)
	write(t, filepath.Join(dir, "secret.yaml.tpl"), fmt.Sprintf(`apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: %[1]s-secret
spec:
  secretStoreRef:
    name: net-env
  target:
    name: %[1]s-secret
  data:
    - secretKey: token
      remoteRef:
        key: '{{ .Values.tokenVar }}'
`, serverName))
	write(t, filepath.Join(dir, "pods.yaml"), fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %[1]s-html
data:
  index.html: "on the network\n"
---
apiVersion: v1
kind: Pod
metadata:
  name: %[1]s
  annotations:
    io.podcd.networks: "%[3]s"
spec:
  restartPolicy: Always
  volumes:
    - name: html
      configMap:
        name: %[1]s-html
    - name: secret
      secret:
        secretName: %[1]s-secret
  containers:
    - name: web
      image: %[2]s
      ports:
        - containerPort: 80
          hostPort: %[5]d
          hostIP: 127.0.0.1
      volumeMounts:
        - name: html
          mountPath: /usr/share/nginx/html
          readOnly: true
        - name: secret
          mountPath: /usr/share/nginx/html/secret
          readOnly: true
---
apiVersion: v1
kind: Pod
metadata:
  name: %[4]s
  annotations:
    io.podcd.networks: "%[3]s"
spec:
  restartPolicy: Always
  containers:
    - name: client
      image: %[2]s
`, serverName, digest, netName, clientName, serverPort))
}

// crossPodGet fetches a path from the server, from inside the client pod, by
// the server's pod name: the DNS the managed network provides.
func crossPodGet(t *testing.T, path string) string {
	t.Helper()
	var out string
	var err error
	for i := 0; i < 20; i++ {
		cmd := exec.Command("podman", "exec", clientName+"-client", "wget", "-qO-", "-T", "3", "http://"+serverName+path)
		var b []byte
		b, err = cmd.CombinedOutput()
		out = string(b)
		if err == nil {
			return out
		}
	}
	t.Fatalf("wget from the client pod: %v\n%s", err, out)
	return ""
}

func appliedSummary(res reconciler.Result) string {
	var out []string
	for _, a := range res.Applied {
		out = append(out, string(a.Type)+" "+a.Subject())
	}
	return strings.Join(out, ", ")
}

func networkExists() bool {
	return exec.Command("podman", "network", "exists", netName).Run() == nil
}

func networkSubnet(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(runCmdOut(t, "podman", "network", "inspect", netName, "--format", "{{(index .Subnets 0).Subnet}}"))
}

func cleanupNetwork(unitDir string) {
	for _, app := range []string{clientName, serverName} {
		_ = exec.Command("systemctl", "--user", "stop", renderer.ServiceName(app)).Run()
		_ = os.Remove(filepath.Join(unitDir, renderer.KubeFileName(app)))
		_ = exec.Command("podman", "pod", "rm", "--force", "--time", "5", app).Run()
	}
	_ = exec.Command("systemctl", "--user", "stop", renderer.NetworkServiceName(netName)).Run()
	_ = os.Remove(filepath.Join(unitDir, renderer.NetworkFileName(netName)))
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	_ = exec.Command("podman", "network", "rm", netName).Run()
	_ = exec.Command("podman", "secret", "rm", serverName+"-secret").Run()
}
