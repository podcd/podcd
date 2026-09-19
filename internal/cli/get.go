package cli

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"

	"github.com/podcd/podcd/pkg/config"
	"github.com/podcd/podcd/pkg/git"
	"github.com/podcd/podcd/pkg/model"
)

// item is one document, flattened for listing: what kind, what name, where
// it is, and its parsed body for -o json|yaml.
type item struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Source string `json:"source"`
	Spec   any    `json:"spec"`
	raw    []byte
	cols   []string // kind-specific table columns
}

// kinds maps every spelling a user might type to a canonical kind.
var kinds = map[string]string{
	"pod": config.KindPod, "pods": config.KindPod, "app": config.KindPod, "apps": config.KindPod,
	"host": config.KindHost, "hosts": config.KindHost,
	"group": config.KindGroup, "groups": config.KindGroup,
	"environment": config.KindEnvironment, "environments": config.KindEnvironment, "env": config.KindEnvironment, "envs": config.KindEnvironment,
	"configmap": config.KindConfigMap, "configmaps": config.KindConfigMap, "cm": config.KindConfigMap,
	"secret": config.KindSecret, "secrets": config.KindSecret,
	"network": config.KindNetwork, "networks": config.KindNetwork, "net": config.KindNetwork,
	"all": "",
}

func newGetCommand(f *configFlags) *cobra.Command {
	var out outputFormat
	var repo, revision string
	cmd := &cobra.Command{
		Use:   "get [KIND] [NAME]",
		Short: "list what a repository defines",
		Long: "Lists the documents in every repository the agent config points at, fetched the same way\n" +
			"the agent fetches them. --repo narrows that to one: the name of a repository in the agent\n" +
			"config, a local directory, or a git URL to clone (in that order of precedence).\n" +
			"KIND is one of applications, pods, hosts, groups, environments, networks, configmaps, secrets, or all\n" +
			"(the " +
			"default). With NAME, only that document; add -o yaml to print it as written.",
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, name := "", ""
			if len(args) > 0 {
				k, ok := kinds[strings.ToLower(args[0])]
				if !ok {
					return fmt.Errorf("unknown kind %q (applications, pods, hosts, groups, environments, configmaps, secrets, all)", args[0])
				}
				kind = k
			}
			if len(args) > 1 {
				name = args[1]
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			ix, offline, err := loadIndex(ctx, f, repo, revision)
			if err != nil {
				return err
			}
			for _, r := range offline {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: repository %q could not be refreshed; showing the commit already on disk\n", r)
			}

			items := collect(ix, kind, name)
			if name != "" && len(items) == 0 {
				return fmt.Errorf("no %s named %q", orAll(kind), name)
			}
			switch {
			case out == "yaml" && name != "":
				// One named document: show it as it is in Git.
				_, err := cmd.OutOrStdout().Write(items[0].raw)
				return err
			case out != "":
				return out.write(cmd.OutOrStdout(), items)
			default:
				printItems(cmd.OutOrStdout(), kind, items)
				return nil
			}
		},
	}
	out.addFlag(cmd)
	cmd.Flags().StringVar(&repo, "repo", "", "only this repository: a name from the agent config, a local directory, or a git URL to clone")
	cmd.Flags().StringVar(&revision, "revision", "main", "branch, tag or commit to read when --repo is a git URL")
	return cmd
}

