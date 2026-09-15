package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func minimal() AgentConfig {
	cfg := DefaultAgentConfig()
	cfg.Repositories = []RepositorySpec{{Name: "infra", URL: "https://example.com/infra.git", Revision: "main"}}
	return cfg
}

func TestRenderShowsDefaultsCommentedAndValuesActive(t *testing.T) {
	cfg := minimal()
	cfg.Host = "vm-1"
	cfg.Jitter = 5 * time.Second
	off := false
	cfg.Prune = &off
	out := "\n" + RenderAgentConfig(cfg)

	for _, active := range []string{"\nhost: vm-1\n", "\njitter: 5s\n", "\nprune: false\n", "\n  - name: infra\n", "\n    url: https://example.com/infra.git\n"} {
		if !strings.Contains(out, active) {
			t.Errorf("missing active line %q:\n%s", active, out)
		}
	}
	for _, commented := range []string{"\n# interval: 1m0s\n", "\n# runtime: podman\n", "\n# logFormat: text\n", "\n    # path: \"\"\n", "\n    # insecure: false\n"} {
		if !strings.Contains(out, commented) {
			t.Errorf("missing commented default %q:\n%s", commented, out)
		}
	}
	// Every documented field appears, and each is either active or commented.
	for _, key := range fieldKeys(cfgType()) {
		if !strings.Contains(out, "\n"+key+":") && !strings.Contains(out, "\n# "+key+":") {
			t.Errorf("field %q is not in the rendered file", key)
		}
	}
	// Docs come from the struct tags: one sentence of each should be present.
	if !strings.Contains(out, "# Which Host document this machine is.") {
		t.Error("doc tag was not rendered")
	}
}

func TestRenderUsesExampleBlocksForUnsetSections(t *testing.T) {
	out := RenderAgentConfig(minimal())
	for _, want := range []string{
		"# vault:\n#   address: https://vault.example.com\n#   roleId: env:VAULT_ROLE_ID",
		"    # auth:\n    #   username: gitlab+deploy-token-42\n    #   token: env:GITOPS_TOKEN",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing example block:\n%s\n--- in ---\n%s", want, out)
		}
	}
}

