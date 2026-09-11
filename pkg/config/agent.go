package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// RepositorySpec is one Git repository the agent pulls.
type RepositorySpec struct {
	Name     string `yaml:"name"`
	URL      string `yaml:"url"`
	Revision string `yaml:"revision"`       // branch, tag or commit sha
	Path     string `yaml:"path,omitempty"` // optional subdirectory holding the YAML
	// Insecure disables host key / TLS verification. Present so it is a visible,
	// auditable choice rather than an undocumented environment variable.
	Insecure bool `yaml:"insecure,omitempty"`
}

// AgentConfig is the agent's own configuration:
// * where Git and who this host is,
// * how often to reconcile.
// It lives on the host, not in Git, because it is what tells the host which Git to trust.
type AgentConfig struct {
	// Host overrides the detected identity. Empty means "use the hostname".
	Host string `yaml:"host,omitempty"`

	// Interval between reconcile attempts. Default 60s.
	Interval time.Duration `yaml:"interval,omitempty"`
	// Jitter spreads a fleet out so a hundred VMs do not hit Git in lockstep.
	Jitter time.Duration `yaml:"jitter,omitempty"`
	// RetryInterval is the wait after a failed reconcile, before backoff. Default 15s.
	RetryInterval time.Duration `yaml:"retryInterval,omitempty"`
	// MaxRetryInterval caps the exponential backoff. Default 10m.
	MaxRetryInterval time.Duration `yaml:"maxRetryInterval,omitempty"`

	// Runtime selects the container runtime. Only "podman" is implemented.
	Runtime string `yaml:"runtime,omitempty"`

	Repositories []RepositorySpec `yaml:"repositories"`

	// StateDir holds the checkouts, the local metadata and the lock.
	StateDir string `yaml:"stateDir,omitempty"`
	// UnitDir is where Quadlet units are written. Default ~/.config/containers/systemd.
	UnitDir string `yaml:"unitDir,omitempty"`
	// SecretsDir is the root for relative file: secret references.
	SecretsDir string `yaml:"secretsDir,omitempty"`
	// EnvFile is the host-local file containing KEY=value secrets for env: references.
	EnvFile string `yaml:"envFile,omitempty"`

	// Prune removes managed applications that Git no longer declares. On by
	// default: leaving orphans running is its own kind of drift.
	Prune *bool `yaml:"prune,omitempty"`

	// LogFormat is "text" (default) or "json".
	LogFormat string `yaml:"logFormat,omitempty"`

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

// LockPath is the reconcile lock, so two agents (or an agent and a human
// running `podcd reconcile`) never apply at the same time.
func (c AgentConfig) LockPath() string { return filepath.Join(c.StateDir, "reconcile.lock") }

// StatePath is the local metadata file.
func (c AgentConfig) StatePath() string { return filepath.Join(c.StateDir, "state.json") }

// SecretEnvDir holds the per-application env files referenced by the units.
func (c AgentConfig) SecretEnvDir() string { return filepath.Join(c.StateDir, "env") }

// KubeDir holds the played manifests referenced by .kube units.
func (c AgentConfig) KubeDir() string { return filepath.Join(c.StateDir, "kube") }

// LoadAgentConfig reads the agent configuration from path, applying defaults.
func LoadAgentConfig(path string) (AgentConfig, error) {
	cfg := DefaultAgentConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("reading agent config %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("parsing agent config %s: %w", path, err)
	}
	cfg.Path = path
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("agent config %s: %w", path, err)
	}
	return cfg, nil
}

// FindAgentConfig returns the first configuration file that exists, looking at
// $PODCD_CONFIG, then the user config dir, then /etc/podcd.
func FindAgentConfig() (string, error) {
	var candidates []string
	if p := os.Getenv("PODCD_CONFIG"); p != "" {
		candidates = append(candidates, p)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".config", "podcd", "agent.yaml"))
	}
	candidates = append(candidates, "/etc/podcd/agent.yaml")
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
	if c.Runtime == "" {
		c.Runtime = d.Runtime
	}
	if c.StateDir == "" {
		c.StateDir = d.StateDir
	}
	if c.UnitDir == "" {
		c.UnitDir = d.UnitDir
	}
	if c.EnvFile == "" {
		c.EnvFile = d.EnvFile
	}
	if c.LogFormat == "" {
		c.LogFormat = d.LogFormat
	}
	c.StateDir = expandPath(c.StateDir)
	c.UnitDir = expandPath(c.UnitDir)
	c.SecretsDir = expandPath(c.SecretsDir)
	c.EnvFile = expandPath(c.EnvFile)
	for i := range c.Repositories {
		if c.Repositories[i].Revision == "" {
			c.Repositories[i].Revision = "main"
		}
		c.Repositories[i].URL = expandPath(c.Repositories[i].URL)
	}
}

// Validate rejects configurations that cannot work, loudly and all at once.
func (c AgentConfig) Validate() error {
	var problems []error
	if len(c.Repositories) == 0 {
		problems = append(problems, errors.New("no repositories configured: the agent has nothing to reconcile against"))
	}
	seen := map[string]bool{}
	for i, r := range c.Repositories {
		switch {
		case r.Name == "":
			problems = append(problems, fmt.Errorf("repository %d has no name", i))
		case !validName(r.Name):
			problems = append(problems, fmt.Errorf("repository name %q must be lowercase letters, digits and dashes", r.Name))
		case seen[r.Name]:
			problems = append(problems, fmt.Errorf("repository %q is listed twice", r.Name))
		default:
			seen[r.Name] = true
		}
		if r.URL == "" {
			problems = append(problems, fmt.Errorf("repository %q has no url", r.Name))
		}
		if filepath.IsAbs(r.Path) {
			problems = append(problems, fmt.Errorf("repository %q: path %q must be relative to the repository root", r.Name, r.Path))
		}
		if strings.Contains(r.Path, "..") {
			problems = append(problems, fmt.Errorf("repository %q: path %q must not escape the repository", r.Name, r.Path))
		}
	}
	if c.Runtime != "podman" && c.Runtime != "docker" {
		problems = append(problems, fmt.Errorf("runtime %q is not known (podman, docker)", c.Runtime))
	}
	if c.LogFormat != "text" && c.LogFormat != "json" {
		problems = append(problems, fmt.Errorf("logFormat %q must be text or json", c.LogFormat))
	}
	return errors.Join(problems...)
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
	home, err := os.UserHomeDir()
	if err == nil {
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

// expandPath expands a leading ~ and any $VARs, so configs can be written once
// for a fleet without knowing the agent user's home directory.
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
