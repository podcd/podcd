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
	configPath := filepath.Join(configDir, "agent.yaml")
	stateDir := filepath.Join(home, ".local", "state", "podcd")
	if err := os.MkdirAll(filepath.Join(stateDir, "repos", "infra"), 0o755); err != nil {
		t.Fatal(err)
	}
	neighbour := filepath.Join(stateDir, "notes.txt") // as with stateDir: /srv
	for _, f := range []string{filepath.Join(stateDir, "state.json"), neighbour} {
		if err := os.WriteFile(f, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	out, code := run(t, "teardown", "-y", "--purge-state", "--purge-config")
	if code != 0 {
		t.Fatalf("teardown exit code = %d: %s", code, out)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatal("--purge-config did not remove agent.yaml")
	}
	for _, gone := range []string{"repos", "state.json"} {
		if _, err := os.Stat(filepath.Join(stateDir, gone)); !os.IsNotExist(err) {
			t.Fatalf("--purge-state did not remove %s", gone)
		}
	}
	if _, err := os.Stat(neighbour); err != nil {
		t.Fatalf("--purge-state removed a file it does not own: %v", err)
	}
}

func TestTeardownPurgeConfigLeavesTheConfigsNeighboursAlone(t *testing.T) {
	home := agentConfig(t)
	requirePodman(t)
	configPath := filepath.Join(home, "agent.yaml") // as with --config ~/agent.yaml
	if err := os.Rename(filepath.Join(home, ".config", "podcd", "agent.yaml"), configPath); err != nil {
		t.Fatal(err)
	}
	neighbour := filepath.Join(home, "notes.txt")
	if err := os.WriteFile(neighbour, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}

	if out, code := run(t, "teardown", "-y", "--purge-config", "--config", configPath); code != 0 {
		t.Fatalf("teardown exit code = %d: %s", code, out)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatal("--purge-config did not remove agent.yaml")
	}
	if _, err := os.Stat(neighbour); err != nil {
		t.Fatalf("--purge-config removed a file it does not own: %v", err)
	}
}
