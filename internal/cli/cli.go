// Package cli implements the commands shared by the agent and the admin CLI.
package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/podcd/podcd/pkg/config"
	"github.com/podcd/podcd/pkg/reconciler"
)

// Version is set at build time with -ldflags "-X .../internal/cli.Version=...".
var Version = "dev"

// Main runs a command and returns a process exit code.
func Main(args []string) int {
	root := newRootCommand()
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		return 1
	}
	return 0
}

// --config     which agent config to read
// --host       act as this host identity
// --log-level  debug | info | warn | error
// --log-format text | json
type configFlags struct {
	config    string
	host      string
	logLevel  string
	logFormat string
}

func (f *configFlags) addFlags(root *cobra.Command) {
	pf := root.PersistentFlags()
	pf.StringVar(&f.config, "config", "", "agent config file (default: $PODCD_CONFIG, ~/.config/podcd/agent.yaml, /etc/podcd/agent.yaml)")
	pf.StringVar(&f.host, "host", "", "act as this host identity instead of the configured one")
	pf.StringVar(&f.logLevel, "log-level", "info", "debug, info, warn or error")
	pf.StringVar(&f.logFormat, "log-format", "", "text or json")
}

func newRootCommand() *cobra.Command {
	f := &configFlags{}
	root := &cobra.Command{
		Use:           "podcd",
		Short:         "Git-driven reconciler for containers on a host",
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)
	f.addFlags(root)
	root.SetUsageTemplate(usageTemplate)

	root.AddCommand(
		newStatusCommand(f),
		newPlanCommand(f),
		newReconcileCommand(f),
		newHealthCommand(f),
		newLogsCommand(f),
		newValidateCommand(f),
		newRunCommand(f),
		newGetCommand(f),
		newLintCommand(),
		newInitCommand(),
		newCreateCommand(),
		newInstallCommand(),
		newUninstallCommand(),
		newPruneCommand(f),
		newTeardownCommand(f),
		newConfigCommand(f),
		newVersionCommand(),
		newOptionsCommand(root),
	)

	root.RunE = func(cmd *cobra.Command, _ []string) error { return cmd.Help() }
	return root
}

const usageTemplate = `Usage:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

Aliases:
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}

Available Commands:{{range .Commands}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

Flags:
{{.LocalNonPersistentFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}
{{if .HasAvailableSubCommands}}
Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}
Use "podcd options" for a list of global command-line options (applies to all commands).
`

func newOptionsCommand(root *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:   "options",
		Short: "print the list of flags inherited by all commands",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintln(cmd.OutOrStdout(), "The following options can be passed to any command:")
			fmt.Fprintln(cmd.OutOrStdout())
			fmt.Fprint(cmd.OutOrStdout(), root.PersistentFlags().FlagUsages())
		},
	}
}

// environment is everything an engine-backed command needs.
type environment struct {
	cfg    config.AgentConfig
	engine *reconciler.Engine
	log    *slog.Logger
	out    io.Writer
}

// withEngine wraps a command body: it resolves the config flags into an engine
func withEngine(f *configFlags, body func(context.Context, *environment) error) func(*cobra.Command, []string) error {
	return withEngineArgs(f, func(ctx context.Context, env *environment, _ *cobra.Command, _ []string) error {
		return body(ctx, env)
	})
}

// withEngineArgs is withEngine for a command whose body also needs cobra's
// own args and *cobra.Command - positional arguments cobra parsed, or a
// confirmation prompt that has to write to the command's own streams.
func withEngineArgs(f *configFlags, body func(context.Context, *environment, *cobra.Command, []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		env, err := setup(*f)
		if err != nil {
			return err
		}
		env.out = cmd.OutOrStdout()
		return body(ctx, env, cmd, args)
	}
}

func setup(f configFlags) (*environment, error) {
	configPath := f.config
	if configPath == "" {
		found, err := config.FindAgentConfig()
		if err != nil {
			return nil, err
		}
		configPath = found
	}
	cfg, err := config.LoadAgentConfig(configPath)
	if err != nil {
		return nil, err
	}
	if f.host != "" {
		cfg.Host = f.host
		// An explicit --host must win over a hostname or PODCD_HOST that
		// happens to be set; otherwise "what does host X get?" is unanswerable.
		os.Unsetenv("PODCD_HOST")
	}
	if f.logFormat != "" {
		cfg.LogFormat = f.logFormat
	}

	log := newLogger(cfg.LogFormat, f.logLevel)
	engine, err := reconciler.NewEngine(cfg, log)
	if err != nil {
		return nil, err
	}
	return &environment{cfg: cfg, engine: engine, log: log, out: os.Stdout}, nil
}

func newLogger(format, level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

func newVersionCommand() *cobra.Command {
	var out outputFormat
	cmd := &cobra.Command{
		Use:   "version",
		Short: "print the version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if out != "" {
				return out.write(cmd.OutOrStdout(), map[string]string{"version": Version, "commit": vcsRevision()})
			}
			fmt.Fprintln(cmd.OutOrStdout(), versionString())
			return nil
		},
	}
	out.addFlag(cmd)
	return cmd
}

func vcsRevision() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				return s.Value
			}
		}
	}
	return ""
}

func versionString() string {
	v := Version
	if rev := vcsRevision(); len(rev) >= 7 {
		v += " (" + rev[:7] + ")"
	}
	return "podcd " + v
}

// defaultServiceFilePath is where `install` writes and `uninstall` looks
// when --output is not given.
func defaultServiceFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determining the home directory: %w", err)
	}
	return filepath.Join(home, ".config", "systemd", "user", serviceUnitName), nil
}
