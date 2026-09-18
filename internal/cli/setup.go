package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/podcd/podcd/deploy"
	"github.com/podcd/podcd/pkg/config"
)

// The host-setup commands. They touch files under the user's home and never
// need Git, the runtime or an agent config to already exist.

func newInstallCommand(f *configFlags) *cobra.Command {
	var output string
	var yes bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "write the systemd user service file that runs the agent",
		Long: "Writes the systemd user unit that runs the agent, from deploy/podcd-agent.service.\n" +
			"The unit is built from the agent config, so that must exist first (podcd config create):\n" +
			"the service runs `podcd run --config <that file>` and loads the agent.env the config's\n" +
			"envFile names. --config picks a config written somewhere other than the default.\n" +
			"Enable it with: systemctl --user enable --now podcd-agent.service",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dest := output
			if dest == "" {
				var err error
				if dest, err = defaultServiceFilePath(); err != nil {
					return err
				}
			}
			configPath, err := existingConfig(f)
			if err != nil {
				return fmt.Errorf("%w; the service is built from the agent config, so create one first with podcd config create (then pass --config if it is not in a default place)", err)
			}
			cfg, err := config.LoadAgentConfig(configPath)
			if err != nil {
				return err
			}
			unit, err := renderAgentService(configPath, cfg.EnvFile)
			if err != nil {
				return err
			}
			if _, err := os.Stat(dest); err == nil && !yes {
				if !confirm(cmd, dest+" already exists. Overwrite?") {
					fmt.Fprintln(cmd.ErrOrStderr(), "aborted")
					return nil
				}
			}
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return fmt.Errorf("creating %s: %w", filepath.Dir(dest), err)
			}
			if err := os.WriteFile(dest, unit, 0o644); err != nil {
				return fmt.Errorf("writing %s: %w", dest, err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), dest)
			return nil
		},
	}
	cmd.Flags().StringVar(&output, "output", "", "destination for the service file (default: ~/.config/systemd/user/podcd-agent.service)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "overwrite an existing file without asking")
	f.addConfigFlag(cmd)
	return cmd
}

// The lines of deploy/podcd-agent.service that install fills in from the
// agent config: the run command gets the config's path, and the environment
// file is the one the config's envFile names.
const (
	execStartLine       = "ExecStart=/usr/local/bin/podcd run"
	environmentFileLine = "EnvironmentFile=-%h/.config/podcd/agent.env"
)

// renderAgentService returns the agent's unit for a config file and the
// envFile it names. The config path is made absolute: the unit's
// WorkingDirectory is the home directory, not wherever install ran.
func renderAgentService(configPath, envFile string) ([]byte, error) {
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", configPath, err)
	}
	if envFile == "" {
		return nil, fmt.Errorf("%s names no envFile", abs)
	}
	unit := string(deploy.AgentService)
	for line, with := range map[string]string{
		execStartLine:       execStartLine + " --config " + abs,
		environmentFileLine: "EnvironmentFile=-" + envFile,
	} {
		if !strings.Contains(unit, line+"\n") {
			return nil, fmt.Errorf("deploy/podcd-agent.service has no %q line to fill in", line)
		}
		unit = strings.Replace(unit, line+"\n", with+"\n", 1)
	}
	return []byte(unit), nil
}

// confirm asks a yes/no question on the terminal. Anything but an explicit
// yes - including EOF from a non-interactive stdin - is a no.
func confirm(cmd *cobra.Command, question string) bool {
	fmt.Fprintf(cmd.ErrOrStderr(), "%s [y/N]: ", question)
	answer, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}

func newConfigCommand(f *configFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "view or create the agent config file",
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newConfigPathCommand(f), newConfigViewCommand(f), newConfigCreateCommand(f), newConfigSetCommand(f))
	return cmd
}

func existingConfig(f *configFlags) (string, error) {
	if f.config != "" {
		return f.config, nil
	}
	return config.FindAgentConfig()
}

