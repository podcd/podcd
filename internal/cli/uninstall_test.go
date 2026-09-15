package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUninstallWithNothingInstalledIsANoOp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if code := Main([]string{"uninstall"}); code != 0 {
		t.Fatalf("uninstall exit code = %d", code)
	}
}

func TestUninstallRemovesTheServiceFile(t *testing.T) {
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
	if code := Main([]string{"uninstall"}); code != 0 {
		t.Fatalf("uninstall exit code = %d", code)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatal("uninstall removed the service file without consent")
	}

	if code := Main([]string{"uninstall", "-y"}); code != 0 {
		t.Fatalf("uninstall -y exit code = %d", code)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("uninstall -y did not remove the service file")
	}
}
