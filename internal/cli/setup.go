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

func newInstallCommand() *cobra.Command {
	var output string
	var yes bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "write the systemd user service file that runs the agent",
		Long: "Writes the systemd user unit that runs the agent (the same file as deploy/podcd-agent.service).\n" +
			"Enable it with: systemctl --user enable --now podcd-agent.service",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dest := output
			if dest == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					return fmt.Errorf("determining the home directory: %w", err)
				}
				dest = filepath.Join(home, ".config", "systemd", "user", "podcd-agent.service")
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
			if err := os.WriteFile(dest, deploy.AgentService, 0o644); err != nil {
				return fmt.Errorf("writing %s: %w", dest, err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), dest)
			return nil
		},
	}
	cmd.Flags().StringVar(&output, "output", "", "destination for the service file (default: ~/.config/systemd/user/podcd-agent.service)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "overwrite an existing file without asking")
	return cmd
}

// confirm asks a yes/no question on the terminal. Anything but an explicit
// yes - including EOF from a non-interactive stdin - is a no.
func confirm(cmd *cobra.Command, question string) bool {
	fmt.Fprintf(cmd.ErrOrStderr(), "%s [y/N]: ", question)
	answer, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}

func newConfigCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "view or create the agent config file",
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newConfigPathCommand(), newConfigViewCommand(), newConfigCreateCommand(), newConfigSetCommand())
	return cmd
}

func newConfigPathCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "print where the agent config is (or would be) read from",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if found, err := config.FindAgentConfig(); err == nil {
				fmt.Fprintln(cmd.OutOrStdout(), found)
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), config.DefaultConfigPath())
			return nil
		},
	}
}

func newConfigViewCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "view [path]",
		Short: "print the agent config file",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := ""
			if len(args) == 1 {
				path = args[0]
			} else {
				found, err := config.FindAgentConfig()
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

func newConfigCreateCommand() *cobra.Command {
	var (
		path, host, repoURL, repoName, repoPath, revision string
		interval                                          time.Duration
		force                                             bool
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "write a new agent config file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
			cfg.Repositories = []config.RepositorySpec{{Name: repoName, URL: repoURL, Revision: revision, Path: repoPath}}
			if err := config.WriteAgentConfig(path, cfg); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), path)
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "where to write (default: the config path)")
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

func newConfigSetCommand() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "set <field> <value>",
		Short: "change one field of the agent config file",
		Long: "Fields: host, interval, jitter, runtime, log-format, state-dir, unit-dir, secrets-dir,\n" +
			"env-file, prune, repo-url, repo-name, repo-path, revision (the last four edit the first repository).",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if path == "" {
				found, err := config.FindAgentConfig()
				if err != nil {
					return err
				}
				path = found
			}
			cfg, err := config.LoadAgentConfig(path)
			if err != nil {
				return err
			}
			if err := cfg.SetValue(args[0], args[1]); err != nil {
				return err
			}
			return config.WriteAgentConfig(path, cfg)
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "config file to edit (default: the config path)")
	return cmd
}
