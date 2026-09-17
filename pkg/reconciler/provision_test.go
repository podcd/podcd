package reconciler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/podcd/podcd/pkg/config"
	"github.com/podcd/podcd/pkg/secrets"
)

// Two hosts, two ExternalSecrets, one store each. vm-1 runs only the app that
// needs "shared", so nothing should make it read "other-only".
const twoHostRepo = `
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
  name: shared
spec:
  secretStoreRef: {name: agent-env}
  target: {name: shared}
  data:
    - secretKey: token
      remoteRef: {key: PODCD_TEST_SHARED}
---
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: other-only
spec:
  secretStoreRef: {name: missing-store}
  target: {name: other-only}
  data:
    - secretKey: token
      remoteRef: {key: PODCD_TEST_OTHER}
---
apiVersion: v1
kind: Pod
metadata:
  name: web
spec:
  containers:
    - name: app
      image: nginx
      envFrom:
        - secretRef: {name: shared}
---
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: vm-1}
spec: {applications: [web]}
`

func indexFrom(t *testing.T, body string) *config.Index {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "repo.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	ix := config.NewIndex()
	if err := ix.LoadTree("test", dir); err != nil {
		t.Fatal(err)
	}
	return ix
}

// A host fetches the secrets its own workloads name, and no others. The
// unreferenced ExternalSecret here points at a SecretStore that does not exist,
// so reaching for it at all would fail the resolve.
func TestProvisionerOnlyFetchesWhatThisHostReferences(t *testing.T) {
	t.Setenv("PODCD_TEST_SHARED", "for-vm-1")
	ix := indexFrom(t, twoHostRepo)

	desired, err := ix.Resolve(context.Background(), config.ResolveOptions{
		Host:    "vm-1",
		Secrets: NewProvisioner(ix, secrets.Default("", "")),
	})
	if err != nil {
		t.Fatalf("resolving vm-1 must not touch the other host's secret: %v", err)
	}
	app, ok := desired.App("web")
	if !ok {
		t.Fatal("web was not resolved")
	}
	if !strings.Contains(string(app.Manifest), "Zm9yLXZtLTE=") { // base64("for-vm-1")
		t.Fatalf("the provisioned value is missing from the manifest:\n%s", app.Manifest)
	}
	if strings.Contains(string(app.Manifest), "other-only") {
		t.Fatalf("a secret this host does not reference was bundled:\n%s", app.Manifest)
	}
}

// One fetch per secret, however many containers name it.
func TestProvisionerCachesWithinOneResolve(t *testing.T) {
	t.Setenv("PODCD_TEST_SHARED", "once")
	ix := indexFrom(t, twoHostRepo)
	p := NewProvisioner(ix, secrets.Default("", ""))

	first, ok, err := p.ProvisionSecret(context.Background(), "shared")
	if err != nil || !ok {
		t.Fatalf("first fetch: %v %v", ok, err)
	}
	t.Setenv("PODCD_TEST_SHARED", "changed-underneath")
	again, ok, err := p.ProvisionSecret(context.Background(), "shared")
	if err != nil || !ok {
		t.Fatalf("second fetch: %v %v", ok, err)
	}
	if string(again.Data["token"]) != string(first.Data["token"]) {
		t.Error("a secret was read twice in one reconcile instead of being cached")
	}
}

func TestProvisionerIgnoresNamesItDoesNotTarget(t *testing.T) {
	ix := indexFrom(t, twoHostRepo)
	_, ok, err := NewProvisioner(ix, secrets.Default("", "")).ProvisionSecret(context.Background(), "not-declared")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("a name no ExternalSecret targets must be left to Git")
	}
}
