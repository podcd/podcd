// Package cli implements the commands shared by the agent and the debugging
// CLI, so that what the agent does and what `podcd plan` shows can never drift.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/podcd/podcd/pkg/config"
	"github.com/podcd/podcd/pkg/model"
	"github.com/podcd/podcd/pkg/reconciler"
	"github.com/podcd/podcd/pkg/state"
)

// Version is set at build time with -ldflags "-X .../internal/cli.Version=...".
var Version = "dev"

const usage = `podcd - Git-driven reconciler containers

usage: %s [global flags] <command> [flags]

commands:
  status      what this host is running and when it last reconciled
  plan        what would change, without changing anything
  reconcile   make this host match Git
  health      probe the applications this host should be running
  logs <app>  recent log output for one application
  validate    load Git and compile the configuration for a host
  install     install the podcd-agent systemd user service file
  config      view or create the agent config file
  run         reconcile in a loop (this is what the systemd service runs)
  version     print the version

global flags:
  --config PATH   agent config (default: $PODCD_CONFIG, ~/.config/podcd/agent.yaml, /etc/podcd/agent.yaml)
  --host NAME     override this host's identity
  --log-level L   debug, info, warn, error (default info)
  --log-format F  text or json
`

// Main runs a command and returns a process exit code.
//
// defaultCmd is used when no command is given, so the agent binary can default
// to "run" while the CLI prints help.
func Main(args []string, defaultCmd string) int {
	global := flag.NewFlagSet("podcd", flag.ContinueOnError)
	global.SetOutput(os.Stderr)
	configPath := global.String("config", "", "path to the agent config")
	hostOverride := global.String("host", "", "override this host's identity")
	logLevel := global.String("log-level", "info", "debug, info, warn, error")
	logFormat := global.String("log-format", "", "text or json")
	global.Usage = func() { fmt.Fprintf(os.Stderr, usage, progName()) }

	if err := global.Parse(args); err != nil {
		return 2
	}
	rest := global.Args()
	cmd := defaultCmd
	if len(rest) > 0 {
		cmd, rest = rest[0], rest[1:]
	}
	if cmd == "" || cmd == "help" || cmd == "-h" || cmd == "--help" {
		global.Usage()
		return 0
	}
	if cmd == "version" {
		fmt.Println(versionString())
		return 0
	}

	if cmd == "install" {
		if err := cmdInstall(rest); err != nil {
			fmt.Fprintln(os.Stderr, "error: "+err.Error())
			return 1
		}
		return 0
	}
	if cmd == "config" {
		if err := cmdConfig(rest); err != nil {
			fmt.Fprintln(os.Stderr, "error: "+err.Error())
			return 1
		}
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	env, err := setup(*configPath, *hostOverride, *logLevel, *logFormat)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		return 1
	}

	runErr := dispatch(ctx, env, cmd, rest)
	if runErr != nil {
		if errors.Is(runErr, flag.ErrHelp) {
			return 2
		}
		fmt.Fprintln(os.Stderr, "error: "+runErr.Error())
		return 1
	}
	return 0
}

type environment struct {
	cfg    config.AgentConfig
	engine *reconciler.Engine
	log    *slog.Logger
	out    io.Writer
}

