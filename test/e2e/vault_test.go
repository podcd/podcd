package e2e

// ExternalSecrets against a real HashiCorp Vault. A dev-mode Vault runs in a
// plain `podman run` container - it is the dependency under test, not a
// podcd workload - and is seeded over its HTTP API: a KV v2 secret, the
// AppRole auth method, a policy that can read that secret and nothing else,
// and a role bound to it. Then a host with two SecretStores, one by token
// and one by AppRole, is reconciled and the values are read back from the
// pod. The unit tests fake Vault with httptest; this is where the fake is
// checked against the thing it imitates.

import (
	"bytes"
	"context"
	"encoding/json"
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
	vaultImage     = "docker.io/hashicorp/vault:1.17"
	vaultContainer = "podcd-e2e-vault"
	vaultPort      = 18200
	vaultRootToken = "podcd-e2e-root"
	vaultAppName   = "podcd-e2e-vaulted"
	vaultAppHost   = "podcd-e2e-vault-host"
	vaultAppPort   = 18096
)

// vaultAPI is the slice of Vault's HTTP API the seeding needs.
type vaultAPI struct {
	t    *testing.T
	base string
}

func (v vaultAPI) call(method, path string, body any) map[string]any {
	v.t.Helper()
	var payload io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		payload = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, v.base+"/v1/"+path, payload)
	req.Header.Set("X-Vault-Token", vaultRootToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		v.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		v.t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, out)
	}
	var parsed map[string]any
	_ = json.Unmarshal(out, &parsed)
	return parsed
}

func (v vaultAPI) data(m map[string]any, keys ...string) string {
	cur := m
	for _, k := range keys[:len(keys)-1] {
		cur, _ = cur[k].(map[string]any)
	}
	s, _ := cur[keys[len(keys)-1]].(string)
	return s
}

