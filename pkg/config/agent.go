package config

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/podcd/podcd/pkg/secrets"
)

// The agent configuration is described once, here, on the struct.
// The yaml tag is the key, the doc tag is the explanation a user sees in the file, and the example tag shows how an optional section is filled in.
// The rendered agent.yaml, `podcd config set`, and the validation messages all derive from these tags, so there is no second list of fields anywhere.

// RepositorySpec is one Git repository the agent pulls.
type RepositorySpec struct {
	Name     string    `yaml:"name,omitempty" default:"infrastructure" doc:"A short name for this repository; it appears in logs and status."`
	URL      string    `yaml:"url" doc:"Where to fetch from: https://, ssh (git@host:path) or a local path."`
	Revision string    `yaml:"revision" doc:"A branch, a tag or a commit. A tag or commit pins the host."`
	Path     string    `yaml:"path,omitempty" doc:"Read only this subdirectory of the repository."`
	Values   []string  `yaml:"values,omitempty" doc:"Values file(s), relative to this repository's tree (path, if set), for {{ .Values }} templating of *.tpl files - a host-local fallback beneath what Host, Group and Environment documents declare. Repeated files merge, later ones winning per key."`
	Insecure bool      `yaml:"insecure,omitempty" doc:"Disable host key / TLS verification. Visible here on purpose, rather than an environment variable nobody sees."`
	Auth     *RepoAuth `yaml:"auth,omitempty" doc:"A private repository needs a read credential. The token is a secret reference (env: or file:), never a literal in this file; it is resolved on every fetch. GitLab deploy tokens have their own username."`
}

// RepoAuth is a repository's read credential.
//
// HTTPS uses a token, which must be a secret reference (env:NAME, file:path, vault:...) because agent.yaml is world-readable.
// The value is resolved on every fetch and reaches git as an Authorization header via git's configuration-in-environment, scoped to this repository's URL.
type RepoAuth struct {
	Username          string `yaml:"username,omitempty" example:"gitlab+deploy-token-42" doc:"Username for HTTPS. Needed for GitLab deploy tokens; access tokens work with the default (x-access-token on github.com, oauth2 elsewhere)."`
	Token             string `yaml:"token,omitempty" example:"env:GITOPS_TOKEN" doc:"A secret reference to the token, resolved at fetch time."`
	SSHKeyPath        string `yaml:"sshKeyPath,omitempty" example:"~/.ssh/deploy_key" doc:"Instead of a token: a private key on this host, used with IdentitiesOnly."`
	SSHKnownHostsPath string `yaml:"sshKnownHostsPath,omitempty" example:"~/.ssh/known_hosts" doc:"Overrides ~/.ssh/known_hosts for host key checks."`
}

// AgentConfig is the agent's own configuration: where Git is, who this host is, how often to reconcile.
// It lives on the host, not in Git, because it is what tells the host which Git to trust.
type AgentConfig struct {
	Host string `yaml:"host,omitempty" doc:"Which Host document this machine is. Empty means the system hostname (with any domain stripped). PODCD_HOST in the environment overrides both."`

	Interval         time.Duration `yaml:"interval,omitempty" doc:"How often to pull and reconcile."`
	Jitter           time.Duration `yaml:"jitter,omitempty" doc:"Random extra delay on top of interval, so a fleet does not hit Git in lockstep."`
	RetryInterval    time.Duration `yaml:"retryInterval,omitempty" doc:"Wait after a failed reconcile; doubles on each further failure."`
	MaxRetryInterval time.Duration `yaml:"maxRetryInterval,omitempty" doc:"Cap for the retry backoff."`

	Runtime string `yaml:"runtime,omitempty" doc:"Container runtime. Only podman (rootless, via Quadlet) is implemented."`

	Repository RepositorySpec `yaml:"repository" doc:"The Git repository this host reconciles against."`

	StateDir   string `yaml:"stateDir,omitempty" doc:"Where the agent keeps checkouts, played manifests and state.json."`
	UnitDir    string `yaml:"unitDir,omitempty" doc:"Where Quadlet units are written. Must be a directory systemd --user reads."`
	SecretsDir string `yaml:"secretsDir,omitempty" doc:"Root for relative file: secret references."`
	EnvFile    string `yaml:"envFile,omitempty" doc:"KEY=value file read for env: references (and loaded by the systemd unit). Re-read on every lookup so rotation needs no restart."`

	Prune *bool `yaml:"prune,omitempty" doc:"Remove applications that Git no longer declares. On by default; leaving orphans running is its own kind of drift."`

	LogFormat string `yaml:"logFormat,omitempty" doc:"text or json."`

	// Path this config was read from, for diagnostics.
	Path string `yaml:"-"`
}