func TestRenderNestedDefaultsComeFromTags(t *testing.T) {
	cfg := minimal()
	cfg.Vault = &VaultConfig{Address: "https://v", RoleID: "env:R", SecretID: "env:S"}
	out := RenderAgentConfig(cfg)
	for _, want := range []string{"\n  address: https://v\n", "\n  # authMount: approle\n", "\n  # kvVersion: 2\n", "\n  # cacheTTL: 30s\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	cfg.Vault.KVVersion = 1
	if !strings.Contains(RenderAgentConfig(cfg), "\n  kvVersion: 1\n") {
		t.Error("a non-default nested value should be active")
	}
}

func TestRenderedFileRoundTrips(t *testing.T) {
	cfg := minimal()
	cfg.Host = "vm-1"
	cfg.Interval = 30 * time.Second
	cfg.Repositories[0].Auth = &RepoAuth{Username: "deploy", Token: "env:T"}
	cfg.Vault = &VaultConfig{Address: "https://v", Token: "env:VT", KVVersion: 1, CacheTTL: time.Minute}

	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := WriteAgentConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	back, err := LoadAgentConfig(path)
	if err != nil {
		t.Fatalf("the rendered file does not load: %v", err)
	}
	back.Path = ""
	if back.Host != "vm-1" || back.Interval != 30*time.Second || back.Repositories[0].Auth.Token != "env:T" ||
		back.Vault.KVVersion != 1 || back.Vault.CacheTTL != time.Minute || back.Vault.Token != "env:VT" {
		t.Fatalf("round trip lost values: %+v", back)
	}
	// Writing what was read back produces the same file: set is stable.
	if again := RenderAgentConfig(back); again != RenderAgentConfig(cfg) {
		t.Fatal("rendering is not stable across a load")
	}
}

// TestRenderScalarSliceIsCommentedWhenEmptyAndRoundTripsWhenSet exercises the
// "values" field on RepositorySpec, a plain []string rather than a slice of
// structs: renderStruct must not try to treat its items as structs.
func TestRenderScalarSliceIsCommentedWhenEmptyAndRoundTripsWhenSet(t *testing.T) {
	out := RenderAgentConfig(minimal())
	if !strings.Contains(out, "\n    # values: []\n") {
		t.Errorf("an unset scalar slice should render as a commented placeholder:\n%s", out)
	}

	cfg := minimal()
	cfg.Repositories[0].Values = []string{"values/zones/dmz.yaml", "values/envs/prd.yaml"}
	out = RenderAgentConfig(cfg)
	if !strings.Contains(out, "\n    values:\n      - values/zones/dmz.yaml\n      - values/envs/prd.yaml\n") {
		t.Fatalf("a set scalar slice should render its items:\n%s", out)
	}

	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := WriteAgentConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	back, err := LoadAgentConfig(path)
	if err != nil {
		t.Fatalf("the rendered file does not load: %v", err)
	}
	if strings.Join(back.Repositories[0].Values, ",") != "values/zones/dmz.yaml,values/envs/prd.yaml" {
		t.Fatalf("round trip lost values: %+v", back.Repositories[0].Values)
	}
}

func TestEditReplacesAValueInPlace(t *testing.T) {
	src := "# my notes\nhost: vm-1   # keep this comment\ninterval: 60s\nrepositories:\n  - name: r\n    url: https://example.com/r.git\n    revision: main\n"
	out, err := EditAgentConfigBytes([]byte(src), Setting{"host", "vm-2"}, Setting{"revision", "v2"})
	if err != nil {
		t.Fatal(err)
	}
	want := "# my notes\nhost: vm-2  # keep this comment\ninterval: 60s\nrepositories:\n  - name: r\n    url: https://example.com/r.git\n    revision: v2\n"
	if string(out) != want {
		t.Fatalf("edit was not surgical:\n--- got ---\n%s--- want ---\n%s", out, want)
	}
}

func TestEditAppendsToTheSectionAndNeverTouchesComments(t *testing.T) {
	src := RenderAgentConfig(minimal())
	out, err := EditAgentConfigBytes([]byte(src),
		Setting{"jitter", "5s"},
		Setting{"repositories.0.path", "clusters/prod"},
		Setting{"vault.address", "https://v"},
		Setting{"vault.roleId", "env:R"},
		Setting{"vault.secretId", "env:S"},
	)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)

	// Every line of the original is still there, unchanged: comments and
	// commented-out defaults included.
	for _, line := range strings.Split(strings.TrimRight(src, "\n"), "\n") {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("original line was changed or lost: %q", line)
		}
	}
	// The new keys were appended to their sections.
	if !strings.HasSuffix(got, "vault:\n  address: https://v\n  roleId: env:R\n  secretId: env:S\n") {
		t.Errorf("vault should be appended at the end, its keys in order:\n%s", got)
	}
	if !strings.Contains(got, "    revision: main\n    path: clusters/prod\n") {
		t.Errorf("path should follow the repository's last key:\n%s", got)
	}
	cfg, err := ParseAgentConfig(out)
	if err != nil || cfg.Jitter.String() != "5s" || cfg.Repositories[0].Path != "clusters/prod" || cfg.Vault == nil || cfg.Vault.SecretID != "env:S" {
		t.Fatalf("edited config does not parse as intended: %v %+v", err, cfg)
	}
}

func TestEditAppendsAnUnknownButValidKey(t *testing.T) {
	src := "host: vm-1\nrepositories:\n  - name: r\n    url: https://example.com/r.git\n    revision: main\n"
	out, err := EditAgentConfigBytes([]byte(src), Setting{"prune", "false"}, Setting{"repositories.0.auth.token", "env:T"})
	if err != nil {
		t.Fatal(err)
	}
	want := "host: vm-1\nrepositories:\n  - name: r\n    url: https://example.com/r.git\n    revision: main\n    auth:\n      token: env:T\nprune: false\n"
	if string(out) != want {
		t.Fatalf("--- got ---\n%s--- want ---\n%s", out, want)
	}
}

func TestEditLeavesTheFileAloneOnAnyError(t *testing.T) {
	src := RenderAgentConfig(minimal())
	for _, tc := range []struct{ path, value, want string }{
		{"nonsense", "x", `no field "nonsense"`},
		{"vault.nope", "x", `no field "nope"`},
		{"interval", "soon", "not a duration"},
		{"prune", "maybe", "not true or false"},
		{"repositories.1.url", "u", "does not exist"},
		{"vault.address", "https://v", "needs roleId and secretId"}, // valid key, invalid result
		{"repositories.0.auth.token", "literal", "must be a secret reference"},
		{"repositories", "x", "cannot set a slice"},
	} {
		path := filepath.Join(t.TempDir(), "agent.yaml")
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		err := EditAgentConfig(path, Setting{tc.path, tc.value})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("set %s=%s: want error containing %q, got %v", tc.path, tc.value, tc.want, err)
		}
		if after, _ := os.ReadFile(path); string(after) != src {
			t.Errorf("set %s=%s: the file was modified despite the error", tc.path, tc.value)
		}
	}
}
