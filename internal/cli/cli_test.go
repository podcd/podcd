package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/podcd/podcd/deploy"
	"github.com/podcd/podcd/pkg/config"
)

// TestEveryCommandOwnsItsFlags guards the bug where a flag was parsed by a
// hand-rolled flag.FlagSet after cobra had already rejected (or silently
// consumed) it. Every flag a command reads must be declared on that command.
func TestEveryCommandOwnsItsFlags(t *testing.T) {
	root := newRootCommand()
	want := map[string][]string{
		"status":    {"output"},
		"plan":      {"output"},
		"reconcile": {"output", "dry-run", "no-prune"},
		"health":    {"output"},
		"logs":      {"tail"},
		"validate":  {"output"},
		"version":   {"output"},
		"install":   {"output", "yes"},
		"uninstall": {"output", "yes"},
		"prune":     {"all", "yes"},
		"teardown":  {"purge-state", "purge-config", "yes"},
	}
	for name, flags := range want {
		cmd, _, err := root.Find([]string{name})
		if err != nil || cmd.Name() != name {
			t.Fatalf("command %q not found: %v", name, err)
		}
		for _, f := range flags {
			if cmd.Flags().Lookup(f) == nil {
				t.Errorf("%s: flag --%s is not declared on the command", name, f)
			}
		}
	}
	if plan, _, _ := root.Find([]string{"plan"}); plan.Flags().ShorthandLookup("o") == nil {
		t.Error("plan: -o shorthand is missing")
	}
	if logs, _, _ := root.Find([]string{"logs"}); logs.Flags().ShorthandLookup("n") != nil {
		t.Error("logs: -n must not be a flag")
	}
	for _, g := range []string{"config", "host", "log-level", "log-format"} {
		if root.PersistentFlags().Lookup(g) == nil {
			t.Errorf("global flag --%s is missing", g)
		}
	}
}

func TestInstallWritesTheEmbeddedServiceFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if code := Main([]string{"install"}); code != 0 {
		t.Fatalf("install exit code = %d", code)
	}
	dest := filepath.Join(home, ".config", "systemd", "user", "podcd-agent.service")
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(deploy.AgentService) {
		t.Fatal("the installed unit is not the file in deploy/")
	}
	for _, want := range []string{"ExecStart=/usr/local/bin/podcd run", "WorkingDirectory=%h", "WantedBy=default.target"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("service file is missing %q", want)
		}
	}
}

func TestInstallDoesNotOverwriteWithoutConsent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dest := filepath.Join(home, ".config", "systemd", "user", "podcd-agent.service")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// stdin is not a terminal here, so the prompt reads EOF: that is a "no".
	if code := Main([]string{"install"}); code != 0 {
		t.Fatalf("install exit code = %d", code)
	}
	if got, _ := os.ReadFile(dest); string(got) != "mine\n" {
		t.Fatal("install overwrote an existing unit without consent")
	}
	if code := Main([]string{"install", "-y"}); code != 0 {
		t.Fatalf("install -y exit code = %d", code)
	}
	if got, _ := os.ReadFile(dest); string(got) == "mine\n" {
		t.Fatal("install -y did not overwrite")
	}
}

func TestConfigCreateWritesTheFullAnnotatedSpec(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PODCD_CONFIG", "")

	if code := Main([]string{"config", "create", "--repo-url", "https://example.com/repo.git", "--repo-path", "clusters/prod", "--interval", "30s"}); code != 0 {
		t.Fatalf("config create exit code = %d", code)
	}
	path := filepath.Join(home, ".config", "podcd", "agent.yaml")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := "\n" + string(got)

	// What was set is active.
	for _, want := range []string{"\n    url: https://example.com/repo.git\n", "\n    path: clusters/prod\n", "\ninterval: 30s\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("config is missing the active line %q:\n%s", want, text)
		}
	}
	// Every other field is present, as a commented default - documented, but
	// not pinning this machine's paths or the current defaults into the file.
	for _, field := range []string{"host", "jitter", "retryInterval", "maxRetryInterval", "runtime", "stateDir", "unitDir", "secretsDir", "envFile", "prune", "logFormat"} {
		if !strings.Contains(text, "\n# "+field+":") {
			t.Errorf("config should show %q as a commented default:\n%s", field, text)
		}
		if strings.Contains(text, "\n"+field+":") {
			t.Errorf("config should not activate the default %q:\n%s", field, text)
		}
	}
	for _, want := range []string{"#   token: env:GITOPS_TOKEN"} {
		if !strings.Contains(text, want) {
			t.Errorf("config should show the %q example:\n%s", want, text)
		}
	}

	// It parses back to what was asked for.
	cfg, err := config.LoadAgentConfig(path)
	if err != nil {
		t.Fatalf("the written config does not load: %v", err)
	}
	if cfg.Interval.String() != "30s" || cfg.Repositories[0].Path != "clusters/prod" || !cfg.PruneEnabled() {
		t.Errorf("round trip lost values: %+v", cfg)
	}

	// A second create refuses to clobber.
	if code := Main([]string{"config", "create", "--repo-url", "https://example.com/other.git"}); code == 0 {
		t.Fatal("config create overwrote an existing file without --force")
	}
}

