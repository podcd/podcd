package cli

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// requirePodman skips a test that reaches the real podman binary. Call it
// after agentConfig, so the check runs under the same HOME the test does.
//
// That ordering is the whole point. These commands inspect the host before they
// do anything, and there is no seam to fake that through the CLI, so the test
// needs a podman that answers. On Linux a fresh HOME just yields an empty
// rootless podman and everything runs; on macOS the machine connection lives in
// HOME, so pointing HOME at a temp dir hides the VM and podman cannot answer at
// all. Checking before the HOME swap would pass and then fail in the test.
func requirePodman(t *testing.T) {
	t.Helper()
	// A skip that CI honours is a test that quietly stops running. CI has
	// podman, so there it is a failure.
	give := t.Skipf
	if os.Getenv("CI") != "" {
		give = t.Fatalf
	}
	if _, err := exec.LookPath("podman"); err != nil {
		give("podman is not installed: %v", err)
		return
	}
	if out, err := exec.Command("podman", "ps", "--format", "json").CombinedOutput(); err != nil {
		give("podman cannot answer under this test's HOME: %s", bytes.TrimSpace(out))
	}
}

// agentConfig writes a minimal agent config under a fresh HOME and returns
// that HOME, so `setup()` finds it the same way a real invocation would.
func agentConfig(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, v := range []string{"PODCD_CONFIG", "PODCD_HOST", "XDG_STATE_HOME", "XDG_CONFIG_HOME"} {
		t.Setenv(v, "")
	}
	if out, code := run(t, "config", "create", "--repo-url", "https://example.com/repo.git"); code != 0 {
		t.Fatalf("config create failed: %s", out)
	}
	return home
}

func TestRemoveRequiresNamesOrAll(t *testing.T) {
	agentConfig(t)
	out, code := run(t, "remove")
	if code == 0 {
		t.Fatalf("remove with nothing to act on should fail: %s", out)
	}
}

func TestRemoveHasTheRmAlias(t *testing.T) {
	agentConfig(t)
	out, code := run(t, "rm")
	if code == 0 || !strings.Contains(out, "name one or more applications") {
		t.Fatalf("rm should be remove: %d %s", code, out)
	}
}

func TestRemoveWithNoManagedApplicationsIsANoOp(t *testing.T) {
	agentConfig(t)
	requirePodman(t)
	out, code := run(t, "remove", "--all")
	if code != 0 {
		t.Fatalf("remove --all exit code = %d: %s", code, out)
	}
	if out != "nothing to remove\n" {
		t.Fatalf("got %q", out)
	}
}

func TestRemoveReportsAnUnknownNameWithoutAsking(t *testing.T) {
	agentConfig(t)
	requirePodman(t)
	// -y skips the confirmation; there is still nothing on the host by that name.
	out, code := run(t, "remove", "never-existed", "-y")
	if code != 0 {
		t.Fatalf("remove exit code = %d: %s", code, out)
	}
	if !strings.Contains(out, "not found: never-existed") {
		t.Fatalf("got %q", out)
	}
}

func TestRemoveAsksBeforeActingWithoutYes(t *testing.T) {
	agentConfig(t)
	requirePodman(t)
	// stdin is not a terminal here, so the prompt reads EOF: that is a "no".
	out, code := run(t, "remove", "never-existed")
	if code != 0 {
		t.Fatalf("an aborted remove is not a failure: %d: %s", code, out)
	}
	if !strings.Contains(out, "aborted") || strings.Contains(out, "not found") {
		t.Fatalf("remove should have stopped at the confirmation prompt: %q", out)
	}
}

// prune takes no names: what to remove is Git's decision, not the caller's.
func TestPruneTakesNoArguments(t *testing.T) {
	agentConfig(t)
	out, code := run(t, "prune", "something")
	if code == 0 {
		t.Fatalf("prune with a name should be rejected: %s", out)
	}
}