func setup(configPath, hostOverride, logLevel, logFormat string) (*environment, error) {
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
	if hostOverride != "" {
		cfg.Host = hostOverride
		// An explicit --host must win over a hostname or PODCD_HOST that
		// happens to be set; otherwise "what does host X get?" is unanswerable.
		os.Unsetenv("PODCD_HOST")
	}
	if logFormat != "" {
		cfg.LogFormat = logFormat
	}

	log := newLogger(cfg.LogFormat, logLevel)
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

func dispatch(ctx context.Context, env *environment, cmd string, args []string) error {
	switch cmd {
	case "status":
		return cmdStatus(ctx, env, args)
	case "plan":
		return cmdPlan(ctx, env, args)
	case "reconcile", "apply":
		return cmdReconcile(ctx, env, args)
	case "health":
		return cmdHealth(ctx, env, args)
	case "logs":
		return cmdLogs(ctx, env, args)
	case "validate":
		return cmdValidate(ctx, env, args)
	case "install":
		return cmdInstall(args)
	case "config":
		return cmdConfig(args)
	case "run", "agent":
		return env.engine.Run(ctx)
	default:
		return fmt.Errorf("unknown command %q (try: status, plan, reconcile, health, logs, validate, install, run, version)", cmd)
	}
}

func serviceFileContents() string {
	return `[Unit]
Description=podcd GitOps agent
Documentation=https://github.com/podcd/podcd
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/podcd-agent run
Restart=always
RestartSec=10
EnvironmentFile=-%h/.config/podcd/agent.env

[Install]
WantedBy=default.target
`
}

func defaultConfigPath() string {
	if p := os.Getenv("PODCD_CONFIG"); p != "" {
		return p
	}
	if user := os.Getenv("SUDO_USER"); user != "" && os.Getuid() == 0 {
		if home := filepath.Join("/home", user); func() bool {
			_, err := os.Stat(home)
			return err == nil
		}() {
			return filepath.Join(home, ".config", "podcd", "agent.yaml")
		}
	}
	home, err := os.UserHomeDir()
	if err == nil {
		return filepath.Join(home, ".config", "podcd", "agent.yaml")
	}
	return "/etc/podcd/agent.yaml"
}

func defaultAgentConfigYAML(host, repoURL, repoName, repoPath, revision, interval string) string {
	if repoName == "" {
		repoName = "infrastructure"
	}
	if repoURL == "" {
		repoURL = "https://github.com/podcd/podcd.git"
	}
	if revision == "" {
		revision = "main"
	}
	if interval == "" {
		interval = "60s"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# The agent's own configuration.\n")
	fmt.Fprintf(&b, "# It lives on the VM (~/.config/podcd/agent.yaml or /etc/podcd/agent.yaml), not in Git.\n\n")
	if host == "" {
		fmt.Fprintf(&b, "host: \"\"\n")
	} else {
		fmt.Fprintf(&b, "host: %s\n", host)
	}
	fmt.Fprintf(&b, "interval: %s\n", interval)
	fmt.Fprintf(&b, "jitter: 10s\n\n")
	fmt.Fprintf(&b, "runtime: podman\n\n")
	fmt.Fprintf(&b, "repositories:\n")
	fmt.Fprintf(&b, "  - name: %s\n", repoName)
	fmt.Fprintf(&b, "    url: %s\n", repoURL)
	fmt.Fprintf(&b, "    revision: %s\n", revision)
	if repoPath != "" {
		fmt.Fprintf(&b, "    path: %s\n", repoPath)
	}
	fmt.Fprintf(&b, "\nprune: true\n")
	fmt.Fprintf(&b, "secretsDir: /etc/podcd/secrets\n")
	fmt.Fprintf(&b, "logFormat: text\n")
	return b.String()
}

func cmdConfig(args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(os.Stdout, defaultConfigPath())
		return nil
	}

	switch args[0] {
	case "path":
		fmt.Fprintln(os.Stdout, defaultConfigPath())
		return nil
	case "create":
		fs := flag.NewFlagSet("config create", flag.ContinueOnError)
		path := fs.String("path", defaultConfigPath(), "path for the agent config")
		host := fs.String("host", "", "host override")
		repoURL := fs.String("repo-url", "https://github.com/podcd/podcd.git", "Git repository URL")
		repoName := fs.String("repo-name", "infrastructure", "repository name")
		repoPath := fs.String("repo-path", "", "repository subdirectory to read")
		revision := fs.String("revision", "main", "branch, tag or SHA")
		interval := fs.String("interval", "60s", "reconcile interval")
		force := fs.Bool("force", false, "overwrite an existing config")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if _, err := os.Stat(*path); err == nil && !*force {
			return fmt.Errorf("config already exists at %s; use --force to overwrite", *path)
		}
		if err := os.MkdirAll(filepath.Dir(*path), 0o755); err != nil {
			return fmt.Errorf("create config dir %s: %w", filepath.Dir(*path), err)
		}
		content := defaultAgentConfigYAML(*host, *repoURL, *repoName, *repoPath, *revision, *interval)
		if err := os.WriteFile(*path, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write config %s: %w", *path, err)
		}
		fmt.Fprintln(os.Stdout, *path)
		return nil
	default:
		return fmt.Errorf("unknown config command %q (try: path, create)", args[0])
	}
}

func cmdInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	outputPath := fs.String("output", "", "destination for the systemd service file")
	force := fs.Bool("y", false, "overwrite the destination file without prompting")
	forceLong := fs.Bool("yes", false, "overwrite the destination file without prompting")
	if err := fs.Parse(args); err != nil {
		return err
	}

	dest := *outputPath
	if dest == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("determine home directory: %w", err)
		}
		dest = filepath.Join(home, ".config", "systemd", "user", "podcd-agent.service")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("create service directory %s: %w", filepath.Dir(dest), err)
	}
	if _, err := os.Stat(dest); err == nil {
		if !*force && !*forceLong {
			fmt.Fprintf(os.Stderr, "%s already exists. Overwrite? [y/N]: ", dest)
			var answer string
			if _, err := fmt.Scanln(&answer); err != nil && !errors.Is(err, io.EOF) {
				return nil
			}
			if !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
				fmt.Fprintln(os.Stderr, "aborted")
				return nil
			}
		}
	}
	if err := os.WriteFile(dest, []byte(serviceFileContents()), 0o644); err != nil {
		return fmt.Errorf("write service file %s: %w", dest, err)
	}
	fmt.Fprintf(os.Stdout, "%s\n", dest)
	return nil
}

