package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadAgent(t *testing.T, yaml string) (AgentConfig, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return LoadAgentConfig(path)
}

func TestRepoAuthTokenMustBeAReference(t *testing.T) {
	_, err := loadAgent(t, `
host: x
repository:
  name: gitops
  url: https://gitlab.com/acme/gitops.git
  auth:
    token: glpat-literal-token
`)
	if err == nil || !strings.Contains(err.Error(), "must be a secret reference") {
		t.Fatalf("a literal token in agent.yaml must be refused, got: %v", err)
	}

	cfg, err := loadAgent(t, `
host: x
repository:
  name: gitops
  url: https://gitlab.com/acme/gitops.git
  auth:
    username: gitlab+deploy-token-42
    token: env:GITOPS_TOKEN
`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Repository.Auth.Token != "env:GITOPS_TOKEN" {
		t.Fatalf("auth not loaded: %+v", cfg.Repository.Auth)
	}
}

func TestRepoAuthIsTokenOrSSHNotBoth(t *testing.T) {
	_, err := loadAgent(t, `
host: x
repository:
  name: gitops
  url: git@github.com:acme/gitops.git
  auth:
    token: env:T
    sshKeyPath: ~/.ssh/key
`)
	if err == nil || !strings.Contains(err.Error(), "pick one") {
		t.Fatalf("want an error about both auth kinds, got: %v", err)
	}
}
