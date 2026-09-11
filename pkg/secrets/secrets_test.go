package secrets

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestEnvProviderLoadsValueFromAgentEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.env")
	if err := os.WriteFile(path, []byte("# secret file\nAPI_DATABASE_PASSWORD=fake\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}

	t.Setenv("PODCD_ENV_FILE", path)

	got, err := (EnvProvider{}).Resolve(context.Background(), "API_DATABASE_PASSWORD")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got != "fake" {
		t.Fatalf("Resolve() = %q, want %q", got, "fake")
	}
}