func cmdStatus(ctx context.Context, env *environment, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print machine readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := env.engine.Status(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(env.out, st)
	}

	w := tabwriter.NewWriter(env.out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "host\t%s\t(%s)\n", st.Identity.Host, st.Identity.Source)
	runtimeNote := "available"
	if !st.Available {
		runtimeNote = "UNAVAILABLE: " + st.Why
	}
	fmt.Fprintf(w, "runtime\t%s\t(%s)\n", st.Runtime, runtimeNote)
	fmt.Fprintf(w, "config\t%s\t\n", st.Config)
	fmt.Fprintf(w, "state\t%s\t\n", env.engine.Store().Path())
	w.Flush()

	fmt.Fprintln(env.out, "\nrepositories")
	w = tabwriter.NewWriter(env.out, 0, 0, 2, ' ', 0)
	for _, r := range st.Repos {
		rev := ""
		if st.State.LastSuccess != nil {
			rev = shortRev(st.State.LastSuccess.Revisions[r.Name])
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", r.Name, r.URL, r.Revision, rev)
	}
	w.Flush()

	fmt.Fprintln(env.out, "\nlast reconcile")
	w = tabwriter.NewWriter(env.out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "  success\t%s\t%s\n", attemptTime(st.State.LastSuccess), attemptDetail(st.State.LastSuccess))
	fmt.Fprintf(w, "  failure\t%s\t%s\n", attemptTime(st.State.LastFailure), attemptDetail(st.State.LastFailure))
	if st.State.FailureCount > 0 {
		fmt.Fprintf(w, "  failing\t%d consecutive failure(s)\t\n", st.State.FailureCount)
	}
	w.Flush()

	fmt.Fprintln(env.out, "\napplications")
	if len(st.Actual.Apps) == 0 {
		fmt.Fprintln(env.out, "  (none)")
		return nil
	}
	w = tabwriter.NewWriter(env.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  APP\tUNIT\tCONTAINER\tIMAGE\tHEALTH\tAPPLIED")
	for _, name := range st.Actual.Names() {
		a := st.Actual.Apps[name]
		rec := st.State.Applications[name]
		owner := ""
		if !a.Managed {
			owner = " (not managed by podcd)"
		}
		fmt.Fprintf(w, "  %s\t%s%s\t%s\t%s\t%s\t%s\n",
			name, string(a.UnitState), owner, dash(a.ContainerState),
			dash(shortImage(firstNonEmpty(rec.Image, a.ContainerImage))),
			dash(rec.Health), dash(rec.AppliedAt))
	}
	return w.Flush()
}

func cmdPlan(ctx context.Context, env *environment, args []string) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print machine readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}

	res, err := env.engine.Plan(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(env.out, res.Plan)
	}
	printPlan(env.out, res)
	return nil
}

func cmdReconcile(ctx context.Context, env *environment, args []string) error {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print machine readable output")
	dryRun := fs.Bool("dry-run", false, "plan only; change nothing")
	noPrune := fs.Bool("no-prune", false, "keep applications Git no longer declares")
	if err := fs.Parse(args); err != nil {
		return err
	}

	opts := reconciler.Options{DryRun: *dryRun}
	if *noPrune {
		off := false
		opts.Prune = &off
	}

	res, err := env.engine.Reconcile(ctx, opts)
	if *asJSON {
		_ = writeJSON(env.out, map[string]any{
			"plan":    res.Plan,
			"applied": res.Applied,
			"health":  res.Health,
			"offline": res.Offline,
			"error":   errString(err),
		})
		return err
	}
	if err != nil {
		// Show what was attempted before reporting why it stopped.
		if len(res.Applied) > 0 {
			fmt.Fprintln(env.out, "applied before failing:")
			for _, a := range res.Applied {
				fmt.Fprintf(env.out, "  %s %s\n", a.Type, a.App)
			}
		}
		return err
	}

	if *dryRun {
		printPlan(env.out, res)
		return nil
	}
	if len(res.Applied) == 0 {
		fmt.Fprintf(env.out, "nothing to do (%d application(s), %s)\n",
			len(res.Desired.Applications), res.Desired.RevisionString())
	} else {
		fmt.Fprintf(env.out, "applied %d change(s) at %s\n", len(res.Applied), res.Desired.RevisionString())
		for _, a := range res.Applied {
			fmt.Fprintf(env.out, "  %s %s - %s\n", actionSymbol(a.Type), a.App, a.Reason)
		}
	}
	printHealth(env.out, res.Health)
	return nil
}

