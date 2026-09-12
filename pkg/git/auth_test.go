package git

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
)

// authHeader finds the Authorization header podcd put into the git
// environment, whatever GIT_CONFIG index it landed on (the surrounding
// environment may already carry entries of its own).
func authHeader(t *testing.T, env []string) (key, value string) {
	t.Helper()
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GIT_CONFIG_KEY_") && strings.HasSuffix(v, ".extraheader") {
			idx := strings.TrimPrefix(k, "GIT_CONFIG_KEY_")
			for _, kv2 := range env {
				if val, ok := strings.CutPrefix(kv2, "GIT_CONFIG_VALUE_"+idx+"="); ok {
					return v, val
				}
			}
		}
	}
	return "", ""
}

func TestTokenTravelsInTheEnvironmentNotTheCommandLineOrDisk(t *testing.T) {
	r := New("infra", "https://gitlab.com/acme/gitops.git", "main", "", t.TempDir())
	r.Auth = Auth{Token: "glpat-secret"}

	key, value := authHeader(t, r.env())
	if key != "http.https://gitlab.com/acme/gitops.git.extraheader" {
		t.Fatalf("the header must be scoped to the repository URL, got key %q", key)
	}
	basic := base64.StdEncoding.EncodeToString([]byte("oauth2:glpat-secret"))
	if value != "Authorization: Basic "+basic {
		t.Fatalf("expected a Basic header for oauth2:token, got %q", value)
	}
	if strings.Contains(strings.Join(r.env(), "\n"), "GIT_ASKPASS=/") {
		t.Error("no askpass program should be involved")
	}

	r.URL = "https://github.com/acme/gitops.git"
	if _, v := authHeader(t, r.env()); !strings.HasSuffix(v, base64.StdEncoding.EncodeToString([]byte("x-access-token:glpat-secret"))) {
		t.Error("github.com should default to the x-access-token username")
	}
	r.Auth.Username = "gitlab+deploy-token-42"
	if _, v := authHeader(t, r.env()); !strings.HasSuffix(v, base64.StdEncoding.EncodeToString([]byte("gitlab+deploy-token-42:glpat-secret"))) {
		t.Error("a configured username must win over the default")
	}

	// Existing GIT_CONFIG_* entries in the agent's environment are kept and
	// ours is appended after them, not written over them.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "safe.bareRepository")
	t.Setenv("GIT_CONFIG_VALUE_0", "explicit")
	env := strings.Join(r.env(), "\n") + "\n"
	for _, want := range []string{"GIT_CONFIG_COUNT=2\n", "GIT_CONFIG_KEY_0=safe.bareRepository\n", "GIT_CONFIG_KEY_1=http.https://github.com/acme/gitops.git.extraheader\n"} {
		if !strings.Contains(env, want) {
			t.Errorf("inherited git config was not preserved, missing %q:\n%s", want, env)
		}
	}
	if strings.Count(env, "GIT_CONFIG_COUNT=") != 1 {
		t.Errorf("GIT_CONFIG_COUNT must appear exactly once:\n%s", env)
	}

	r.Auth = Auth{}
	if k, _ := authHeader(t, r.env()); k != "" {
		t.Error("no auth, no header")
	}
}

func TestSSHKeyIsPinnedWithIdentitiesOnly(t *testing.T) {
	r := New("infra", "git@github.com:acme/gitops.git", "main", "", t.TempDir())
	r.Auth = Auth{SSHKeyPath: "/home/podcd/.ssh/deploy key", SSHKnownHostsPath: "/home/podcd/.ssh/known_hosts"}
	env := strings.Join(r.env(), "\n")
	for _, want := range []string{
		"IdentitiesOnly=yes", "-i '/home/podcd/.ssh/deploy key'", "UserKnownHostsFile='/home/podcd/.ssh/known_hosts'", "BatchMode=yes",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("GIT_SSH_COMMAND is missing %q:\n%s", want, env)
		}
	}
}

// TestGitSendsTheTokenToTheServer is the real test: git itself, given only the
// environment podcd builds, must present the Authorization header to the
// remote. The server records what it saw; whether the clone then succeeds is
// beside the point.
func TestGitSendsTheTokenToTheServer(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	var seen atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		seen.Store(req.Header.Get("Authorization"))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	r := New("infra", srv.URL+"/acme/gitops.git", "main", "", t.TempDir())
	r.Auth = Auth{Username: "deploy", Token: "s3cret"}
	_, _ = r.Sync(context.Background()) // fails: the server answers 401 to everything

	got, _ := seen.Load().(string)
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("deploy:s3cret"))
	if got != want {
		t.Fatalf("git sent Authorization %q, want %q", got, want)
	}
}

// TestGitDoesNotLeakTheTokenToAnotherHost: the header is scoped to the
// repository URL, so a different origin must not receive it.
func TestGitDoesNotLeakTheTokenToAnotherHost(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	var seen atomic.Value
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		seen.Store(req.Header.Get("Authorization"))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer other.Close()

	// The token is scoped to gitlab.com; the host actually contacted is another.
	scoped := New("infra", "https://gitlab.com/acme/gitops.git", "main", "", t.TempDir())
	scoped.Auth = Auth{Token: "s3cret"}
	cmd := exec.Command("git", "ls-remote", other.URL+"/acme/gitops.git")
	cmd.Env = scoped.env()
	_ = cmd.Run()

	if got, _ := seen.Load().(string); got != "" {
		t.Fatalf("a header scoped to gitlab.com reached another host: %q", got)
	}
}
