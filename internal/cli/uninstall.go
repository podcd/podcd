package cli

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

// serviceUnitName is the agent's systemd unit name, whatever --output is.
const serviceUnitName = "podcd-agent.service"

func newUninstallCommand() *cobra.Command {
	var output string
	var yes bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "stop the agent's systemd user service and remove its unit file",
		Long: "The reverse of `podcd install`: stops and disables " + serviceUnitName + ", removes its unit\n" +
			"file, and reloads systemd. It does not touch the applications the agent manages - see\n" +
			"`podcd prune` and `podcd teardown` for that.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dest := output
			if dest == "" {
				var err error
				if dest, err = defaultServiceFilePath(); err != nil {
					return err
				}
			}
			_, statErr := os.Stat(dest)
			if os.IsNotExist(statErr) {
				fmt.Fprintln(cmd.OutOrStdout(), dest+" is not there; nothing to uninstall")
				return nil
			}
			if statErr != nil {
				return statErr
			}
			if !yes && !confirm(cmd, "Stop "+serviceUnitName+" and remove "+dest+"?") {
				fmt.Fprintln(cmd.ErrOrStderr(), "aborted")
				return nil
			}
			uninstallAgentService(dest)
			fmt.Fprintln(cmd.OutOrStdout(), "removed "+dest)
			return nil
		},
	}
	cmd.Flags().StringVar(&output, "output", "", "the service file to remove (default: ~/.config/systemd/user/podcd-agent.service)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "uninstall without asking")
	return cmd
}

// uninstallAgentService stops and disables the unit and removes dest, best-effort.
func uninstallAgentService(dest string) {
	userSystemctl("stop", serviceUnitName)
	userSystemctl("disable", serviceUnitName)
	_ = os.Remove(dest)
	userSystemctl("daemon-reload")
}

// userSystemctl runs `systemctl --user <args>`, ignoring errors.
func userSystemctl(args ...string) {
	_ = exec.Command("systemctl", append([]string{"--user"}, args...)...).Run()
}
