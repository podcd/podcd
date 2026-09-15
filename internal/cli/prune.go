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

func newPruneCommand(f *configFlags) *cobra.Command {
	var all, yes bool
	cmd := &cobra.Command{
		Use:   "prune [name...]",
		Short: "stop and remove applications directly, without consulting Git",
		Long: "The counterpart to a reconcile's automatic pruning: removes applications right now, by name\n" +
			"or with --all, whether or not the repository is reachable.",
		Args: cobra.ArbitraryArgs,
		RunE: withEngineArgs(f, func(ctx context.Context, env *environment, cmd *cobra.Command, names []string) error {
			if len(names) == 0 && !all {
				return fmt.Errorf("name one or more applications, or pass --all")
			}
			candidates, _, err := env.engine.Candidates(ctx, names, all)
			if err != nil {
				return err
			}
			if len(candidates) == 0 {
				fmt.Fprintln(env.out, "nothing to prune")
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

			res, pruneErr := env.engine.Prune(ctx, names, all)
			printPruneResult(env.out, res)
			return pruneErr
		}),
	}
	cmd.Flags().BoolVar(&all, "all", false, "remove every application podcd manages")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "prune without asking")
	return cmd
}

func printPruneResult(w io.Writer, res reconciler.PruneResult) {
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