// DefaultAgentConfig returns the configuration used when nothing is set.
func DefaultAgentConfig() AgentConfig {
	prune := true
	return AgentConfig{
		Interval:         60 * time.Second,
		Jitter:           10 * time.Second,
		RetryInterval:    15 * time.Second,
		MaxRetryInterval: 10 * time.Minute,
		Runtime:          "podman",
		StateDir:         defaultStateDir(),
		UnitDir:          defaultUnitDir(),
		SecretsDir:       "",
		EnvFile:          defaultEnvFilePath(),
		Prune:            &prune,
		LogFormat:        "text",
	}
}

// PruneEnabled reports whether orphaned applications should be removed.
func (c AgentConfig) PruneEnabled() bool { return c.Prune == nil || *c.Prune }

// ReposDir is where repository checkouts live.
func (c AgentConfig) ReposDir() string { return filepath.Join(c.StateDir, "repos") }

// LockPath is the reconcile lock, so two agents (or an agent and a human running `podcd reconcile`) never apply at the same time.
func (c AgentConfig) LockPath() string { return filepath.Join(c.StateDir, "reconcile.lock") }

// StatePath is the local metadata file.
func (c AgentConfig) StatePath() string { return filepath.Join(c.StateDir, "state.json") }

// KubeDir holds the played manifests referenced by .kube units.
func (c AgentConfig) KubeDir() string { return filepath.Join(c.StateDir, "kube") }

// LoadAgentConfig reads the agent configuration from path, applying defaults.
func LoadAgentConfig(path string) (AgentConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return DefaultAgentConfig(), fmt.Errorf("reading agent config %s: %w", path, err)
	}
	cfg, err := ParseAgentConfig(data)
	if err != nil {
		return cfg, fmt.Errorf("agent config %s: %w", path, err)
	}
	cfg.Path = path
	return cfg, nil
}

// ParseAgentConfig decodes, defaults and validates one configuration document.
func ParseAgentConfig(data []byte) (AgentConfig, error) {
	cfg := DefaultAgentConfig()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return cfg, err
	}
	cfg.applyDefaults()
	return cfg, cfg.Validate()
}

// ConfigCandidates lists where the agent config may live, in lookup order: $PODCD_CONFIG, the user's config dir, then /etc/podcd.
func ConfigCandidates() []string {
	var candidates []string
	if p := os.Getenv("PODCD_CONFIG"); p != "" {
		candidates = append(candidates, p)
	}
	if home := userHome(); home != "" {
		candidates = append(candidates, filepath.Join(home, ".config", "podcd", "agent.yaml"))
	}
	return append(candidates, "/etc/podcd/agent.yaml")
}

// DefaultConfigPath is where `podcd config create` writes: the first candidate, whether or not it exists yet.
func DefaultConfigPath() string { return ConfigCandidates()[0] }

// FindAgentConfig returns the first configuration file that exists.
func FindAgentConfig() (string, error) {
	candidates := ConfigCandidates()
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("no agent config found (looked in: %s)", strings.Join(candidates, ", "))
}

