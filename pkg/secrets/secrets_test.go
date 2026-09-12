package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestEnvProviderPrefersTheProcessEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.env")
	if err := os.WriteFile(path, []byte("TOKEN=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOKEN", "from-env")
	got, err := (EnvProvider{File: path}).Resolve(context.Background(), "TOKEN")
	if err != nil || got != "from-env" {
		t.Fatalf("Resolve() = %q, %v; want the process environment to win", got, err)
	}
}

func TestEnvProviderReadsTheEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.env")
	content := "# secrets for this host\nexport DB_PASSWORD='quoted value'\nPLAIN=v=with=equals\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	p := EnvProvider{File: path}
	for key, want := range map[string]string{"DB_PASSWORD": "quoted value", "PLAIN": "v=with=equals"} {
		got, err := p.Resolve(context.Background(), key)
		if err != nil || got != want {
			t.Errorf("Resolve(%s) = %q, %v; want %q", key, got, err, want)
		}
	}

	// Rotation is seen without restarting: the file is read on every lookup.
	if err := os.WriteFile(path, []byte("DB_PASSWORD=rotated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.Resolve(context.Background(), "DB_PASSWORD"); got != "rotated" {
		t.Errorf("after rotation Resolve() = %q, want rotated", got)
	}
}

func TestEnvProviderMissingIsNotFound(t *testing.T) {
	p := EnvProvider{File: filepath.Join(t.TempDir(), "does-not-exist.env")}
	_, err := p.Resolve(context.Background(), "NOPE_NOT_SET_12345")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound for a missing key and a missing file, got %v", err)
	}
}