func cmdHealth(ctx context.Context, env *environment, args []string) error {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print machine readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	results, err := env.engine.Health(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(env.out, results)
	}
	printHealth(env.out, results)
	for _, h := range results {
		if !h.OK() {
			return fmt.Errorf("%d application(s) are not healthy", countUnhealthy(results))
		}
	}
	return nil
}

func cmdLogs(ctx context.Context, env *environment, args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	lines := fs.Int("n", 50, "number of lines")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: podcd logs [-n lines] <application>")
	}
	out, err := env.engine.Logs(ctx, fs.Arg(0), *lines)
	if err != nil {
		return err
	}
	fmt.Fprint(env.out, out)
	return nil
}

func cmdValidate(ctx context.Context, env *environment, args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the compiled desired state")
	if err := fs.Parse(args); err != nil {
		return err
	}
	res, err := env.engine.Plan(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(env.out, res.Desired)
	}
	d := res.Desired
	fmt.Fprintf(env.out, "host %s (environment %s, groups %s) at %s\n",
		d.Host, dash(d.Environment), dash(strings.Join(d.Groups, ",")), d.RevisionString())
	w := tabwriter.NewWriter(env.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  APP\tKIND\tIMAGE\tPORTS\tFROM")
	for _, a := range d.Applications {
		kind := "container"
		image := shortImage(a.Image)
		if a.IsKube() {
			kind = "pod"
			image = fmt.Sprintf("%d image(s): %s", len(a.ImageList()), shortImage(a.Image))
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n", a.Name, kind, image, portList(a.Ports), strings.Join(a.Origins, " → "))
	}
	w.Flush()
	fmt.Fprintf(env.out, "%d application(s); configuration is valid\n", len(d.Applications))
	return nil
}

func printPlan(w io.Writer, res reconciler.Result) {
	fmt.Fprintf(w, "plan for %s at %s\n", res.Desired.Host, res.Desired.RevisionString())
	for _, r := range res.Offline {
		fmt.Fprintf(w, "  warning: repository %q could not be refreshed; using the commit already on disk\n", r)
	}
	changes := res.Plan.Changes()
	if len(changes) == 0 {
		fmt.Fprintf(w, "  no changes (%d application(s) already match Git)\n", len(res.Desired.Applications))
		return
	}
	for _, a := range changes {
		destructive := ""
		if a.Destructive {
			destructive = "  [destructive]"
		}
		fmt.Fprintf(w, "  %s %s - %s%s\n", actionSymbol(a.Type), a.App, a.Reason, destructive)
		for _, d := range a.Details {
			fmt.Fprintf(w, "      %s\n", d)
		}
	}
	fmt.Fprintf(w, "%d change(s)\n", len(changes))
}

func printHealth(w io.Writer, results []model.Health) {
	if len(results) == 0 {
		return
	}
	sorted := append([]model.Health(nil), results...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].App < sorted[j].App })
	fmt.Fprintln(w, "health")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, h := range sorted {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", h.App, h.Status, h.Probe, h.Message)
	}
	tw.Flush()
}

func actionSymbol(t model.ActionType) string {
	switch t {
	case model.ActionCreate:
		return "+ create"
	case model.ActionUpdate:
		return "~ update"
	case model.ActionDelete:
		return "- delete"
	case model.ActionRestart:
		return "> restart"
	default:
		return "= noop"
	}
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// attemptTime renders when an attempt happened, with how long ago that was:
// "three hours ago" is what tells you the agent is stuck.
func attemptTime(a *state.Attempt) string {
	if a == nil || a.At.IsZero() {
		return "never"
	}
	return fmt.Sprintf("%s (%s ago)", a.At.Format(time.RFC3339), time.Since(a.At).Round(time.Second))
}

func attemptDetail(a *state.Attempt) string {
	switch {
	case a == nil:
		return ""
	case a.Error != "":
		return firstLine(a.Error)
	case len(a.Actions) > 0:
		return strings.Join(a.Actions, ", ")
	default:
		return "no changes"
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " ..."
	}
	return s
}

func shortRev(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func shortImage(s string) string {
	if i := strings.Index(s, "@sha256:"); i > 0 && len(s) > i+20 {
		return s[:i+20] + "..."
	}
	return s
}

func portList(ports []model.Port) string {
	if len(ports) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		parts = append(parts, fmt.Sprintf("%d→%d", p.Host, p.Container))
	}
	return strings.Join(parts, ",")
}

func countUnhealthy(results []model.Health) int {
	n := 0
	for _, h := range results {
		if !h.OK() {
			n++
		}
	}
	return n
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func progName() string {
	if len(os.Args) == 0 {
		return "podcd"
	}
	parts := strings.Split(os.Args[0], "/")
	return parts[len(parts)-1]
}

func versionString() string {
	v := Version
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				v += " (" + s.Value[:7] + ")"
			}
		}
	}
	return "podcd " + v
}
