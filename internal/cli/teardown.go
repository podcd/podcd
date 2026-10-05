package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// newTeardownCommand removes every managed application, then the agent unit. State and config are kept unless purged.
func newTeardownCommand(f *configFlags) *cobra.Command {
	var purgeState, purgeConfig, yes bool
	cmd := &cobra.Command{
		Use:   "teardown",
		Short: "stop and remove everything podcd runs on this host, and uninstall the agent",
		Long: "Composes `podcd prune --all` and `podcd uninstall`: every application podcd manages is\n" +
			"stopped and removed, then the agent's own systemd unit is stopped, disabled and removed.\n" +
			"--purge-state additionally deletes stateDir (checkouts, played manifests, state.json).\n" +
			"--purge-config additionally deletes agent.yaml and its envFile - the repository URL and any\n" +
			"secrets resolved through env: references live there.",
		Args: cobra.NoArgs,
		RunE: withEngineArgs(f, func(ctx context.Context, env *environment, cmd *cobra.Command, _ []string) error {
			candidates, networks, _, err := env.engine.RemoveCandidates(ctx, nil, true)
			if err != nil {
				return err
			}
			serviceFile, err := defaultServiceFilePath()
			if err != nil {
				return err
			}
			stateDir := env.cfg.StateDir

			fmt.Fprintln(env.out, "this will:")
			if len(candidates) > 0 {
				fmt.Fprintf(env.out, "  stop and remove %d application(s): %s\n", len(candidates), strings.Join(candidates, ", "))
			} else {
				fmt.Fprintln(env.out, "  stop and remove 0 applications (none are managed by podcd here)")
			}
			if len(networks) > 0 {
				fmt.Fprintf(env.out, "  remove %d network(s): %s\n", len(networks), strings.Join(networks, ", "))
			}
			fmt.Fprintln(env.out, "  stop, disable and remove "+serviceUnitName+" ("+serviceFile+")")
			if purgeState {
				fmt.Fprintln(env.out, "  delete "+stateDir+" (checkouts, played manifests, state.json)")
			}
			if purgeConfig {
				fmt.Fprintln(env.out, "  delete "+env.cfg.Path+" and "+env.cfg.EnvFile+" (secrets)")
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
				// Only the files podcd owns: --config may point into $HOME, /etc or anywhere else.
				for _, p := range []string{env.cfg.Path, env.cfg.EnvFile} {
					if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
						return fmt.Errorf("removing %s: %w", p, err)
					}
				}
				fmt.Fprintln(env.out, "removed "+env.cfg.Path+" and "+env.cfg.EnvFile)
			}
			if removeErr != nil {
				return removeErr
			}
			return res.Err()
		}),
	}
	cmd.Flags().BoolVar(&purgeState, "purge-state", false, "also delete stateDir (checkouts, played manifests, state.json)")
	cmd.Flags().BoolVar(&purgeConfig, "purge-config", false, "also delete agent.yaml and its envFile (secrets)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "tear down without asking")
	return cmd
}