// loadIndex reads the documents. Without --repo the repository in the agent
// config is fetched as the agent would fetch it. With --repo, the value is
// tried as the repository name from the agent config, then as a local
// directory, then as a git URL; the config is only required in the first case.
func loadIndex(ctx context.Context, f *configFlags, repo, revision string) (*config.Index, []string, error) {
	env, setupErr := setup(*f)
	if repo == "" {
		if setupErr != nil {
			return nil, nil, setupErr
		}
		ix, _, _, offline, err := env.engine.Index(ctx)
		return ix, offline, err
	}
	if setupErr == nil && env.cfg.Repository.Name == repo {
		ix, _, _, offline, err := env.engine.Index(ctx, repo)
		return ix, offline, err
	}

	// One explicit repository needs no label in front of its paths.
	ix := config.NewIndex()
	if info, err := os.Stat(repo); err == nil && info.IsDir() {
		return ix, nil, ix.LoadTree("", repo)
	}
	if !strings.Contains(repo, "://") && !strings.HasPrefix(repo, "git@") {
		return nil, nil, fmt.Errorf("%s: %s", repo, notARepo(env, setupErr))
	}
	tmp, err := os.MkdirTemp("", "podcd-get-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(tmp)
	r := git.New("repo", repo, revision, "", tmp)
	if _, err := r.Sync(ctx); err != nil {
		return nil, nil, err
	}
	return ix, nil, ix.LoadTree("", r.TreePath())
}

// notARepo explains what --repo could have been, naming the configured
// repository when there is a config to name it from.
func notARepo(env *environment, setupErr error) string {
	if setupErr != nil {
		return fmt.Sprintf("not a directory or git URL, and the agent config could not be read to look it up by name (%v)", setupErr)
	}
	return fmt.Sprintf("not the configured repository (%s), a directory, or a git URL", env.cfg.Repository.Name)
}

// collect flattens the index into sorted items, filtered by kind and name.
func collect(ix *config.Index, kind, name string) []item {
	var items []item
	add := func(k, n string, src config.Source, spec any, cols ...string) {
		if (kind != "" && kind != k) || (name != "" && name != n) {
			return
		}
		items = append(items, item{Kind: k, Name: n, Source: src.String(), Spec: spec, raw: src.Raw, cols: cols})
	}
	for n, d := range ix.Pods {
		add(config.KindPod, n, d.Source, d.Spec.Spec, containerNames(d.Spec), podPorts(d.Spec))
	}
	for n, d := range ix.Hosts {
		add(config.KindHost, n, d.Source, d.Spec, dash(d.Spec.Environment), dash(strings.Join(d.Spec.Groups, ",")), dash(strings.Join(d.Spec.Applications, ",")))
	}
	for n, d := range ix.Groups {
		add(config.KindGroup, n, d.Source, d.Spec, dash(strings.Join(d.Spec.Applications, ",")), strconv.Itoa(len(d.Spec.Overrides)))
	}
	for n, d := range ix.Environments {
		add(config.KindEnvironment, n, d.Source, d.Spec, dash(strings.Join(d.Spec.Applications, ",")), strconv.Itoa(len(d.Spec.Overrides)))
	}
	for n, d := range ix.Networks {
		add(config.KindNetwork, n, d.Source, d.Spec, dash(d.Spec.Driver), dash(d.Spec.Subnet))
	}
	for n, d := range ix.ConfigMaps {
		add(config.KindConfigMap, n, d.Source, d.Spec.Data, dash(strings.Join(slices.Sorted(maps.Keys(d.Spec.Data)), ",")))
	}
	for n, d := range ix.Secrets {
		// StringData holds references, never values, so listing keys and
		// references is safe.
		add(config.KindSecret, n, d.Source, d.Spec.StringData, dash(strings.Join(slices.Sorted(maps.Keys(d.Spec.StringData)), ",")))
	}
	slices.SortFunc(items, func(a, b item) int {
		return cmp.Or(cmp.Compare(kindOrder(a.Kind), kindOrder(b.Kind)), cmp.Compare(a.Name, b.Name))
	})
	return items
}

// headers are the kind-specific columns shown after NAME.
var headers = map[string][]string{
	config.KindPod:         {"CONTAINERS", "PORTS"},
	config.KindHost:        {"ENVIRONMENT", "GROUPS", "APPLICATIONS"},
	config.KindGroup:       {"APPLICATIONS", "OVERRIDES"},
	config.KindEnvironment: {"APPLICATIONS", "OVERRIDES"},
	config.KindNetwork:     {"DRIVER", "SUBNET"},
	config.KindConfigMap:   {"KEYS"},
	config.KindSecret:      {"KEYS"},
}

func printItems(w io.Writer, kind string, items []item) {
	if len(items) == 0 {
		fmt.Fprintf(w, "no %s defined\n", orAll(kind))
		return
	}
	tw := table(w)
	if kind == "" {
		// Mixed kinds: the columns they all share.
		fmt.Fprintln(tw, "KIND\tNAME\tSOURCE")
		for _, it := range items {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", it.Kind, it.Name, it.Source)
		}
	} else {
		fmt.Fprintln(tw, "NAME\t"+strings.Join(headers[kind], "\t")+"\tSOURCE")
		for _, it := range items {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", it.Name, strings.Join(it.cols, "\t"), it.Source)
		}
	}
	tw.Flush()
}

func kindOrder(k string) int {
	for i, o := range []string{config.KindEnvironment, config.KindGroup, config.KindHost, config.KindNetwork, config.KindPod, config.KindConfigMap, config.KindSecret} {
		if o == k {
			return i
		}
	}
	return 99
}

func orAll(kind string) string {
	if kind == "" {
		return "documents"
	}
	return strings.ToLower(kind) + "s"
}

func containerNames(pod corev1.Pod) string {
	names := make([]string, 0, len(pod.Spec.Containers))
	for _, c := range pod.Spec.Containers {
		names = append(names, c.Name)
	}
	return strings.Join(names, ",")
}

func podPorts(pod corev1.Pod) string {
	var ports []model.Port
	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			if p.HostPort > 0 {
				ports = append(ports, model.Port{Host: int(p.HostPort), Container: int(p.ContainerPort)})
			}
		}
	}
	return portList(ports)
}
