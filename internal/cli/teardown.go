package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// Stop and remove every application podcd manages, then stop, disable and remove the agent's own systemd unit.
// It gives this host back, the way it was before `podcd install` and the first reconcile.
// State and config are left alone by default - state.json is metadata a human might still want to look at
func newTeardownCommand(f *configFlags) *cobra.Command {
	var purgeState, purgeConfig, yes bool
	cmd := &cobra.Command{
		Use:   "teardown",
		Short: "stop and remove everything podcd runs on this host, and uninstall the agent",
		Long: "Composes `podcd prune --all` and `podcd uninstall`: every application podcd manages is\n" +
			"stopped and removed, then the agent's own systemd unit is stopped, disabled and removed.\n" +
			"--purge-state additionally deletes stateDir (checkouts, played manifests, state.json).\n" +
			"--purge-config additionally deletes the directory holding agent.yaml and agent.env - the\n" +
			"repository URL and any secrets resolved through env: references live there.",
		Args: cobra.NoArgs,
		RunE: withEngineArgs(f, func(ctx context.Context, env *environment, cmd *cobra.Command, _ []string) error {
			candidates, _, err := env.engine.RemoveCandidates(ctx, nil, true)
			if err != nil {
				return err
			}
			serviceFile, err := defaultServiceFilePath()
			if err != nil {
				return err
			}
			stateDir := env.cfg.StateDir
			configDir := filepath.Dir(env.cfg.Path)

			fmt.Fprintln(env.out, "this will:")
			if len(candidates) > 0 {
				fmt.Fprintf(env.out, "  stop and remove %d application(s): %s\n", len(candidates), strings.Join(candidates, ", "))
			} else {
				fmt.Fprintln(env.out, "  stop and remove 0 applications (none are managed by podcd here)")
			}
			fmt.Fprintln(env.out, "  stop, disable and remove "+serviceUnitName+" ("+serviceFile+")")
			if purgeState {
				fmt.Fprintln(env.out, "  delete "+stateDir+" (checkouts, played manifests, state.json)")
			}
			if purgeConfig {
				fmt.Fprintln(env.out, "  delete "+configDir+", INCLUDING agent.env (secrets)")
			}
			if !yes && !confirm(cmd, "Proceed?") {
				fmt.Fprintln(cmd.ErrOrStderr(), "aborted")
				return nil
			}

			res, removeErr := env.engine.Remove(ctx, nil, true)
			printRemoveResult(env.out, res)

			uninstallAgentService(serviceFile)
			fmt.Fprintln(env.out, "removed "+serviceFile)

			if purgeState {
				if err := os.RemoveAll(stateDir); err != nil {
					return fmt.Errorf("removing %s: %w", stateDir, err)
				}
				fmt.Fprintln(env.out, "removed "+stateDir)
			}
			if purgeConfig {
				if err := os.RemoveAll(configDir); err != nil {
					return fmt.Errorf("removing %s: %w", configDir, err)
				}
				fmt.Fprintln(env.out, "removed "+configDir)
			}
			if removeErr != nil {
				return removeErr
			}
			return res.Err()
		}),
	}
	cmd.Flags().BoolVar(&purgeState, "purge-state", false, "also delete stateDir (checkouts, played manifests, state.json)")
	cmd.Flags().BoolVar(&purgeConfig, "purge-config", false, "also delete agent.yaml and agent.env (secrets)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "tear down without asking")
	return cmd
}
