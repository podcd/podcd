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
		"    # auth:\n    #   username: gitlab+deploy-token-42\n    #   token: env:GITOPS_TOKEN",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing example block:\n%s\n--- in ---\n%s", want, out)
		}
	}
}

func TestRenderedFileRoundTrips(t *testing.T) {
	cfg := minimal()
	cfg.Host = "vm-1"
	cfg.Interval = 30 * time.Second
	cfg.Repositories[0].Auth = &RepoAuth{Username: "deploy", Token: "env:T"}

	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := WriteAgentConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	back, err := LoadAgentConfig(path)
	if err != nil {
		t.Fatalf("the rendered file does not load: %v", err)
	}
	back.Path = ""
	if back.Host != "vm-1" || back.Interval != 30*time.Second || back.Repositories[0].Auth.Token != "env:T" {
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
	if !strings.Contains(got, "    revision: main\n    path: clusters/prod\n") {
		t.Errorf("path should follow the repository's last key:\n%s", got)
	}
	cfg, err := ParseAgentConfig(out)
	if err != nil || cfg.Jitter.String() != "5s" || cfg.Repositories[0].Path != "clusters/prod" {
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

func TestEditCreatesAndGrowsAScalarSequence(t *testing.T) {
	src := "host: vm-1\nrepositories:\n  - name: r\n    url: https://example.com/r.git\n    revision: main\n    path: multi-env\n"

	// First set creates the sequence from nothing.
	out, err := EditAgentConfigBytes([]byte(src), Setting{"repositories.0.values.0", "values/common.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	want := "host: vm-1\nrepositories:\n  - name: r\n    url: https://example.com/r.git\n    revision: main\n    path: multi-env\n    values:\n      - values/common.yaml\n"
	if string(out) != want {
		t.Fatalf("--- got ---\n%s--- want ---\n%s", out, want)
	}

	// Two more sets append to it, one line each, exactly the sequence a
	// user runs from the CLI: repositories.0.values.0, .1, .2 in turn.
	out, err = EditAgentConfigBytes(out, Setting{"repositories.0.values.1", "values/zones/dmz.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	out, err = EditAgentConfigBytes(out, Setting{"repositories.0.values.2", "values/envs/prd.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	want = "host: vm-1\nrepositories:\n  - name: r\n    url: https://example.com/r.git\n    revision: main\n    path: multi-env\n    values:\n      - values/common.yaml\n      - values/zones/dmz.yaml\n      - values/envs/prd.yaml\n"
	if string(out) != want {
		t.Fatalf("--- got ---\n%s--- want ---\n%s", out, want)
	}

	cfg, err := ParseAgentConfig(out)
	if err != nil {
		t.Fatalf("edited config does not parse: %v", err)
	}
	if got := strings.Join(cfg.Repositories[0].Values, ","); got != "values/common.yaml,values/zones/dmz.yaml,values/envs/prd.yaml" {
		t.Fatalf("values = %v", cfg.Repositories[0].Values)
	}
}

func TestEditStillRefusesANewRepositoryByIndex(t *testing.T) {
	// A sequence of mappings is a different story: repositories.1.url with
	// only one repository configured must still be rejected, not silently
	// produce a broken new list entry.
	src := "host: vm-1\nrepositories:\n  - name: r\n    url: https://example.com/r.git\n    revision: main\n"
	if _, err := EditAgentConfigBytes([]byte(src), Setting{"repositories.1.url", "https://example.com/other.git"}); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("want a \"does not exist\" error, got %v", err)
	}
}

func TestEditLeavesTheFileAloneOnAnyError(t *testing.T) {
	src := RenderAgentConfig(minimal())
	for _, tc := range []struct{ path, value, want string }{
		{"nonsense", "x", `no field "nonsense"`},
		{"interval", "soon", "not a duration"},
		{"prune", "maybe", "not true or false"},
		{"repositories.1.url", "u", "does not exist"},
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
