package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/podcd/podcd/pkg/reconciler"
)

func newRemoveCommand(f *configFlags) *cobra.Command {
	var all, yes bool
	cmd := &cobra.Command{
		Use:     "remove [name...]",
		Aliases: []string{"rm"},
		Short:   "stop and remove applications directly, without consulting Git",
		Long: "Removes applications right now, by name or with --all, whether or not the repository is\n" +
			"reachable. Git is not consulted: an application it still declares comes back on the next\n" +
			"reconcile. To remove only what Git no longer declares, use prune.",
		Args: cobra.ArbitraryArgs,
		RunE: withEngineArgs(f, func(ctx context.Context, env *environment, cmd *cobra.Command, names []string) error {
			if len(names) == 0 && !all {
				return fmt.Errorf("name one or more applications, or pass --all")
			}
			candidates, _, err := env.engine.RemoveCandidates(ctx, names, all)
			if err != nil {
				return err
			}
			if len(candidates) == 0 {
				fmt.Fprintln(env.out, "nothing to remove")
				return nil
			}
			fmt.Fprintln(env.out, "will stop and remove:")
			for _, n := range candidates {
				fmt.Fprintln(env.out, "  "+n)
			}
			if !yes && !confirm(cmd, fmt.Sprintf("Remove %d application(s)?", len(candidates))) {
				fmt.Fprintln(cmd.ErrOrStderr(), "aborted")
				return nil
			}

			res, err := env.engine.Remove(ctx, names, all)
			printRemoveResult(env.out, res)
			if err != nil {
				return err
			}
			return res.Err()
		}),
	}
	cmd.Flags().BoolVar(&all, "all", false, "remove every application podcd manages")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "remove without asking")
	return cmd
}

func newPruneCommand(f *configFlags) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "remove what podcd manages here but Git no longer declares",
		Long: "Pulls the repository, compares it with the host, and removes the managed applications the\n" +
			"repository no longer declares for this host - the same set a reconcile would delete, without\n" +
			"applying anything else. Applications still in Git and units podcd does not manage are never\n" +
			"touched. To remove an application regardless of Git, use remove.",
		Args: cobra.NoArgs,
		RunE: withEngineArgs(f, func(ctx context.Context, env *environment, cmd *cobra.Command, _ []string) error {
			candidates, _, err := env.engine.PruneCandidates(ctx)
			if err != nil {
				return err
			}
			if len(candidates) == 0 {
				fmt.Fprintln(env.out, "nothing to prune: everything podcd manages here is still declared in Git")
				return nil
			}
			fmt.Fprintln(env.out, "no longer declared in Git, will stop and remove:")
			for _, n := range candidates {
				fmt.Fprintln(env.out, "  "+n)
			}
			if !yes && !confirm(cmd, fmt.Sprintf("Remove %d application(s)?", len(candidates))) {
				fmt.Fprintln(cmd.ErrOrStderr(), "aborted")
				return nil
			}

			res, err := env.engine.Prune(ctx)
			printRemoveResult(env.out, res)
			if err != nil {
				return err
			}
			return res.Err()
		}),
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "prune without asking")
	return cmd
}

func printRemoveResult(w io.Writer, res reconciler.RemoveResult) {
	if len(res.Removed) > 0 {
		fmt.Fprintln(w, "removed: "+strings.Join(res.Removed, ", "))
	}
	if len(res.Skipped) > 0 {
		fmt.Fprintln(w, "not managed by podcd, left alone: "+strings.Join(res.Skipped, ", "))
	}
	if len(res.NotFound) > 0 {
		fmt.Fprintln(w, "not found: "+strings.Join(res.NotFound, ", "))
	}
	names := make([]string, 0, len(res.Failed))
	for name := range res.Failed {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(w, "failed: %s: %v\n", name, res.Failed[name])
	}
}
