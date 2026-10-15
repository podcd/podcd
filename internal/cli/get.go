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

// item is one document flattened for listing.
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
	"pod": config.KindPod, "pods": config.KindPod,
	"host": config.KindHost, "hosts": config.KindHost,
	"group": config.KindGroup, "groups": config.KindGroup,
	"environment": config.KindEnvironment, "environments": config.KindEnvironment, "env": config.KindEnvironment, "envs": config.KindEnvironment,
	"configmap": config.KindConfigMap, "configmaps": config.KindConfigMap, "cm": config.KindConfigMap,
	"secret": config.KindSecret, "secrets": config.KindSecret,
	"secretstore": config.KindSecretStore, "secretstores": config.KindSecretStore,
	"externalsecret": config.KindExternalSecret, "externalsecrets": config.KindExternalSecret,
	"network": config.KindNetwork, "networks": config.KindNetwork, "net": config.KindNetwork,
	"all": "",
}

func newGetCommand(f *configFlags) *cobra.Command {
	var out outputFormat
	var repo, revision string
	var all bool
	cmd := &cobra.Command{
		Use:   "get [KIND] [NAME]",
		Short: "list what a repository defines",
		Long: "Lists the documents in every repository the agent config points at, fetched the same way\n" +
			"the agent fetches them. --repo narrows that to one: the name of a repository in the agent\n" +
			"config, a local directory, or a git URL to clone (in that order of precedence).\n" +
			"KIND is one of pods, hosts, groups, environments, networks, configmaps, secrets,\n" +
			"secretstores, externalsecrets, or all\n" +
			"(the default). With NAME, only that document; add -o yaml to print it as written\n" +
			"(a template as rendered).\n\n" +
			"With a host (the agent's own for its repository, --host for any) get lists only what\n" +
			"that host uses, *.tpl files rendered with its values; --all lists every host's, and\n" +
			"`get hosts` always does. Without one, everything the repository defines. A --repo\n" +
			"directory or URL renders without agent.yaml's values.",
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, name := "", ""
			if len(args) > 0 {
				k, ok := kinds[strings.ToLower(args[0])]
				if !ok {
					return fmt.Errorf("unknown kind %q (pods, hosts, groups, environments, networks, configmaps, secrets, secretstores, externalsecrets, all)", args[0])
				}
				kind = k
			}
			if len(args) > 1 {
				name = args[1]
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			src, err := loadIndex(ctx, f, repo, revision)
			if err != nil {
				return err
			}
			for _, r := range src.offline {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: repository %q could not be refreshed; showing the commit already on disk\n", r)
			}
			// With a host, templates render with its values and the listing narrows to
			// what it uses. An explicit --host must exist; an identity without a Host
			// document lists the repository unrendered and unfiltered.
			docs := []*config.Documents{&src.ix.Documents}
			var used map[string]map[string]bool
			unrendered, why := len(src.ix.Templates()), error(nil)
			if src.host != "" {
				v, err := src.ix.View(ctx, src.host, src.values, src.secrets)
				switch {
				case v == nil && f.host != "":
					return err
				case v == nil:
					why = err
				default:
					// Only a failure that touches this kind is worth a warning.
					if err != nil && (kind == "" || v.Used[kind] == nil) {
						fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", err)
					}
					if v.Rendered != nil {
						docs = append(docs, v.Rendered)
					}
					unrendered = 0 // rendered, or the warning above says why not
					// `get hosts` is how to find the others, so it is never narrowed.
					if !all && kind != config.KindHost {
						used = v.Used
					}
				}
			}
			if unrendered > 0 && templatable(kind) {
				if why != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: %d template(s) not listed: %v; pass --host to render them with another host's values\n", unrendered, why)
				} else {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: %d template(s) not listed: pass --host to render them with that host's values\n", unrendered)
				}
			}

			items := slices.DeleteFunc(collect(docs, kind, name), func(it item) bool {
				set, ok := used[it.Kind]
				return ok && !set[it.Name]
			})
			if name != "" && len(items) == 0 {
				switch {
				case used[kind] != nil:
					return fmt.Errorf("no %s named %q that %s uses (--all lists every host's)", orAll(kind), name, src.host)
				case unrendered > 0:
					return fmt.Errorf("no %s named %q among the plain documents; %d template(s) were not rendered, pass --host", orAll(kind), name, unrendered)
				}
				return fmt.Errorf("no %s named %q", orAll(kind), name)
			}
			switch {
			case out == "yaml" && name != "":
				// One named document: as it is in Git, or as rendered for a template.
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
	cmd.Flags().BoolVarP(&all, "all", "a", false, "every host's documents, not only what the host uses")
	return cmd
}

// templatable reports whether a template may render kind ("" is all).
func templatable(kind string) bool {
	switch kind {
	case "", config.KindPod, config.KindConfigMap, config.KindSecret, config.KindExternalSecret, config.KindNetwork:
		return true
	}
	return false
}

// source is what get reads: the index, agent.yaml's values and registry
// Secrets, the host it renders and filters for, and repositories found offline.
type source struct {
	ix      *config.Index
	values  config.Values
	secrets []string
	host    string
	offline []string
}

// loadIndex reads the agent config's repository, or --repo as a configured
// repository name, a local directory, or a git URL. The host is the agent's
// identity for its own repository, only --host for any other.
func loadIndex(ctx context.Context, f *configFlags, repo, revision string) (source, error) {
	env, setupErr := setup(*f)
	if repo == "" && setupErr != nil {
		return source{}, setupErr
	}
	if repo == "" || setupErr == nil && env.cfg.Repository.Name == repo {
		var only []string
		if repo != "" {
			only = []string{repo}
		}
		ix, values, _, offline, err := env.engine.Index(ctx, only...)
		var secrets []string
		for _, r := range env.cfg.Registries {
			if r.SecretRef != "" {
				secrets = append(secrets, r.SecretRef)
			}
		}
		return source{ix: ix, values: values, secrets: secrets, host: env.engine.Identity().Host, offline: offline}, err
	}

	// One explicit repository needs no label in front of its paths.
	ix := config.NewIndex()
	if info, err := os.Stat(repo); err == nil && info.IsDir() {
		return source{ix: ix, host: f.host}, ix.LoadTree("", repo)
	}
	if !strings.Contains(repo, "://") && !strings.HasPrefix(repo, "git@") {
		return source{}, fmt.Errorf("%s: %s", repo, notARepo(env, setupErr))
	}
	tmp, err := os.MkdirTemp("", "podcd-get-")
	if err != nil {
		return source{}, err
	}
	defer os.RemoveAll(tmp)
	r := git.New("repo", repo, revision, "", tmp)
	if _, err := r.Sync(ctx); err != nil {
		return source{}, err
	}
	return source{ix: ix, host: f.host}, ix.LoadTree("", r.TreePath())
}

// notARepo explains what --repo accepts.
func notARepo(env *environment, setupErr error) string {
	if setupErr != nil {
		return fmt.Sprintf("not a directory or git URL, and the agent config could not be read to look it up by name (%v)", setupErr)
	}
	return fmt.Sprintf("not the configured repository (%s), a directory, or a git URL", env.cfg.Repository.Name)
}

// collect flattens document sets into sorted items, filtered by kind and name.
func collect(sets []*config.Documents, kind, name string) []item {
	var items []item
	add := func(k, n string, src config.Source, spec any, cols ...string) {
		if (kind != "" && kind != k) || (name != "" && name != n) {
			return
		}
		items = append(items, item{Kind: k, Name: n, Source: src.String(), Spec: spec, raw: src.Raw, cols: cols})
	}
	for _, ix := range sets {
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
			add(config.KindConfigMap, n, d.Source, d.Spec.Data, keyList(slices.Sorted(maps.Keys(d.Spec.Data))))
		}
		for n, d := range ix.Secrets {
			// StringData holds references, not values.
			add(config.KindSecret, n, d.Source, d.Spec.StringData, keyList(slices.Sorted(maps.Keys(d.Spec.StringData))))
		}
		for n, d := range ix.SecretStores {
			add(config.KindSecretStore, n, d.Source, d.Spec, storeProvider(d.Spec.Provider))
		}
		for n, d := range ix.ExternalSecrets {
			// Secret key names only; values are fetched at reconcile, never here.
			keys := make([]string, 0, len(d.Spec.Data)+len(d.Spec.DataFrom))
			for _, k := range d.Spec.Data {
				keys = append(keys, k.SecretKey)
			}
			for _, k := range d.Spec.DataFrom {
				keys = append(keys, "all of "+k.Extract.Key)
			}
			add(config.KindExternalSecret, n, d.Source, d.Spec, d.Spec.SecretStoreRef.Name, cmp.Or(d.Spec.Target.Name, n), keyList(keys))
		}
	}
	slices.SortFunc(items, func(a, b item) int {
		return cmp.Or(cmp.Compare(kindOrder(a.Kind), kindOrder(b.Kind)), cmp.Compare(a.Name, b.Name))
	})
	return items
}

// headers are the kind-specific columns shown after NAME.
var headers = map[string][]string{
	config.KindPod:            {"CONTAINERS", "PORTS"},
	config.KindHost:           {"ENVIRONMENT", "GROUPS", "APPLICATIONS"},
	config.KindGroup:          {"APPLICATIONS", "OVERRIDES"},
	config.KindEnvironment:    {"APPLICATIONS", "OVERRIDES"},
	config.KindNetwork:        {"DRIVER", "SUBNET"},
	config.KindConfigMap:      {"KEYS"},
	config.KindSecret:         {"KEYS"},
	config.KindSecretStore:    {"PROVIDER"},
	config.KindExternalSecret: {"STORE", "TARGET", "KEYS"},
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
	for i, o := range []string{config.KindEnvironment, config.KindGroup, config.KindHost, config.KindNetwork, config.KindPod, config.KindConfigMap, config.KindSecret, config.KindSecretStore, config.KindExternalSecret} {
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

// keyList shows up to three keys and counts the rest, so one wide document keeps the table readable.
func keyList(keys []string) string {
	if len(keys) > 3 {
		return strings.Join(keys[:3], ",") + fmt.Sprintf(" +%d", len(keys)-3)
	}
	return dash(strings.Join(keys, ","))
}

// storeProvider names the one backend a SecretStore configures.
func storeProvider(p config.SecretStoreProvider) string {
	switch {
	case p.Vault != nil:
		return "vault " + p.Vault.Server
	case p.Env != nil:
		return "env"
	case p.File != nil:
		return "file"
	}
	return "-"
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
