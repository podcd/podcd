package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pinned = "@sha256:1111111111111111111111111111111111111111111111111111111111111111"

func lintFiles(t *testing.T, files map[string]string) (*Index, []Finding, error) {
	t.Helper()
	dir := t.TempDir()
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return LintPaths(context.Background(), nil, nil, dir)
}

func TestLintCompilesEveryHost(t *testing.T) {
	// vm-1 is fine; vm-2 references an undefined application and clashes on a port.
	_, findings, err := lintFiles(t, map[string]string{
		"apps.yaml": `
apiVersion: v1
kind: Pod
metadata: {name: web}
spec:
  containers:
    - name: web
      image: example.com/web` + pinned + `
      ports:
        - containerPort: 80
          hostPort: 8080
---
apiVersion: v1
kind: Pod
metadata: {name: api}
spec:
  containers:
    - name: api
      image: example.com/api` + pinned + `
      ports:
        - containerPort: 8080
          hostPort: 8080
`,
		"hosts.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: vm-1}
spec: {applications: [web]}
---
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: vm-2}
spec: {applications: [web, api, missing]}
`,
	})
	if err != nil {
		t.Fatal(err)
	}
	var vm1, vm2 []string
	for _, f := range findings {
		if f.Host == "vm-1" {
			vm1 = append(vm1, f.Err)
		} else {
			vm2 = append(vm2, f.Err)
		}
	}
	if len(vm1) != 0 {
		t.Errorf("vm-1 should be clean (its secret is only checked for shape): %v", vm1)
	}
	joined := strings.Join(vm2, "\n")
	if len(vm2) != 2 || !strings.Contains(joined, `"missing"`) || !strings.Contains(joined, "both publish host port 8080") {
		t.Errorf("vm-2 should have exactly the undefined reference and the port clash, got %v", vm2)
	}
}

func TestLintLoaderErrorsAndNoHosts(t *testing.T) {
	_, _, err := lintFiles(t, map[string]string{"a.yaml": "apiVersion: v1\nkind: Pod\nmetadata: {name: x}\nspec: {containers: [{name: x, imagee: y}]}\n"})
	if err == nil || !strings.Contains(err.Error(), "imagee") {
		t.Fatalf("a loader error is returned as an error, not a finding: %v", err)
	}
	_, _, err = lintFiles(t, map[string]string{"a.yaml": "apiVersion: v1\nkind: Pod\nmetadata: {name: x}\nspec: {containers: [{name: x, image: y" + pinned + "}]}\n"})
	if !errors.Is(err, ErrNoHosts) {
		t.Fatalf("documents without a Host should report ErrNoHosts, got %v", err)
	}
}

func TestLintAcceptsFilesAndLimitsToNamedHosts(t *testing.T) {
	dir := t.TempDir()
	apps := filepath.Join(dir, "apps.yaml")
	hosts := filepath.Join(dir, "hosts.yaml")
	os.WriteFile(apps, []byte("apiVersion: v1\nkind: Pod\nmetadata: {name: web}\nspec: {containers: [{name: web, image: example.com/web"+pinned+"}]}\n"), 0o644)
	os.WriteFile(hosts, []byte("apiVersion: gitops.podcd.io/v1\nkind: Host\nmetadata: {name: ok}\nspec: {applications: [web]}\n---\napiVersion: gitops.podcd.io/v1\nkind: Host\nmetadata: {name: bad}\nspec: {applications: [nope]}\n"), 0o644)

	_, findings, err := LintPaths(context.Background(), nil, nil, apps, hosts)
	if err != nil || len(findings) != 1 || findings[0].Host != "bad" {
		t.Fatalf("two files, one bad host: %v %v", findings, err)
	}
	_, findings, err = LintPaths(context.Background(), []string{"ok"}, nil, apps, hosts)
	if err != nil || len(findings) != 0 {
		t.Fatalf("--host ok should be clean: %v %v", findings, err)
	}
	if _, _, err := LintPaths(context.Background(), []string{"ghost"}, nil, apps, hosts); err == nil {
		t.Fatal("an unknown --host must be an error")
	}
}

func TestSplitDocumentsReportsLinesAndSurvivesOddEndings(t *testing.T) {
	// CRLF endings, a comment-only document, and no newline at the end.
	data := []byte("# top\r\n---\r\n\r\napiVersion: v1\r\nkind: ConfigMap\r\n---\r\n# nothing here\r\n---\r\nkind: Secret")
	docs, err := SplitDocuments("r", "f.yaml", data)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		t.Fatalf("want 2 documents, got %d: %q", len(docs), docs)
	}
	if docs[0].Line != 4 || docs[1].Line != 9 {
		t.Errorf("lines = %d, %d; want 4, 9", docs[0].Line, docs[1].Line)
	}
	if string(docs[1].Raw) != "kind: Secret\n" || docs[1].String() != "r/f.yaml:9" {
		t.Errorf("last document = %q at %s", docs[1].Raw, docs[1])
	}

	// A file that ends in garbage is an error naming the line, not a panic.
	bad := filepath.Join(t.TempDir(), "env.yaml")
	os.WriteFile(bad, []byte("apiVersion: gitops.podcd.io/v1\nkind: Environment\nmetadata:\n  name: local\nspec:\n  applications:\n    - local\n123"), 0o644)
	if _, err := LoadPaths(bad); err == nil || !strings.Contains(err.Error(), "env.yaml:1") {
		t.Fatalf("want a located error, got %v", err)
	}
}

// Linting must never reach a secret store, so it runs with no provisioner. A
// Secret that only an ExternalSecret produces still has to pass: it does not
// exist in Git and will not exist until the agent reconciles.
func TestLintAcceptsSecretsOnlyAnExternalSecretProduces(t *testing.T) {
	_, findings, err := lintFiles(t, map[string]string{
		"repo.yaml": `
apiVersion: external-secrets.io/v1beta1
kind: SecretStore
metadata: {name: vault}
spec:
  provider:
    vault:
      server: https://vault.example.com
      auth:
        tokenSecretRef: {name: env:VAULT_TOKEN}
---
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata: {name: db-creds}
spec:
  secretStoreRef: {name: vault}
  target: {name: renamed-db}
  data:
    - secretKey: PASSWORD
      remoteRef: {key: secret/prod/db, property: password}
---
apiVersion: v1
kind: Pod
metadata: {name: web}
spec:
  containers:
    - name: app
      image: example.com/web` + pinned + `
      envFrom:
        - secretRef: {name: renamed-db}
---
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: vm-1}
spec: {applications: [web]}
`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("an ExternalSecret target must satisfy the reference at lint time: %+v", findings)
	}
}

func TestLintStillReportsASecretNobodyProduces(t *testing.T) {
	_, findings, err := lintFiles(t, map[string]string{
		"repo.yaml": `
apiVersion: v1
kind: Pod
metadata: {name: web}
spec:
  containers:
    - name: app
      image: example.com/web` + pinned + `
      envFrom:
        - secretRef: {name: nowhere}
---
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: vm-1}
spec: {applications: [web]}
`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) == 0 {
		t.Fatal("a Secret that nothing defines or provisions must still be reported")
	}
}
