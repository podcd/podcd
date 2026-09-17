package reconciler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/podcd/podcd/pkg/config"
	"github.com/podcd/podcd/pkg/secrets"
)

// fakeVault answers the two calls a SecretStore's vault provider makes: an
// AppRole login and KV v2 reads. It checks credentials so a test can tell
// that the store's auth block was wired through, not just that a request left.
type fakeVault struct {
	token    string // the static token the token store must present
	roleID   string
	secretID string
	data     map[string]map[string]any
	logins   atomic.Int32
	reads    atomic.Int32
	lastNS   string
}

func (f *fakeVault) serve(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/approle/login", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["role_id"] != f.roleID || body["secret_id"] != f.secretID {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.logins.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{"client_token": "approle-token", "lease_duration": 3600}})
	})
	mux.HandleFunc("GET /v1/", func(w http.ResponseWriter, r *http.Request) {
		f.lastNS = r.Header.Get("X-Vault-Namespace")
		if tok := r.Header.Get("X-Vault-Token"); tok != f.token && tok != "approle-token" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		f.reads.Add(1)
		path := strings.Replace(strings.TrimPrefix(r.URL.Path, "/v1/"), "/data/", "/", 1)
		values, ok := f.data[path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": values}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func newFakeVault(t *testing.T) (*fakeVault, string) {
	f := &fakeVault{token: "root-token", roleID: "rid", secretID: "sid", data: map[string]map[string]any{
		"secret/demo/db":  {"username": "app", "password": "hunter2", "port": 5432},
		"secret/demo/tls": {"certificate": "CERT", "private_key": "KEY"},
	}}
	return f, f.serve(t)
}

// resolverWith gives the agent an env-file with these values, as agent.env would.
func resolverWith(t *testing.T, env string) *secrets.Resolver {
	t.Helper()
	dir := t.TempDir()
	envFile := filepath.Join(dir, "agent.env")
	if err := os.WriteFile(envFile, []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	return secrets.Default(filepath.Join(dir, "secrets"), envFile)
}

const vaultRepoTemplate = `
apiVersion: external-secrets.io/v1beta1
kind: SecretStore
metadata: {name: by-token}
spec:
  provider:
    vault:
      server: SERVER
      namespace: team-a
      auth:
        tokenSecretRef: {name: env:VAULT_TOKEN}
---
apiVersion: external-secrets.io/v1beta1
kind: SecretStore
metadata: {name: by-approle}
spec:
  provider:
    vault:
      server: SERVER
      auth:
        appRole:
          roleId: env:VAULT_ROLE_ID
          secretRef: {name: env:VAULT_SECRET_ID}
---
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata: {name: demo-db}
spec:
  secretStoreRef: {name: by-token}
  data:
    - secretKey: DB_PASSWORD
      remoteRef: {key: secret/demo/db, property: password}
    - secretKey: DB_PORT
      remoteRef: {key: secret/demo/db, property: port}
  dataFrom:
    - extract: {key: secret/demo/tls}
---
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata: {name: demo-tls}
spec:
  secretStoreRef: {name: by-approle}
  target: {name: demo-tls-material}
  dataFrom:
    - extract: {key: secret/demo/tls}
---
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata: {name: no-property}
spec:
  secretStoreRef: {name: by-token}
  data:
    - secretKey: x
      remoteRef: {key: secret/demo/db}
---
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata: {name: no-such-store}
spec:
  secretStoreRef: {name: nowhere}
  data:
    - secretKey: x
      remoteRef: {key: secret/demo/db, property: password}
`

func vaultProvisioner(t *testing.T, env string) (*Provisioner, *fakeVault) {
	t.Helper()
	f, url := newFakeVault(t)
	ix := indexFrom(t, strings.ReplaceAll(vaultRepoTemplate, "SERVER", url))
	return NewProvisioner(ix, resolverWith(t, env)), f
}

func TestVaultStoreWithATokenFetchesFieldsAndWholeSecrets(t *testing.T) {
	p, f := vaultProvisioner(t, "VAULT_TOKEN=root-token\n")
	sec, ok, err := p.ProvisionSecret(context.Background(), "demo-db")
	if err != nil || !ok {
		t.Fatalf("got ok=%v err=%v", ok, err)
	}
	want := map[string]string{"DB_PASSWORD": "hunter2", "DB_PORT": "5432", "certificate": "CERT", "private_key": "KEY"}
	for k, v := range want {
		if string(sec.Data[k]) != v {
			t.Errorf("%s = %q, want %q", k, sec.Data[k], v)
		}
	}
	if len(sec.Data) != len(want) {
		t.Errorf("unexpected keys in %v", sec.Data)
	}
	if sec.Name != "demo-db" || sec.Labels[config.LabelManaged] != "true" || sec.Kind != "Secret" {
		t.Errorf("the produced Secret should be named after the target and marked managed: %+v", sec.ObjectMeta)
	}
	if f.logins.Load() != 0 {
		t.Error("a token store must never log in")
	}
	if f.lastNS != "team-a" {
		t.Errorf("the store's namespace should reach Vault, got %q", f.lastNS)
	}
}

func TestVaultStoreWithAppRoleLogsInWithTheAgentsCredentials(t *testing.T) {
	p, f := vaultProvisioner(t, "VAULT_ROLE_ID=rid\nVAULT_SECRET_ID=sid\n")
	// The ExternalSecret is demo-tls; the Secret it produces is demo-tls-material.
	sec, ok, err := p.ProvisionSecret(context.Background(), "demo-tls-material")
	if err != nil || !ok {
		t.Fatalf("got ok=%v err=%v", ok, err)
	}
	if string(sec.Data["certificate"]) != "CERT" || string(sec.Data["private_key"]) != "KEY" {
		t.Fatalf("got %v", sec.Data)
	}
	if f.logins.Load() != 1 {
		t.Errorf("want one AppRole login, got %d", f.logins.Load())
	}
	if _, ok, _ := p.ProvisionSecret(context.Background(), "demo-tls"); ok {
		t.Error("the ExternalSecret's own name is not a Secret when target.name differs")
	}
}

func TestVaultStoreFailsLoudlyOnBadCredentials(t *testing.T) {
	p, _ := vaultProvisioner(t, "VAULT_ROLE_ID=rid\nVAULT_SECRET_ID=wrong\n")
	if _, _, err := p.ProvisionSecret(context.Background(), "demo-tls-material"); err == nil {
		t.Fatal("a rejected AppRole login must fail the provision")
	}
	p, _ = vaultProvisioner(t, "VAULT_TOKEN=stale\n")
	if _, _, err := p.ProvisionSecret(context.Background(), "demo-db"); err == nil {
		t.Fatal("a rejected token must fail the provision")
	}
	// No credentials in agent.env at all.
	p, _ = vaultProvisioner(t, "")
	if _, _, err := p.ProvisionSecret(context.Background(), "demo-db"); err == nil || !strings.Contains(err.Error(), "VAULT_TOKEN") {
		t.Fatalf("a missing env var should be named: %v", err)
	}
}

func TestVaultDataEntriesMustNameAProperty(t *testing.T) {
	p, _ := vaultProvisioner(t, "VAULT_TOKEN=root-token\n")
	_, _, err := p.ProvisionSecret(context.Background(), "no-property")
	if err == nil || !strings.Contains(err.Error(), "must set property") {
		t.Fatalf("a KV secret is a map; a data entry without property is ambiguous: %v", err)
	}
}

func TestUnknownStoreAndMissingKeysAreErrors(t *testing.T) {
	p, _ := vaultProvisioner(t, "VAULT_TOKEN=root-token\n")
	if _, _, err := p.ProvisionSecret(context.Background(), "no-such-store"); err == nil || !strings.Contains(err.Error(), `unknown SecretStore "nowhere"`) {
		t.Fatalf("got %v", err)
	}

	ix := indexFrom(t, `
apiVersion: external-secrets.io/v1beta1
kind: SecretStore
metadata: {name: e}
spec: {provider: {env: {}}}
---
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata: {name: s}
spec:
  secretStoreRef: {name: e}
  data:
    - secretKey: token
      remoteRef: {key: PODCD_TEST_DEFINITELY_UNSET}
`)
	_, _, err := NewProvisioner(ix, resolverWith(t, "")).ProvisionSecret(context.Background(), "s")
	if err == nil || !strings.Contains(err.Error(), `key "token"`) {
		t.Fatalf("a missing env var should name the secretKey it was for: %v", err)
	}
}

func TestVaultStoreWithoutAuthIsRejected(t *testing.T) {
	_, err := newSecretFetcher(config.SecretStoreProvider{Vault: &config.VaultStoreProvider{Server: "http://x"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "no auth method") {
		t.Fatalf("got %v", err)
	}
	if _, err := newSecretFetcher(config.SecretStoreProvider{}, nil); err == nil || !strings.Contains(err.Error(), "no recognized provider") {
		t.Fatalf("got %v", err)
	}
}

func TestEnvStoreReadsTheAgentEnvAndKVFilesForDataFrom(t *testing.T) {
	r := resolverWith(t, "API_KEY=k-1\n")
	kv := filepath.Join(t.TempDir(), "extra.env")
	if err := os.WriteFile(kv, []byte("# comment\n\nA = 1\nB=two=parts\nnot-a-pair\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &envSecretFetcher{resolver: r}
	if v, err := f.fetch(context.Background(), "API_KEY", ""); err != nil || v != "k-1" {
		t.Fatalf("got %q %v", v, err)
	}
	all, err := f.fetchAll(context.Background(), kv)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all["A"] != "1" || all["B"] != "two=parts" {
		t.Fatalf("got %v", all)
	}
	if _, err := f.fetchAll(context.Background(), "/nonexistent/x.env"); err == nil {
		t.Fatal("a missing file must be an error, not an empty secret")
	}
}

func TestFileStoreReadsUnderItsDirAndWholeDirectoriesForDataFrom(t *testing.T) {
	root := t.TempDir()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, "tls", "sub"), 0o755))
	must(os.WriteFile(filepath.Join(root, "password"), []byte("pw\n"), 0o600))
	must(os.WriteFile(filepath.Join(root, "tls", "cert.pem"), []byte("CERT\r\n"), 0o600))
	must(os.WriteFile(filepath.Join(root, "tls", "key.pem"), []byte("KEY"), 0o600))
	must(os.WriteFile(filepath.Join(root, "tls", "sub", "ignored"), []byte("x"), 0o600))

	f := &fileSecretFetcher{resolver: secrets.Default(root, ""), dir: root}
	if v, err := f.fetch(context.Background(), "password", ""); err != nil || v != "pw" {
		t.Fatalf("relative key under dir: got %q %v", v, err)
	}
	all, err := f.fetchAll(context.Background(), "tls")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all["cert.pem"] != "CERT" || all["key.pem"] != "KEY" {
		t.Fatalf("one key per file, trailing newlines trimmed, subdirectories skipped: %v", all)
	}
	// An absolute key ignores dir.
	abs := filepath.Join(t.TempDir(), "elsewhere")
	must(os.MkdirAll(abs, 0o755))
	must(os.WriteFile(filepath.Join(abs, "k"), []byte("v"), 0o600))
	if all, err := f.fetchAll(context.Background(), abs); err != nil || all["k"] != "v" {
		t.Fatalf("absolute key: got %v %v", all, err)
	}
	if _, err := f.fetchAll(context.Background(), "missing"); err == nil {
		t.Fatal("a missing directory must be an error")
	}
}

func TestParseKVPairs(t *testing.T) {
	got := parseKVPairs("  # c\nK=V\n\n X = spaced \nnovalue\nE=\n")
	want := map[string]string{"K": "V", "X": "spaced", "E": ""}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}
