package cli

import (
	"strings"
	"testing"
)

// agentConfig writes a minimal agent config under a fresh HOME and returns
// that HOME, so `setup()` finds it the same way a real invocation would.
func agentConfig(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, v := range []string{"PODCD_CONFIG", "PODCD_ENV_FILE", "PODCD_HOST", "XDG_STATE_HOME", "XDG_CONFIG_HOME"} {
		t.Setenv(v, "")
	}
	if out, code := run(t, "config", "create", "--repo-url", "https://example.com/repo.git"); code != 0 {
		t.Fatalf("config create failed: %s", out)
	}
	return home
}

func TestPruneRequiresNamesOrAll(t *testing.T) {
	agentConfig(t)
	out, code := run(t, "prune")
	if code == 0 {
		t.Fatalf("prune with nothing to act on should fail: %s", out)
	}
}

func TestPruneWithNoManagedApplicationsIsANoOp(t *testing.T) {
	agentConfig(t)
	out, code := run(t, "prune", "--all")
	if code != 0 {
		t.Fatalf("prune --all exit code = %d: %s", code, out)
	}
	if out != "nothing to prune\n" {
		t.Fatalf("got %q", out)
	}
}

func TestPruneReportsAnUnknownNameWithoutAsking(t *testing.T) {
	agentConfig(t)
	// -y skips the confirmation; there is still nothing on the host by that name.
	out, code := run(t, "prune", "never-existed", "-y")
	if code != 0 {
		t.Fatalf("prune exit code = %d: %s", code, out)
	}
	if !strings.Contains(out, "not found: never-existed") {
		t.Fatalf("got %q", out)
	}
}

func TestPruneAsksBeforeActingWithoutYes(t *testing.T) {
	agentConfig(t)
	// stdin is not a terminal here, so the prompt reads EOF: that is a "no".
	out, code := run(t, "prune", "never-existed")
	if code != 0 {
		t.Fatalf("an aborted prune is not a failure: %d: %s", code, out)
	}
	if !strings.Contains(out, "aborted") || strings.Contains(out, "not found") {
		t.Fatalf("prune should have stopped at the confirmation prompt: %q", out)
	}
}
