package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCmdInstallWritesSystemdService(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := cmdInstall(nil); err != nil {
		t.Fatalf("cmdInstall() error = %v", err)
	}

	dest := filepath.Join(home, ".config", "systemd", "user", "podcd-agent.service")
	content, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("reading installed service: %v", err)
	}
	text := string(content)
	if !strings.Contains(text, "ExecStart=/usr/local/bin/podcd-agent run") {
		t.Fatalf("service file does not contain the podcd agent command:\n%s", text)
	}
	if !strings.Contains(text, "WantedBy=default.target") {
		t.Fatalf("service file is missing the systemd install target:\n%s", text)
	}
}