func TestConfigCreateRequiresARepository(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PODCD_CONFIG", "")
	if code := Main([]string{"config", "create"}); code == 0 {
		t.Fatal("config create without --repo-url must fail")
	}
}

func TestConfigSetUpdatesOneField(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, "agent.yaml")
	t.Setenv("PODCD_CONFIG", path)

	if code := Main([]string{"config", "create", "--host", "old-host", "--repo-url", "https://example.com/a.git"}); code != 0 {
		t.Fatalf("config create exit code = %d", code)
	}
	for _, args := range [][]string{
		{"config", "set", "host", "new-host"},
		{"config", "set", "repo-url", "https://example.com/new.git"},
		{"config", "set", "prune", "false"},
	} {
		if code := Main(args); code != 0 {
			t.Fatalf("%v exit code = %d", args, code)
		}
	}
	got, _ := os.ReadFile(path)
	for _, want := range []string{"host: new-host", "url: https://example.com/new.git", "prune: false"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("config is missing %q:\n%s", want, got)
		}
	}
	if code := Main([]string{"config", "set", "interval", "not-a-duration"}); code == 0 {
		t.Fatal("an invalid value must be rejected")
	}
	if code := Main([]string{"config", "set", "nonsense", "x"}); code == 0 {
		t.Fatal("an unknown field must be rejected")
	}
}

func TestOutputFormatIsValidatedAtParseTime(t *testing.T) {
	if code := Main([]string{"version", "-o", "xml"}); code == 0 {
		t.Fatal("-o xml must be rejected")
	}
	for _, format := range []string{"json", "yaml"} {
		if code := Main([]string{"version", "-o", format}); code != 0 {
			t.Errorf("version -o %s exit code = %d", format, code)
		}
	}
}

func TestHelpHidesGlobalFlags(t *testing.T) {
	root := newRootCommand()
	var buf strings.Builder
	root.SetOut(&buf)
	root.SetArgs([]string{"install", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	help := buf.String()
	if strings.Contains(help, "Global Flags") || strings.Contains(help, "--log-level") {
		t.Fatalf("install --help should not list the global flags:\n%s", help)
	}
	if !strings.Contains(help, "--yes") || !strings.Contains(help, `"podcd options"`) {
		t.Fatalf("install --help should list its own flags and point at podcd options:\n%s", help)
	}

	buf.Reset()
	root.SetArgs([]string{"options"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, g := range []string{"--config", "--host", "--log-level", "--log-format"} {
		if !strings.Contains(buf.String(), g) {
			t.Errorf("podcd options should document %s:\n%s", g, buf.String())
		}
	}
	// The flags are still accepted before an unrelated subcommand.
	if code := Main([]string{"--log-level", "debug", "version"}); code != 0 {
		t.Errorf("a global flag before a subcommand must still parse, got exit %d", code)
	}
}

func TestHelpNeverFails(t *testing.T) {
	for _, args := range [][]string{
		{"--help"}, {"config", "--help"}, {"config", "create", "--help"},
		{"config", "set", "--help"}, {"install", "--help"}, {"reconcile", "--help"},
	} {
		if code := Main(args); code != 0 {
			t.Errorf("%v exit code = %d", args, code)
		}
	}
	if code := Main(nil); code != 0 {
		t.Errorf("bare podcd should print help and exit 0, got %d", code)
	}
}
