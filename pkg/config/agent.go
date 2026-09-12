package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
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

// ConfigCandidates lists where the agent config may live, in lookup order:
// $PODCD_CONFIG, the user's config dir, then /etc/podcd.
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

// DefaultConfigPath is where `podcd config create` writes: the first candidate,
// whether or not it exists yet.
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

// WriteAgentConfig writes cfg to path as a compact document: only the fields
// that differ from the defaults, so a file written by the CLI reads like one a
// human would write and does not pin this machine's home directory into it.
func WriteAgentConfig(path string, cfg AgentConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	d := DefaultAgentConfig()
	doc := compactAgentConfig{
		Host:         cfg.Host,
		Repositories: cfg.Repositories,
	}
	if cfg.Interval != d.Interval {
		doc.Interval = cfg.Interval.String()
	}
	if cfg.Jitter != d.Jitter {
		doc.Jitter = cfg.Jitter.String()
	}
	if cfg.RetryInterval != d.RetryInterval {
		doc.RetryInterval = cfg.RetryInterval.String()
	}
	if cfg.MaxRetryInterval != d.MaxRetryInterval {
		doc.MaxRetryInterval = cfg.MaxRetryInterval.String()
	}
	if cfg.Runtime != d.Runtime {
		doc.Runtime = cfg.Runtime
	}
	if cfg.StateDir != d.StateDir {
		doc.StateDir = cfg.StateDir
	}
	if cfg.UnitDir != d.UnitDir {
		doc.UnitDir = cfg.UnitDir
	}
	if cfg.SecretsDir != d.SecretsDir {
		doc.SecretsDir = cfg.SecretsDir
	}
	if cfg.EnvFile != d.EnvFile {
		doc.EnvFile = cfg.EnvFile
	}
	if !cfg.PruneEnabled() {
		off := false
		doc.Prune = &off
	}
	if cfg.LogFormat != d.LogFormat {
		doc.LogFormat = cfg.LogFormat
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	header := "# podcd agent configuration. This file says which Git to trust.\n# It lives on this host, not in Git.\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	return os.WriteFile(path, append([]byte(header), out...), 0o644)
}

// compactAgentConfig is AgentConfig with durations as strings and every field
// optional; it exists only so WriteAgentConfig can leave defaults out.
type compactAgentConfig struct {
	Host             string           `yaml:"host,omitempty"`
	Interval         string           `yaml:"interval,omitempty"`
	Jitter           string           `yaml:"jitter,omitempty"`
	RetryInterval    string           `yaml:"retryInterval,omitempty"`
	MaxRetryInterval string           `yaml:"maxRetryInterval,omitempty"`
	Runtime          string           `yaml:"runtime,omitempty"`
	Repositories     []RepositorySpec `yaml:"repositories"`
	StateDir         string           `yaml:"stateDir,omitempty"`
	UnitDir          string           `yaml:"unitDir,omitempty"`
	SecretsDir       string           `yaml:"secretsDir,omitempty"`
	EnvFile          string           `yaml:"envFile,omitempty"`
	Prune            *bool            `yaml:"prune,omitempty"`
	LogFormat        string           `yaml:"logFormat,omitempty"`
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

// SetValue updates a single known field on the config using the same schema and
// validation rules the loader uses.
func (c *AgentConfig) SetValue(field, value string) error {
	switch field {
	case "host":
		c.Host = value
	case "interval":
		dur, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("parse interval %q: %w", value, err)
		}
		c.Interval = dur
	case "jitter":
		dur, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("parse jitter %q: %w", value, err)
		}
		c.Jitter = dur
	case "runtime":
		c.Runtime = value
	case "log-format":
		c.LogFormat = value
	case "state-dir":
		c.StateDir = value
	case "unit-dir":
		c.UnitDir = value
	case "secrets-dir":
		c.SecretsDir = value
	case "env-file":
		c.EnvFile = value
	case "prune":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("parse prune %q: %w", value, err)
		}
		c.Prune = &b
	case "repo-url", "repo-name", "repo-path", "revision":
		// `config set` edits the first repository; a config with none gets
		// one, and Validate below insists it has a URL.
		if len(c.Repositories) == 0 {
			c.Repositories = []RepositorySpec{{Name: "infrastructure", Revision: "main"}}
		}
		repo := &c.Repositories[0]
		switch field {
		case "repo-url":
			repo.URL = value
		case "repo-name":
			repo.Name = value
		case "repo-path":
			repo.Path = value
		case "revision":
			repo.Revision = value
		}
	default:
		return fmt.Errorf("unknown config field %q (try: host, interval, jitter, runtime, log-format, state-dir, unit-dir, secrets-dir, env-file, prune, repo-url, repo-name, repo-path, revision)", field)
	}
	return c.Validate()
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

// userHome is the home directory the agent's files belong in. Under sudo that
// is the invoking user's home, looked up properly rather than guessed as
// /home/<name>, so `sudo podcd config create` does not write into /root.
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