// startVault runs a dev-mode Vault and returns an API handle once it answers.
// Dev mode is unsealed, KV v2 at secret/, root token as given.
func startVault(t *testing.T) vaultAPI {
	t.Helper()
	if err := exec.Command("podman", "image", "exists", vaultImage).Run(); err != nil {
		t.Skipf("pull %s first (podman pull %s)", vaultImage, vaultImage)
	}
	_ = exec.Command("podman", "rm", "--force", vaultContainer).Run()
	out, err := exec.Command("podman", "run", "--detach", "--name", vaultContainer,
		"--publish", fmt.Sprintf("127.0.0.1:%d:8200", vaultPort),
		"--env", "VAULT_DEV_ROOT_TOKEN_ID="+vaultRootToken,
		"--env", "VAULT_DEV_LISTEN_ADDRESS=0.0.0.0:8200",
		vaultImage).CombinedOutput()
	if err != nil {
		t.Fatalf("starting vault: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("podman", "rm", "--force", vaultContainer).Run() })

	api := vaultAPI{t: t, base: fmt.Sprintf("http://127.0.0.1:%d", vaultPort)}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(api.base + "/v1/sys/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return api
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("vault did not come up within 30s:\n%s", runCmdOut(t, "podman", "logs", vaultContainer))
	return api
}

// seedVault writes the demo secret and sets up an AppRole that can read it,
// returning the role and secret ids the agent will present.
func seedVault(t *testing.T, api vaultAPI, password string) (roleID, secretID string) {
	t.Helper()
	api.call("POST", "secret/data/e2e/app", map[string]any{"data": map[string]any{
		"DB_PASSWORD": password, "token": "t-" + password, "port": 5432,
	}})
	api.call("POST", "secret/data/e2e/forbidden", map[string]any{"data": map[string]any{"x": "never"}})
	// Enabling twice is an error; a leftover Vault from a failed run is removed first, so it is not.
	api.call("POST", "sys/auth/approle", map[string]any{"type": "approle"})
	api.call("PUT", "sys/policies/acl/podcd-e2e", map[string]any{
		"policy": `path "secret/data/e2e/app" { capabilities = ["read"] }`,
	})
	api.call("POST", "auth/approle/role/podcd-e2e", map[string]any{"token_policies": "podcd-e2e", "token_ttl": "1h"})
	roleID = api.data(api.call("GET", "auth/approle/role/podcd-e2e/role-id", nil), "data", "role_id")
	secretID = api.data(api.call("POST", "auth/approle/role/podcd-e2e/secret-id", map[string]any{}), "data", "secret_id")
	if roleID == "" || secretID == "" {
		t.Fatal("vault returned no AppRole credentials")
	}
	return roleID, secretID
}

func writeVaultRepo(t *testing.T, dir, digest, forbidden string) {
	t.Helper()
	extra := ""
	if forbidden != "" {
		extra = fmt.Sprintf(`    - secretKey: forbidden
      remoteRef: {key: %s, property: x}
`, forbidden)
	}
	write(t, filepath.Join(dir, "app.yaml"), fmt.Sprintf(`apiVersion: external-secrets.io/v1beta1
kind: SecretStore
metadata: {name: vault-token}
spec:
  provider:
    vault:
      server: http://127.0.0.1:%[4]d
      auth:
        tokenSecretRef: {name: env:PODCD_E2E_VAULT_TOKEN}
---
apiVersion: external-secrets.io/v1beta1
kind: SecretStore
metadata: {name: vault-approle}
spec:
  provider:
    vault:
      server: http://127.0.0.1:%[4]d
      auth:
        appRole:
          roleId: env:PODCD_E2E_VAULT_ROLE_ID
          secretRef: {name: env:PODCD_E2E_VAULT_SECRET_ID}
---
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata: {name: %[1]s-by-token}
spec:
  secretStoreRef: {name: vault-token}
  data:
    - secretKey: password
      remoteRef: {key: secret/e2e/app, property: DB_PASSWORD}
    - secretKey: port
      remoteRef: {key: secret/e2e/app, property: port}
---
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata: {name: %[1]s-by-approle}
spec:
  secretStoreRef: {name: vault-approle}
  target: {name: %[1]s-files}
  dataFrom:
    - extract: {key: secret/e2e/app}
  data:
%[6]s---
apiVersion: v1
kind: Pod
metadata:
  name: %[1]s
spec:
  volumes:
    - name: token-secret
      secret:
        secretName: %[1]s-by-token
    - name: approle-secret
      secret:
        secretName: %[1]s-files
  containers:
    - name: web
      image: %[2]s
      volumeMounts:
        - name: token-secret
          mountPath: /usr/share/nginx/html/token
        - name: approle-secret
          mountPath: /usr/share/nginx/html/approle
      ports:
        - containerPort: 80
          hostPort: %[5]d
          hostIP: 127.0.0.1
---
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: %[3]s}
spec: {applications: [%[1]s]}
`, vaultAppName, digest, vaultAppHost, vaultPort, vaultAppPort, extra))
}

func TestVaultEndToEnd(t *testing.T) {
	digest := requireE2E(t)
	api := startVault(t)
	roleID, secretID := seedVault(t, api, "pw-one")
	t.Setenv("PODCD_E2E_VAULT_TOKEN", vaultRootToken)
	t.Setenv("PODCD_E2E_VAULT_ROLE_ID", roleID)
	t.Setenv("PODCD_E2E_VAULT_SECRET_ID", secretID)

	ctx := context.Background()
	repoDir := t.TempDir()
	unitDir := userUnitDir(t)
	t.Cleanup(func() {
		_ = exec.Command("systemctl", "--user", "stop", renderer.ServiceName(vaultAppName)).Run()
		_ = os.Remove(filepath.Join(unitDir, renderer.KubeFileName(vaultAppName)))
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
		_ = exec.Command("podman", "pod", "rm", "--force", "--time", "5", vaultAppName).Run()
		for _, s := range []string{vaultAppName + "-by-token", vaultAppName + "-files"} {
			_ = exec.Command("podman", "secret", "rm", s).Run()
		}
	})
	writeVaultRepo(t, repoDir, digest, "")
	gitInit(t, repoDir)

	cfg := config.DefaultAgentConfig()
	cfg.Host = vaultAppHost
	cfg.StateDir = t.TempDir()
	cfg.UnitDir = unitDir
	cfg.Repository = config.RepositorySpec{Name: "infra", URL: repoDir, Revision: "main"}
	engine, err := reconciler.NewEngine(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	// 1. Both stores are read while compiling; the values land as files.
	res, err := engine.Reconcile(ctx, reconciler.Options{})
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if len(res.Applied) != 1 || res.Applied[0].Type != model.ActionCreate || !res.Health[0].OK() {
		t.Fatalf("applied=%+v health=%+v", res.Applied, res.Health)
	}
	for path, want := range map[string]string{
		"/token/password":      "pw-one", // data + property, by token
		"/token/port":          "5432",   // a number in Vault arrives as text
		"/approle/DB_PASSWORD": "pw-one", // dataFrom, by AppRole, under target.name
		"/approle/token":       "t-pw-one",
	} {
		if got := strings.TrimSpace(get(t, vaultAppPort, path)); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	// The manifest on disk is the only place the plaintext lives; never the unit dir.
	unit, _ := os.ReadFile(filepath.Join(unitDir, renderer.KubeFileName(vaultAppName)))
	if strings.Contains(string(unit), "pw-one") {
		t.Fatal("a secret value leaked into the Quadlet unit")
	}

	// 2. Nothing changed: no fetch result differs, no restart.
	again, err := engine.Reconcile(ctx, reconciler.Options{})
	if err != nil || len(again.Applied) != 0 {
		t.Fatalf("second reconcile should change nothing: %v %+v", err, again.Applied)
	}

	// 3. Rotation in Vault, no commit in Git: the next reconcile picks it up.
	api.call("POST", "secret/data/e2e/app", map[string]any{"data": map[string]any{
		"DB_PASSWORD": "pw-two", "token": "t-pw-two", "port": 5432,
	}})
	rotated, err := engine.Reconcile(ctx, reconciler.Options{})
	if err != nil {
		t.Fatalf("reconcile after rotation: %v", err)
	}
	if len(rotated.Applied) != 1 || rotated.Applied[0].Type != model.ActionUpdate {
		t.Fatalf("a rotated secret is an update, got %+v", rotated.Applied)
	}
	for _, d := range rotated.Applied[0].Details {
		if strings.Contains(d, "pw-") {
			t.Fatalf("the plan must not print secret values: %q", d)
		}
	}
	if got := strings.TrimSpace(get(t, vaultAppPort, "/approle/DB_PASSWORD")); got != "pw-two" {
		t.Fatalf("rotation did not reach the pod: %q", got)
	}

	// 4. The AppRole's policy is the boundary: a key it may not read fails
	//    the compile for this application, loudly, and the running pod is
	//    left as it was.
	writeVaultRepo(t, repoDir, digest, "secret/e2e/forbidden")
	gitCommit(t, repoDir, "reach for a secret the role may not read")
	_, err = engine.Reconcile(ctx, reconciler.Options{})
	if err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("a permission denied from Vault must fail the reconcile and name the key: %v", err)
	}
	if got := strings.TrimSpace(get(t, vaultAppPort, "/approle/DB_PASSWORD")); got != "pw-two" {
		t.Fatalf("a failed compile must not touch the running pod: %q", got)
	}
}
