package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/podcd/podcd/pkg/reconciler"
)

func newStatusCommand(f *configFlags) *cobra.Command {
	var out outputFormat
	cmd := &cobra.Command{
		Use:   "status",
		Short: "what this host is running and when it last reconciled",
		Args:  cobra.NoArgs,
		RunE: withEngine(f, func(ctx context.Context, env *environment) error {
			st, err := env.engine.Status(ctx)
			if err != nil {
				return err
			}
			if out != "" {
				return out.write(env.out, st)
			}
			printStatus(env, st)
			return nil
		}),
	}
	out.addFlag(cmd)
	return cmd
}

func newPlanCommand(f *configFlags) *cobra.Command {
	var out outputFormat
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "what would change, without changing anything",
		Args:  cobra.NoArgs,
		RunE: withEngine(f, func(ctx context.Context, env *environment) error {
			res, err := env.engine.Plan(ctx)
			if err != nil {
				return err
			}
			if out != "" {
				return out.write(env.out, res.Plan)
			}
			printPlan(env.out, res)
			return nil
		}),
	}
	out.addFlag(cmd)
	return cmd
}

func newReconcileCommand(f *configFlags) *cobra.Command {
	var out outputFormat
	var dryRun, noPrune bool
	cmd := &cobra.Command{
		Use:     "reconcile",
		Aliases: []string{"apply"},
		Short:   "make this host match Git",
		Args:    cobra.NoArgs,
		RunE: withEngine(f, func(ctx context.Context, env *environment) error {
			opts := reconciler.Options{DryRun: dryRun}
			if noPrune {
				off := false
				opts.Prune = &off
			}
			res, err := env.engine.Reconcile(ctx, opts)
			if out != "" {
				_ = out.write(env.out, map[string]any{
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
			if dryRun {
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
		}),
	}
	out.addFlag(cmd)
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "plan only; change nothing")
	cmd.Flags().BoolVar(&noPrune, "no-prune", false, "keep applications Git no longer declares")
	return cmd
}

func newHealthCommand(f *configFlags) *cobra.Command {
	var out outputFormat
	cmd := &cobra.Command{
		Use:   "health",
		Short: "report what podman says about the applications this host runs",
		Args:  cobra.NoArgs,
		RunE: withEngine(f, func(ctx context.Context, env *environment) error {
			results, err := env.engine.Health(ctx)
			if err != nil {
				return err
			}
			if out != "" {
				return out.write(env.out, results)
			}
			printHealth(env.out, results)
			if n := countUnhealthy(results); n > 0 {
				return fmt.Errorf("%d application(s) are not healthy", n)
			}
			return nil
		}),
	}
	out.addFlag(cmd)
	return cmd
}

func newLogsCommand(f *configFlags) *cobra.Command {
	var tail int
	var app string
	cmd := &cobra.Command{
		Use:   "logs <application>",
		Short: "recent log output for one application",
		Args:  cobra.ExactArgs(1),
		PreRun: func(_ *cobra.Command, args []string) {
			app = args[0]
		},
		RunE: withEngine(f, func(ctx context.Context, env *environment) error {
			text, err := env.engine.Logs(ctx, app, tail)
			if err != nil {
				return err
			}
			fmt.Fprint(env.out, text)
			return nil
		}),
	}
	cmd.Flags().IntVar(&tail, "tail", 50, "number of recent lines to show")
	return cmd
}

func newValidateCommand(f *configFlags) *cobra.Command {
	var out outputFormat
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "load Git and compile the configuration for a host",
		Args:  cobra.NoArgs,
		RunE: withEngine(f, func(ctx context.Context, env *environment) error {
			res, err := env.engine.Plan(ctx)
			if err != nil {
				return err
			}
			if out != "" {
				return out.write(env.out, res.Desired)
			}
			d := res.Desired
			fmt.Fprintf(env.out, "host %s (environment %s, groups %s) at %s\n",
				d.Host, dash(d.Environment), dash(strings.Join(d.Groups, ",")), d.RevisionString())
			w := table(env.out)
			fmt.Fprintln(w, "  APP\tIMAGE\tPORTS\tFROM")
			for _, a := range d.Applications {
				image := fmt.Sprintf("%d image(s): %s", len(a.ImageList()), shortImage(a.Image))
				fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", a.Name, image, portList(a.Ports), strings.Join(a.Origins, " → "))
			}
			w.Flush()
			fmt.Fprintf(env.out, "%d application(s); configuration is valid\n", len(d.Applications))
			return nil
		}),
	}
	out.addFlag(cmd)
	return cmd
}

func newRunCommand(f *configFlags) *cobra.Command {
	return &cobra.Command{
		Use:     "run",
		Aliases: []string{"agent"},
		Short:   "reconcile in a loop (this is what the systemd service runs)",
		Args:    cobra.NoArgs,
		RunE: withEngine(f, func(ctx context.Context, env *environment) error {
			return env.engine.Run(ctx)
		}),
	}
}
