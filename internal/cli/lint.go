package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/podcd/podcd/pkg/config"
)

func newLintCommand() *cobra.Command {
	var out outputFormat
	var hosts []string
	var valuesFiles []string
	cmd := &cobra.Command{
		Use:   "lint [PATH...]",
		Short: "check repository files without fetching or touching the host",
		Long: "Loads the given directories and files (default: the current directory) with the agent's\n" +
			"loader and compiles them for every Host they define, applying the same rules a reconcile\n" +
			"would: known fields, unique names, references that exist, no host port\n" +
			"conflicts. Secret references are checked for syntax, not looked up, so this runs anywhere.\n" +
			"--values renders any document containing \"{{\" against the given file(s) first, the same\n" +
			"way a host's own agent.yaml repositories[].values would.\n" +
			"Exits non-zero on any finding.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			paths := args
			if len(paths) == 0 {
				paths = []string{"."}
			}
			values, err := config.LoadValuesFiles(valuesFiles...)
			if err != nil {
				return err
			}
			ix, findings, err := config.LintPaths(context.Background(), hosts, values, paths...)
			if err != nil {
				if errors.Is(err, config.ErrNoHosts) {
					fmt.Fprintf(cmd.OutOrStdout(), "ok: %d document(s) loaded, but %v\n", countDocs(ix), err)
					return nil
				}
				return err
			}
			if out != "" {
				return out.write(cmd.OutOrStdout(), map[string]any{"documents": countDocs(ix), "hosts": ix.HostNames(), "findings": findings})
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "ok: %d document(s) loaded\n", countDocs(ix))
			if len(findings) == 0 {
				fmt.Fprintf(w, "ok: %d host(s) compile cleanly\n", len(ix.Hosts))
				return nil
			}
			for _, f := range findings {
				fmt.Fprintf(w, "host %s: %s\n", f.Host, f.Err)
			}
			return fmt.Errorf("%d finding(s)", len(findings))
		},
	}
	out.addFlag(cmd)
	cmd.Flags().StringArrayVar(&hosts, "host", nil, "only compile for this host (repeatable; default: every Host defined)")
	cmd.Flags().StringArrayVar(&valuesFiles, "values", nil, "a values file for {{ .Values }} templating (repeatable; later files win)")
	return cmd
}

func countDocs(ix *config.Index) int {
	if ix == nil {
		return 0
	}
	return len(ix.Applications) + len(ix.Pods) + len(ix.Hosts) + len(ix.Groups) + len(ix.Environments) + len(ix.ConfigMaps) + len(ix.Secrets)
}