func (c *AgentConfig) applyDefaults() {
	d := DefaultAgentConfig()
	if c.Interval <= 0 {
		c.Interval = d.Interval
	}
	if c.Jitter < 0 {
		c.Jitter = 0
	}
	if c.RetryInterval <= 0 {
		c.RetryInterval = d.RetryInterval
	}
	if c.MaxRetryInterval <= 0 {
		c.MaxRetryInterval = d.MaxRetryInterval
	}
	c.Runtime = cmp.Or(c.Runtime, d.Runtime)
	c.LogFormat = cmp.Or(c.LogFormat, d.LogFormat)
	c.StateDir = expandPath(cmp.Or(c.StateDir, d.StateDir))
	c.UnitDir = expandPath(cmp.Or(c.UnitDir, d.UnitDir))
	c.EnvFile = expandPath(cmp.Or(c.EnvFile, d.EnvFile))
	c.SecretsDir = expandPath(c.SecretsDir)
	r := &c.Repository
	r.Name = cmp.Or(r.Name, "infrastructure")
	r.Revision = cmp.Or(r.Revision, "main")
	r.URL = expandPath(r.URL)
	if a := r.Auth; a != nil {
		a.SSHKeyPath = expandPath(a.SSHKeyPath)
		a.SSHKnownHostsPath = expandPath(a.SSHKnownHostsPath)
	}
}

// Validate rejects configurations that cannot work, loudly and all at once.
func (c AgentConfig) Validate() error {
	var p problems
	r := c.Repository
	if r.URL == "" {
		p.add("no repository configured (repository.url is empty): the agent has nothing to reconcile against")
	} else {
		if !validName(r.Name) {
			p.add("repository name %q must be lowercase letters, digits and dashes", r.Name)
		}
		if filepath.IsAbs(r.Path) {
			p.add("repository %q: path %q must be relative to the repository root", r.Name, r.Path)
		}
		if strings.Contains(r.Path, "..") {
			p.add("repository %q: path %q must not escape the repository", r.Name, r.Path)
		}
		for _, v := range r.Values {
			if filepath.IsAbs(v) {
				p.add("repository %q: values file %q must be relative to the repository root", r.Name, v)
			}
			if strings.Contains(v, "..") {
				p.add("repository %q: values file %q must not escape the repository", r.Name, v)
			}
		}
		if a := r.Auth; a != nil {
			if a.Token != "" && a.SSHKeyPath != "" {
				p.add("repository %q: auth has both a token and an ssh key; pick one", r.Name)
			}
			if a.Token != "" && !secrets.IsReference(a.Token) {
				p.add("repository %q: auth.token must be a secret reference such as env:GITOPS_TOKEN, not a literal (agent.yaml is not a secret store)", r.Name)
			}
			if a.Token == "" && a.SSHKeyPath == "" {
				p.add("repository %q: auth needs a token or an sshKeyPath", r.Name)
			}
		}
	}
	if c.Runtime != "podman" && c.Runtime != "docker" {
		p.add("runtime %q is not known (podman, docker)", c.Runtime)
	}
	if c.LogFormat != "text" && c.LogFormat != "json" {
		p.add("logFormat %q must be text or json", c.LogFormat)
	}
	return p.err()
}

// userHome is the home directory the agent's files belong in.
func userHome() string {
	if os.Getuid() == 0 {
		if name := os.Getenv("SUDO_USER"); name != "" && name != "root" {
			if u, err := user.Lookup(name); err == nil && u.HomeDir != "" {
				return u.HomeDir
			}
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

func defaultStateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "podcd")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "/var/lib/podcd"
	}
	return filepath.Join(home, ".local", "state", "podcd")
}

func defaultEnvFilePath() string {
	if p := os.Getenv("PODCD_ENV_FILE"); p != "" {
		return p
	}
	if p := os.Getenv("PODCD_CONFIG"); p != "" {
		return filepath.Join(filepath.Dir(p), "agent.env")
	}
	if home := userHome(); home != "" {
		return filepath.Join(home, ".config", "podcd", "agent.env")
	}
	return "/etc/podcd/agent.env"
}

func defaultUnitDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "containers", "systemd")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "/etc/containers/systemd"
	}
	return filepath.Join(home, ".config", "containers", "systemd")
}

// expandPath expands a leading ~ and any $VARs, so configs can be written once for a fleet without knowing the agent user's home directory.
func expandPath(p string) string {
	if p == "" {
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[2:])
		}
	}
	return os.ExpandEnv(p)
}