func newConfigPathCommand(f *configFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "print where the agent config is (or would be) read from",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if found, err := existingConfig(f); err == nil {
				fmt.Fprintln(cmd.OutOrStdout(), found)
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), config.DefaultConfigPath())
			return nil
		},
	}
}

func newConfigViewCommand(f *configFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "view [path]",
		Short: "print the agent config file",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := ""
			if len(args) == 1 {
				path = args[0]
			} else {
				found, err := existingConfig(f)
				if err != nil {
					return err
				}
				path = found
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(data)
			return err
		},
	}
}

func newConfigCreateCommand(f *configFlags) *cobra.Command {
	var (
		path, envFile, host, repoURL, repoName, repoPath, revision string
		interval                                                   time.Duration
		force                                                      bool
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "write a new agent config file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if path == "" {
				path = f.config
			}
			if path == "" {
				path = config.DefaultConfigPath()
			}
			if _, err := os.Stat(path); err == nil && !force {
				return fmt.Errorf("%s already exists; use --force to overwrite it", path)
			}
			cfg := config.DefaultAgentConfig()
			cfg.Host = host
			if interval > 0 {
				cfg.Interval = interval
			}
			// The secrets file is written into the config, never guessed at
			// run time: beside the config unless told otherwise.
			cfg.EnvFile = envFile
			if cfg.EnvFile == "" {
				cfg.EnvFile = config.EnvFileBeside(path)
			} else if abs, err := filepath.Abs(cfg.EnvFile); err == nil {
				cfg.EnvFile = abs
			}
			cfg.Repositories = []config.RepositorySpec{{Name: repoName, URL: repoURL, Revision: revision, Path: repoPath}}
			if err := config.WriteAgentConfig(path, cfg); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), path)
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "where to write (default: the config path)")
	cmd.Flags().StringVar(&envFile, "env-file", "", "KEY=value secrets file the agent reads (default: agent.env beside the config)")
	cmd.Flags().StringVar(&host, "host", "", "this host's identity (default: the hostname)")
	cmd.Flags().StringVar(&repoURL, "repo-url", "", "Git repository URL (required)")
	cmd.Flags().StringVar(&repoName, "repo-name", "infrastructure", "repository name")
	cmd.Flags().StringVar(&repoPath, "repo-path", "", "subdirectory of the repository to read")
	cmd.Flags().StringVar(&revision, "revision", "main", "branch, tag or commit")
	cmd.Flags().DurationVar(&interval, "interval", 0, "reconcile interval (default 60s)")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing config")
	_ = cmd.MarkFlagRequired("repo-url")
	return cmd
}

func newConfigSetCommand(f *configFlags) *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "set <field> <value> | set <field>=<value> [<field>=<value>...]",
		Short: "change fields of the agent config file in place",
		Long: "Edits the file where it is: an existing key has its value replaced on its line, a new\n" +
			"key is appended to its section, and nothing else - comments included - is touched.\n" +
			"Fields are yaml paths: host, interval, jitter, prune, logFormat, stateDir, unitDir,\n" +
			"secretsDir, envFile, repositories.N.url, repositories.N.auth.token, ...\n" +
			"Shorthands for the first repository: repo-url, repo-name, repo-path, revision.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			settings, err := parseSettings(args)
			if err != nil {
				return err
			}
			if path == "" {
				found, err := existingConfig(f)
				if err != nil {
					return err
				}
				path = found
			}
			return config.EditAgentConfig(path, settings...)
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "config file to edit (default: the config path)")
	return cmd
}

// parseSettings accepts "field value" or any number of "field=value".
func parseSettings(args []string) ([]config.Setting, error) {
	if len(args) == 2 && !strings.Contains(args[0], "=") {
		return []config.Setting{{Path: args[0], Value: args[1]}}, nil
	}
	settings := make([]config.Setting, 0, len(args))
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("%q: expected <field> <value> or <field>=<value>", a)
		}
		settings = append(settings, config.Setting{Path: k, Value: v})
	}
	return settings, nil
}
