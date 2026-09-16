package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTeardownAsksBeforeActingWithoutYes(t *testing.T) {
	agentConfig(t)
	requirePodman(t)
	// stdin is not a terminal here, so the prompt reads EOF: that is a "no".
	out, code := run(t, "teardown")
	if code != 0 {
		t.Fatalf("an aborted teardown is not a failure: %d: %s", code, out)
	}
	if !strings.Contains(out, "aborted") {
		t.Fatalf("teardown should have stopped at the confirmation prompt: %q", out)
	}
}

func TestTeardownRemovesTheServiceFile(t *testing.T) {
	home := agentConfig(t)
	requirePodman(t)
	dest := filepath.Join(home, ".config", "systemd", "user", "podcd-agent.service")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, code := run(t, "teardown", "-y")
	if code != 0 {
		t.Fatalf("teardown -y exit code = %d: %s", code, out)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("teardown -y did not remove the service file")
	}
}

func TestTeardownLeavesStateAndConfigByDefault(t *testing.T) {
	home := agentConfig(t)
	requirePodman(t)
	configPath := filepath.Join(home, ".config", "podcd", "agent.yaml")

	if out, code := run(t, "teardown", "-y"); code != 0 {
		t.Fatalf("teardown -y exit code = %d: %s", code, out)
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatal("teardown without --purge-config must leave the agent config alone")
	}
}

func TestTeardownPurgesStateAndConfigWhenAsked(t *testing.T) {
	home := agentConfig(t)
	requirePodman(t)
	configDir := filepath.Join(home, ".config", "podcd")
	stateDir := filepath.Join(home, ".local", "state", "podcd")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	out, code := run(t, "teardown", "-y", "--purge-state", "--purge-config")
	if code != 0 {
		t.Fatalf("teardown exit code = %d: %s", code, out)
	}
	if _, err := os.Stat(configDir); !os.IsNotExist(err) {
		t.Fatal("--purge-config did not remove the config directory")
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatal("--purge-state did not remove the state directory")
	}
}
