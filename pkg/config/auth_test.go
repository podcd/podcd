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
repositories:
  - name: gitops
    url: https://gitlab.com/acme/gitops.git
    auth:
      token: glpat-literal-token
`)
	if err == nil || !strings.Contains(err.Error(), "must be a secret reference") {
		t.Fatalf("a literal token in agent.yaml must be refused, got: %v", err)
	}

	cfg, err := loadAgent(t, `
host: x
repositories:
  - name: gitops
    url: https://gitlab.com/acme/gitops.git
    auth:
      username: gitlab+deploy-token-42
      token: env:GITOPS_TOKEN
`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Repositories[0].Auth.Token != "env:GITOPS_TOKEN" {
		t.Fatalf("auth not loaded: %+v", cfg.Repositories[0].Auth)
	}
}

func TestRepoAuthIsTokenOrSSHNotBoth(t *testing.T) {
	_, err := loadAgent(t, `
host: x
repositories:
  - name: gitops
    url: git@github.com:acme/gitops.git
    auth:
      token: env:T
      sshKeyPath: ~/.ssh/key
`)
	if err == nil || !strings.Contains(err.Error(), "pick one") {
		t.Fatalf("want an error about both auth kinds, got: %v", err)
	}
}

func TestVaultConfigValidation(t *testing.T) {
	cases := map[string]string{
		"missing address":   "vault:\n  roleId: env:R\n  secretId: env:S\n",
		"literal role id":   "vault:\n  address: https://v\n  roleId: role-literal\n  secretId: env:S\n",
		"token and approle": "vault:\n  address: https://v\n  token: env:T\n  roleId: env:R\n  secretId: env:S\n",
		"nothing":           "vault:\n  address: https://v\n",
		"bad kv version":    "vault:\n  address: https://v\n  token: env:T\n  kvVersion: 3\n",
	}
	base := "host: x\nrepositories:\n  - name: r\n    url: https://example.com/r.git\n"
	for name, snippet := range cases {
		if _, err := loadAgent(t, base+snippet); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
	cfg, err := loadAgent(t, base+"vault:\n  address: https://vault.example.com/\n  roleId: env:VAULT_ROLE_ID\n  secretId: env:VAULT_SECRET_ID\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Vault == nil || cfg.Vault.RoleID != "env:VAULT_ROLE_ID" {
		t.Fatalf("vault config not loaded: %+v", cfg.Vault)
	}
}
