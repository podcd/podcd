package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeVault is the slice of the Vault HTTP API the provider uses: AppRole
// login and KV reads, with tokens that can be revoked to force a re-login.
type fakeVault struct {
	t         *testing.T
	kvVersion int
	roleID    string
	secretID  string

	logins  atomic.Int32
	reads   atomic.Int32
	revoked atomic.Bool
	data    map[string]map[string]any
}

func (f *fakeVault) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/approle/login", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["role_id"] != f.roleID || body["secret_id"] != f.secretID {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.logins.Add(1)
		f.revoked.Store(false)
		_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{
			"client_token": "tok-" + strings.Repeat("x", int(f.logins.Load())), "lease_duration": 3600,
		}})
	})
	mux.HandleFunc("GET /v1/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") == "" || f.revoked.Load() {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		f.reads.Add(1)
		path := strings.TrimPrefix(r.URL.Path, "/v1/")
		if f.kvVersion == 2 {
			path = strings.Replace(path, "/data/", "/", 1)
		}
		values, ok := f.data[path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if f.kvVersion == 2 {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": values}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": values})
	})
	return mux
}

func newTestVault(t *testing.T, kvVersion int) (*fakeVault, *VaultProvider) {
	t.Helper()
	f := &fakeVault{t: t, kvVersion: kvVersion, roleID: "role", secretID: "secret",
		data: map[string]map[string]any{
			"secret/prod/api": {"DATABASE_PASSWORD": "pw-1", "PORT": 5432},
		}}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	p, err := NewVaultProvider(VaultOptions{
		Address:   srv.URL,
		RoleID:    func(context.Context) (string, error) { return "role", nil },
		SecretID:  func(context.Context) (string, error) { return "secret", nil },
		KVVersion: kvVersion,
		CacheTTL:  time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return f, p
}

func TestVaultAppRoleLoginAndKVv2Read(t *testing.T) {
	f, p := newTestVault(t, 2)
	got, err := p.Resolve(context.Background(), "secret/prod/api/DATABASE_PASSWORD")
	if err != nil || got != "pw-1" {
		t.Fatalf("Resolve() = %q, %v", got, err)
	}
	if f.logins.Load() != 1 {
		t.Errorf("want one login, got %d", f.logins.Load())
	}
	// Non-string values are returned in their JSON form rather than refused.
	if got, _ := p.Resolve(context.Background(), "secret/prod/api/PORT"); got != "5432" {
		t.Errorf("PORT = %q", got)
	}
}

func TestVaultKVv1Read(t *testing.T) {
	_, p := newTestVault(t, 1)
	got, err := p.Resolve(context.Background(), "secret/prod/api/DATABASE_PASSWORD")
	if err != nil || got != "pw-1" {
		t.Fatalf("Resolve() = %q, %v", got, err)
	}
}

func TestVaultReadsAreCachedWithinTheTTL(t *testing.T) {
	f, p := newTestVault(t, 2)
	for i := 0; i < 5; i++ {
		if _, err := p.Resolve(context.Background(), "secret/prod/api/DATABASE_PASSWORD"); err != nil {
			t.Fatal(err)
		}
	}
	if f.reads.Load() != 1 {
		t.Fatalf("five lookups of one path should be one read, got %d", f.reads.Load())
	}
}

func TestVaultReLogsInWhenTheTokenIsRevoked(t *testing.T) {
	f, p := newTestVault(t, 2)
	if _, err := p.Resolve(context.Background(), "secret/prod/api/DATABASE_PASSWORD"); err != nil {
		t.Fatal(err)
	}
	f.revoked.Store(true)
	p.cache = map[string]cacheEntry{} // force a read
	if _, err := p.Resolve(context.Background(), "secret/prod/api/DATABASE_PASSWORD"); err != nil {
		t.Fatalf("a revoked token should trigger one re-login, got %v", err)
	}
	if f.logins.Load() != 2 {
		t.Errorf("want two logins, got %d", f.logins.Load())
	}
}

func TestVaultRefSpellings(t *testing.T) {
	want := VaultRef{Mount: "secret", Path: "prod/api", Key: "DATABASE_PASSWORD"}
	for _, in := range []string{
		"secret/prod/api/DATABASE_PASSWORD",
		"prod/api/DATABASE_PASSWORD@secret",
		"/secret/prod/api/DATABASE_PASSWORD/",
	} {
		got, err := ParseVaultRef(in)
		if err != nil || got != want {
			t.Errorf("ParseVaultRef(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	// A deeper path: the last segment is still the key, nothing is guessed.
	got, err := ParseVaultRef("kv/teams/platform/db/PASSWORD")
	if err != nil || got != (VaultRef{Mount: "kv", Path: "teams/platform/db", Key: "PASSWORD"}) {
		t.Errorf("deep path: %+v, %v", got, err)
	}
	// # has no meaning: it is an ordinary character in a key or a path.
	if got, err := ParseVaultRef("secret/prod/api#KEY"); err != nil || got.Key != "api#KEY" {
		t.Errorf("# should be an ordinary character, got %+v, %v", got, err)
	}
	for _, bad := range []string{"", "secret", "secret/api", "DATABASE_PASSWORD@secret", "a/b@c@d"} {
		if _, err := ParseVaultRef(bad); err == nil {
			t.Errorf("ParseVaultRef(%q) should fail", bad)
		}
	}
}

func TestVaultAllSpellingsResolveAndShareTheCache(t *testing.T) {
	f, p := newTestVault(t, 2)
	for _, in := range []string{
		"secret/prod/api/DATABASE_PASSWORD",
		"prod/api/DATABASE_PASSWORD@secret",
	} {
		got, err := p.Resolve(context.Background(), in)
		if err != nil || got != "pw-1" {
			t.Errorf("Resolve(%q) = %q, %v", in, got, err)
		}
	}
	if f.reads.Load() != 1 {
		t.Errorf("both spellings of one secret should be one read, got %d", f.reads.Load())
	}
}

func TestVaultMissingKeyAndPathAreNotFound(t *testing.T) {
	_, p := newTestVault(t, 2)
	if _, err := p.Resolve(context.Background(), "secret/prod/api/NOPE"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing key: %v", err)
	}
	if _, err := p.Resolve(context.Background(), "secret/prod/other/X"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing path: %v", err)
	}
	if _, err := p.Resolve(context.Background(), "no-key-here"); err == nil {
		t.Error("a reference with neither mount nor key must be rejected")
	}
}

func TestVaultBadCredentialsFailLoudly(t *testing.T) {
	f, _ := newTestVault(t, 2)
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	p, _ := NewVaultProvider(VaultOptions{
		Address:  srv.URL,
		RoleID:   func(context.Context) (string, error) { return "role", nil },
		SecretID: func(context.Context) (string, error) { return "wrong", nil },
	})
	if _, err := p.Resolve(context.Background(), "secret/prod/api/DATABASE_PASSWORD"); err == nil || !strings.Contains(err.Error(), "login") {
		t.Fatalf("want a login error, got %v", err)
	}
}

func TestVaultThroughTheResolver(t *testing.T) {
	_, p := newTestVault(t, 2)
	r := NewResolver(EnvProvider{}, p)
	got, err := r.Resolve(context.Background(), "vault:secret/prod/api/DATABASE_PASSWORD")
	if err != nil || got != "pw-1" {
		t.Fatalf("Resolve() = %q, %v", got, err)
	}
}
