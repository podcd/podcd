package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/podcd/podcd/pkg/identity"
	"github.com/podcd/podcd/pkg/scaffold"
)

// The repository-side commands: they write Git documents, never touch the
// host, and need no agent config.

func newInitCommand() *cobra.Command {
	var host string
	var force bool
	cmd := &cobra.Command{
		Use:   "init [directory]",
		Short: "scaffold a minimal repository",
		Long: "Writes apps.yaml, hosts.yaml and a README into the directory (default: current working dir).\n" +
			"The Host is named after this machine unless --host is given, so `podcd validate` against\n" +
			"the result works as soon as the repository is pushed.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			if host == "" {
				id, err := identity.Resolve("")
				if err != nil {
					return err
				}
				host = id.Host
			}
			written, err := scaffold.Init(dir, host, force)
			if err != nil {
				return err
			}
			abs, _ := filepath.Abs(dir)
			for _, f := range written {
				fmt.Fprintln(cmd.OutOrStdout(), filepath.Join(abs, f))
			}
			if _, err := os.Stat(filepath.Join(dir, ".git")); os.IsNotExist(err) {
				fmt.Fprintf(cmd.ErrOrStderr(), "\n%s is not a git repository yet: git init && git add . && git commit -m 'podcd init'\n", abs)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&host, "host", "", "name of the Host document (default: this machine's hostname)")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite existing files")
	return cmd
}

func newCreateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create KIND NAME [flags]",
		Short: "print a validated document for the repository",
		Long: "Builds one document from flags, per kind. It is checked with the same loader the agent\n" +
			"uses and printed; put it where you want it (`>> apps.yaml`) and run `podcd lint`.",
		Example: "  podcd create pod api --image ghcr.io/you/api:1.2.3 --port 8081:8080 >> apps.yaml",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newCreatePod(), newCreateNetwork(), newCreateHost(), newCreateGroup(), newCreateEnvironment())
	return cmd
}

// emit prints documents as one YAML stream. A leading separator is written
// before every document, so appending the output to a file that already has
// documents in it gives a valid stream.
func emit(cmd *cobra.Command, docs ...[]byte) error {
	for _, d := range docs {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "---\n%s", d); err != nil {
			return err
		}
	}
	return nil
}

func newCreatePod() *cobra.Command {
	var o scaffold.PodOptions
	cmd := &cobra.Command{
		Use:   "pod NAME --image IMAGE [--port HOST:CONTAINER]... [--env KEY=VALUE]...",
		Short: "a core/v1 Pod with one container, played by podman",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			o.Name = args[0]
			doc, err := scaffold.Pod(o)
			if err != nil {
				return err
			}
			return emit(cmd, doc)
		},
	}
	cmd.Flags().StringVar(&o.Image, "image", "", "container image (required)")
	cmd.Flags().StringArrayVar(&o.Ports, "port", nil, "host:container port to publish on 127.0.0.1 (repeatable)")
	cmd.Flags().StringArrayVar(&o.Env, "env", nil, "KEY=value environment variable (repeatable)")
	_ = cmd.MarkFlagRequired("image")
	return cmd
}

func newCreateNetwork() *cobra.Command {
	var o scaffold.NetworkOptions
	cmd := &cobra.Command{
		Use:     "network NAME [--subnet CIDR] [--gateway IP] [--internal] [--dns IP]... [--opt KEY=VALUE]...",
		Aliases: []string{"net"},
		Short:   "a Network: a podman network the pods that name it join",
		Long: "A Network is created on a host when a Pod there names it in its io.podcd.networks\n" +
			"annotation, and removed when none does. Every flag is optional: with none, podman picks\n" +
			"the subnet the way it does for `podman network create NAME`.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			o.Name = args[0]
			doc, err := scaffold.Network(o)
			if err != nil {
				return err
			}
			return emit(cmd, doc)
		},
	}
	cmd.Flags().StringVar(&o.Driver, "driver", "", "bridge (the default), macvlan or ipvlan")
	cmd.Flags().StringVar(&o.Subnet, "subnet", "", "subnet in CIDR notation")
	cmd.Flags().StringVar(&o.Gateway, "gateway", "", "gateway address within the subnet")
	cmd.Flags().StringVar(&o.IPRange, "ip-range", "", "range within the subnet to allocate from")
	cmd.Flags().BoolVar(&o.Internal, "internal", false, "no route out of the host")
	cmd.Flags().BoolVar(&o.IPv6, "ipv6", false, "dual-stack")
	cmd.Flags().StringArrayVar(&o.DNS, "dns", nil, "resolver for containers on the network (repeatable)")
	cmd.Flags().StringArrayVar(&o.Options, "opt", nil, "driver option, key=value (repeatable)")
	return cmd
}

func newCreateHost() *cobra.Command {
	var o scaffold.HostOptions
	cmd := &cobra.Command{
		Use:   "host NAME [--environment ENV] [--group GROUP]... [--app APP]...",
		Short: "a Host: which environment, groups and applications a machine has",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			o.Name = args[0]
			doc, err := scaffold.Host(o)
			if err != nil {
				return err
			}
			return emit(cmd, doc)
		},
	}
	cmd.Flags().StringVar(&o.Environment, "environment", "", "environment the host belongs to")
	cmd.Flags().StringArrayVar(&o.Groups, "group", nil, "group the host belongs to (repeatable, order matters for overrides)")
	cmd.Flags().StringArrayVar(&o.Applications, "app", nil, "application to run on top of the groups (repeatable)")
	return cmd
}

func newCreateGroup() *cobra.Command {
	var apps []string
	cmd := &cobra.Command{
		Use:   "group NAME [--app APP]...",
		Short: "a Group: a role, with the applications every member runs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			doc, err := scaffold.Group(args[0], apps)
			if err != nil {
				return err
			}
			return emit(cmd, doc)
		},
	}
	cmd.Flags().StringArrayVar(&apps, "app", nil, "application every host in the group runs (repeatable)")
	return cmd
}

func newCreateEnvironment() *cobra.Command {
	var apps []string
	cmd := &cobra.Command{
		Use:     "environment NAME [--app APP]...",
		Aliases: []string{"env"},
		Short:   "an Environment: what every host in it runs",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			doc, err := scaffold.Environment(args[0], apps)
			if err != nil {
				return err
			}
			return emit(cmd, doc)
		},
	}
	cmd.Flags().StringArrayVar(&apps, "app", nil, "application every host in the environment runs (repeatable)")
	return cmd
}
