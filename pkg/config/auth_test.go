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
envFile: /etc/podcd/agent.env
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
envFile: /etc/podcd/agent.env
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
envFile: /etc/podcd/agent.env
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

func TestRegistriesNeedAReferencedPasswordAndUniqueServers(t *testing.T) {
	_, err := loadAgent(t, `
host: x
envFile: /etc/podcd/agent.env
repository: {url: https://example.com/infra.git}
registries:
  - {server: registry.example.com, username: robot, password: hunter2}
  - {server: https://other.example.com, username: robot, password: env:P}
  - {server: other.example.com, username: robot, password: env:P}
  - {server: other.example.com, password: env:P}
`)
	for _, want := range []string{
		"registries[0]: password must be a secret reference",
		"registries[1]: registry server \"https://other.example.com\" must be a host[:port]",
		"registries[3]: server \"other.example.com\" is listed twice",
		"registries[3]: needs a username and password, or a secretRef",
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q in: %v", want, err)
		}
	}

	cfg, err := loadAgent(t, `
host: x
envFile: /etc/podcd/agent.env
repository: {url: https://example.com/infra.git}
registries:
  - {server: "registry.example.com:5000", username: env:REG_USER, password: env:REG_PASS}
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Registries) != 1 || cfg.Registries[0].Password != "env:REG_PASS" {
		t.Fatalf("registries not loaded: %+v", cfg.Registries)
	}
}
