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

func TestMainInstallWithoutConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PODCD_CONFIG", "")

	if code := Main([]string{"install"}, ""); code != 0 {
		t.Fatalf("Main(install) exit code = %d; expected 0", code)
	}

	dest := filepath.Join(home, ".config", "systemd", "user", "podcd-agent.service")
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("install command did not write service file: %v", err)
	}
}

func TestMainConfigCreateWritesDefaultConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PODCD_CONFIG", "")

	if code := Main([]string{"config", "create", "--repo-url", "https://github.com/example/repo.git", "--repo-path", "clusters/prod"}, ""); code != 0 {
		t.Fatalf("Main(config create) exit code = %d; expected 0", code)
	}

	path := filepath.Join(home, ".config", "podcd", "agent.yaml")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading created config: %v", err)
	}
	text := string(content)
	if !strings.Contains(text, "url: https://github.com/example/repo.git") {
		t.Fatalf("default config did not include the repo URL:\n%s", text)
	}
	if !strings.Contains(text, "path: clusters/prod") {
		t.Fatalf("default config did not include the repo path:\n%s", text)
	}
}

func TestMainConfigCreateHelpDoesNotCrash(t *testing.T) {
	code := Main([]string{"config", "create", "--help"}, "")
	if code != 0 {
		t.Fatalf("Main(config create --help) exit code = %d; expected 0", code)
	}
}
