package config

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	sigyaml "sigs.k8s.io/yaml"

	"github.com/podcd/podcd/pkg/model"
)

// SecretProvisioner fetches a Secret that an ExternalSecret declares but Git
// does not contain.
//
// It is asked only for the names a workload on this host actually references,
// so a host never reaches out to a secret store on behalf of somebody else's
// machine. ok is false when no ExternalSecret targets that name.
type SecretProvisioner interface {
	ProvisionSecret(ctx context.Context, name string) (sec corev1.Secret, ok bool, err error)
	// AddExternalSecrets makes the ExternalSecrets a host's templates rendered
	// to known alongside the plain ones. Templates render per host, inside
	// Resolve, so these cannot be handed over any earlier.
	AddExternalSecrets(docs map[string]Doc[ExternalSecretSpec])
}

// ProvisioningError means one application's ExternalSecret could not be
// materialized. Unlike an invalid document, this is transient: callers can
// safely reconcile the other applications and retry this one later.
type ProvisioningError struct {
	Applications map[string]error
}

func (e *ProvisioningError) Error() string {
	var errs []error
	for _, app := range slices.Sorted(maps.Keys(e.Applications)) {
		errs = append(errs, fmt.Errorf("application %q: %w", app, e.Applications[app]))
	}
	return errors.Join(errs...).Error()
}

// ResolveOptions controls one compilation of the index for one host.
type ResolveOptions struct {
	// Host is the identity of this host. It must match a Host document.
	Host string
	// Secrets resolves ExternalSecret targets on demand. May be nil, in which
	// case only Secrets defined in Git are available - that is what `podcd lint`
	// does, since linting must not talk to Vault.
	Secrets SecretProvisioner
	// Revisions is repo name -> commit, carried into the desired state for reporting.
	Revisions map[string]string
	// Values is the lowest-precedence input to {{ .Values }} templating
	Values Values
}

// Resolve compiles the index into the desired state for one host.
//
// Precedence, lowest to highest: the Application document, the Environment's override for it, each Group's override (in the order the Host lists its groups), then the Host's own override.
// The output is sorted, so the same commit always compiles to the same bytes.
func (ix *Index) Resolve(ctx context.Context, opts ResolveOptions) (model.DesiredState, error) {
	var zero model.DesiredState
	if opts.Host == "" {
		return zero, errors.New("no host identity: cannot decide what should run here")
	}
	host, ok := ix.Hosts[opts.Host]
	if !ok {
		known := ix.HostNames()
		if len(known) == 0 {
			return zero, fmt.Errorf("no Host document matches %q (no Host documents were found at all)", opts.Host)
		}
		return zero, fmt.Errorf("no Host document matches %q (known hosts: %s)", opts.Host, strings.Join(known, ", "))
	}

	// Layers, lowest precedence first. selected is the union of what the
	// layers name; the output is sorted, so nothing downstream depends on
	// the order documents happened to be written in.
	var layers []layer
	selected := map[string]bool{}
	addLayer := func(label, repo string, spec SelectionSpec) error {
		for _, n := range spec.Applications {
			selected[n] = true
		}
		values, err := ix.layerValues(repo, spec.Values)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		layers = append(layers, layer{label, spec.Overrides, spec.NetworkOverrides, values})
		return nil
	}

	if e := host.Spec.Environment; e != "" {
		env, ok := ix.Environments[e]
		if !ok {
			return zero, fmt.Errorf("%s: environment %q is not defined (known: %s)",
				host.Source, e, strings.Join(slices.Sorted(maps.Keys(ix.Environments)), ", "))
		}
		if err := addLayer("environment/"+e, env.Source.Repo, env.Spec); err != nil {
			return zero, err
		}
	}
	for _, g := range host.Spec.Groups {
		group, ok := ix.Groups[g]
		if !ok {
			return zero, fmt.Errorf("%s: group %q is not defined (known: %s)",
				host.Source, g, strings.Join(slices.Sorted(maps.Keys(ix.Groups)), ", "))
		}
		if err := addLayer("group/"+g, group.Source.Repo, group.Spec); err != nil {
			return zero, err
		}
	}
	hostSelection := SelectionSpec{Applications: host.Spec.Applications, Overrides: host.Spec.Overrides, NetworkOverrides: host.Spec.NetworkOverrides, Values: host.Spec.Values}
	if err := addLayer("host/"+opts.Host, host.Source.Repo, hostSelection); err != nil {
		return zero, err
	}

	// Values apply in the same precedence as overrides, plus one layer below
	// all of them: agent.yaml's own fallback.
	values := opts.Values
	for _, l := range layers {
		values = MergeValues(values, l.values)
	}

	for _, ex := range host.Spec.ExcludeApplications {
		if !selected[ex] {
			return zero, fmt.Errorf("%s: excludeApplications lists %q, which this host does not run anyway",
				host.Source, ex)
		}
		delete(selected, ex)
	}

	// Templates render for this host's values, into a second set of
	// documents that sits on top of the plain ones. Only now: a template's
	// kind and name are whatever it renders to.
	rendered, err := ix.renderTemplates(values)
	if err != nil {
		return zero, err
	}
	docs := hostDocuments{ix, rendered}
	if opts.Secrets != nil {
		opts.Secrets.AddExternalSecrets(rendered.ExternalSecrets)
	}

	var p problems
	var apps []model.Application
	provisioningFailures := map[string]error{}
	for _, name := range slices.Sorted(maps.Keys(selected)) {
		// A pod manifest and a podcd Application are compiled by different
		// code, but they land in the same canonical type and the same plan.
		var (
			app     model.Application
			src     Source
			origins []string
			err     error
		)
		doc, ok := docs.pod(name)
		if !ok {
			p.add("application %q is referenced by host %q but no Pod defines it", name, opts.Host)
			continue
		}
		pod := doc.Spec
		src = doc.Source
		origins, err = overlay(layers, podOverrides, name, "pod", src, func(o Override) (e error) {
			pod, e = patchPod(pod, o)
			return e
		})
		if err == nil {
			app, err = docs.podToApplication(ctx, name, pod, opts.Secrets)
		}
		if err != nil {
			if isProvisioningFailure(err) {
				provisioningFailures[name] = err
				continue
			}
			p.errs = append(p.errs, err)
			continue
		}
		app.SourceRepo = src.Repo
		app.Origins = origins
		apps = append(apps, app)
	}

	// A network is wanted here exactly when an application here joins it.
	// One with a Network document is podcd's to create, and the application
	// records that so its unit can depend on the network's unit. One without
	// is expected to exist already - created by hand, or podman's own - and
	// podcd neither creates nor removes it.
	var networks []model.Network
	seenNet := map[string]bool{}
	for i := range apps {
		for _, n := range apps[i].Networks {
			doc, ok := docs.network(n)
			if !ok {
				continue
			}
			apps[i].ManagedNetworks = append(apps[i].ManagedNetworks, n)
			if seenNet[n] {
				continue
			}
			seenNet[n] = true
			net, err := networkFromDoc(doc, layers)
			if err != nil {
				p.errs = append(p.errs, err)
				continue
			}
			networks = append(networks, net)
		}
	}
	slices.SortFunc(networks, func(a, b model.Network) int { return cmp.Compare(a.Name, b.Name) })

	// The same rule as for applications: an override for a network no
	// document defines is a mistake, and a host may only override a network
	// it actually uses.
	for _, l := range layers {
		for name := range l.networkOverrides {
			switch {
			case seenNet[name]:
			case strings.HasPrefix(l.label, "host/"):
				p.add("%s overrides network %q, which no application on host %q joins", l.label, name, opts.Host)
			default:
				if _, ok := docs.network(name); !ok {
					p.add("%s overrides network %q, but no Network defines it", l.label, name)
				}
			}
		}
	}

	// An override for an application nobody defines is a mistake;
	// An environment or group may override an application only some of its members run\
	// A host will have an list of its own applications.
	// so a host override for something it does not run is a mistake.
	for _, l := range layers {
		for name := range l.overrides {
			switch {
			case selected[name]:
			case strings.HasPrefix(l.label, "host/"):
				p.add("%s overrides application %q, which host %q does not run", l.label, name, opts.Host)
			default:
				if _, ok := docs.pod(name); !ok {
					p.add("%s overrides application %q, but no Pod defines it", l.label, name)
				}
			}
		}
	}
	p.errs = append(p.errs, checkHostPortConflicts(apps)...)
	if err := p.err(); err != nil {
		return zero, err
	}

	desired := model.DesiredState{
		Host:         opts.Host,
		Environment:  host.Spec.Environment,
		Groups:       slices.Clone(host.Spec.Groups),
		Revisions:    opts.Revisions,
		Applications: apps,
		Networks:     networks,
	}
	if len(provisioningFailures) > 0 {
		return desired, &ProvisioningError{Applications: provisioningFailures}
	}
	return desired, nil
}

func isProvisioningFailure(err error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !isProvisioningFailure(child) {
				return false
			}
		}
		return true
	}
	var provisioningErr *SecretProvisionError
	return errors.As(err, &provisioningErr)
}

// layer is one source of overrides and values, lowest precedence first: the
// environment, then each group in the order the host lists them, then the
// host itself.
type layer struct {
	label            string
	overrides        map[string]Override
	networkOverrides map[string]Override
	values           Values
}

// podOverrides and networkOverrides pick which of a layer's override maps
// overlay walks.
func podOverrides(l layer) map[string]Override     { return l.overrides }
func networkOverrides(l layer) map[string]Override { return l.networkOverrides }

// layerValues loads and merges a layer's own values files, in list order,
// resolved against the repository the document naming them came from - the
// only repository a bare path in that document could sensibly mean.
func (ix *Index) layerValues(repo string, files []string) (Values, error) {
	values := Values{}
	for _, f := range files {
		v, err := ix.readValuesFile(repo, f)
		if err != nil {
			return nil, err
		}
		values = MergeValues(values, v)
	}
	return values, nil
}

// renderTemplates renders every template for one host's values and decodes
// the result through the same path plain documents take, into a second set
// of documents. Everything about a template is decided here and nowhere
// earlier: its kind, its name, whether it is valid. A rendered document
// may not reuse the name of a plain one (or of another rendered one) - the
// same rule as two files defining the same thing.
func (ix *Index) renderTemplates(values Values) (*documents, error) {
	rendered := newDocuments()
	for _, tpl := range ix.templates {
		out, err := renderTemplate(tpl.String(), tpl.Raw, values)
		if err != nil {
			return nil, err
		}
		if err := rendered.addDocuments(tpl.Repo, tpl.Path, out, true); err != nil {
			return nil, err
		}
	}
	if err := ix.checkNoOverlap(&rendered); err != nil {
		return nil, err
	}
	return &rendered, nil
}

// checkNoOverlap is put's "defined twice" rule across the two sets.
func (ix *Index) checkNoOverlap(rendered *documents) error {
	for name, doc := range rendered.Pods {
		if prev, ok := ix.Pods[name]; ok {
			return fmt.Errorf("Pod %q is defined twice: %s and %s (rendered)", name, prev.Source, doc.Source)
		}
	}
	for name, doc := range rendered.ConfigMaps {
		if prev, ok := ix.ConfigMaps[name]; ok {
			return fmt.Errorf("ConfigMap %q is defined twice: %s and %s (rendered)", name, prev.Source, doc.Source)
		}
	}
	for name, doc := range rendered.Secrets {
		if prev, ok := ix.Secrets[name]; ok {
			return fmt.Errorf("Secret %q is defined twice: %s and %s (rendered)", name, prev.Source, doc.Source)
		}
	}
	for name, doc := range rendered.ExternalSecrets {
		if prev, ok := ix.ExternalSecrets[name]; ok {
			return fmt.Errorf("ExternalSecret %q is defined twice: %s and %s (rendered)", name, prev.Source, doc.Source)
		}
	}
	for name, doc := range rendered.Networks {
		if prev, ok := ix.Networks[name]; ok {
			return fmt.Errorf("Network %q is defined twice: %s and %s (rendered)", name, prev.Source, doc.Source)
		}
	}
	return nil
}

// hostDocuments is every document one particular host can see: the plain
// ones every host shares (the Index), plus the ones this host's templates
// rendered to with its own values. Two hosts get two different sets from the
// same templates - that is the point of templating. Lookups try the plain
// set first; checkNoOverlap has already made sure a name cannot be in both.
type hostDocuments struct {
	*Index
	rendered *documents
}

func (h hostDocuments) pod(name string) (Doc[corev1.Pod], bool) {
	if d, ok := h.Pods[name]; ok {
		return d, true
	}
	d, ok := h.rendered.Pods[name]
	return d, ok
}

func (h hostDocuments) configMap(name string) (Doc[corev1.ConfigMap], bool) {
	if d, ok := h.ConfigMaps[name]; ok {
		return d, true
	}
	d, ok := h.rendered.ConfigMaps[name]
	return d, ok
}

func (h hostDocuments) network(name string) (Doc[NetworkSpec], bool) {
	if d, ok := h.Networks[name]; ok {
		return d, true
	}
	d, ok := h.rendered.Networks[name]
	return d, ok
}

// networkFromDoc turns a Network document, with every layer's override for
// it applied in precedence order, into the canonical network. The spec is
// already the model's shape; what is checked here is the name, which becomes
// a unit file name and a podman network name.
func networkFromDoc(doc Doc[NetworkSpec], layers []layer) (model.Network, error) {
	if !validName(doc.Name) {
		return model.Network{}, fmt.Errorf("%s: network %q: name must be lowercase letters, digits and dashes", doc.Source, doc.Name)
	}
	sp := doc.Spec
	origins, err := overlay(layers, networkOverrides, doc.Name, "network", doc.Source, func(o Override) error {
		return patchNetwork(&sp, o)
	})
	if err != nil {
		return model.Network{}, err
	}
	return model.Network{
		Name:       doc.Name,
		Driver:     sp.Driver,
		Subnet:     sp.Subnet,
		Gateway:    sp.Gateway,
		IPRange:    sp.IPRange,
		Internal:   sp.Internal,
		IPv6:       sp.IPv6,
		DisableDNS: sp.DisableDNS,
		DNS:        slices.Clone(sp.DNS),
		Options:    maps.Clone(sp.Options),
		SourceRepo: doc.Source.Repo,
		Origins:    origins,
	}, nil
}

// patchNetwork merges one override into a network spec. Decoding into the
// existing value is the merge: a field the override names is set, one it
// does not name is kept, and options gains and replaces keys rather than
// starting over. Strict, so a misspelled field is refused like anywhere else.
func patchNetwork(sp *NetworkSpec, o Override) error {
	if len(bytes.TrimSpace(o)) == 0 {
		return nil
	}
	if err := sigyaml.UnmarshalStrict(o, sp); err != nil {
		return err
	}
	return nil
}

func (h hostDocuments) secret(name string) (Doc[corev1.Secret], bool) {
	if d, ok := h.Secrets[name]; ok {
		return d, true
	}
	d, ok := h.rendered.Secrets[name]
	return d, ok
}

// externalSecretDeclares reports whether some ExternalSecret promises to produce
// a Secret of this name at reconcile time.
//
// It answers from Git alone, without fetching anything, which is what `podcd
// lint` needs: linting runs with no provisioner so that validating a repository
// never reaches out to Vault, and without this every ExternalSecret-backed
// Secret would look undefined.
func (h hostDocuments) externalSecretDeclares(name string) bool {
	declares := func(docs map[string]Doc[ExternalSecretSpec]) bool {
		for esName, es := range docs {
			target := es.Spec.Target.Name
			if target == "" {
				target = esName
			}
			if target == name {
				return true
			}
		}
		return false
	}
	return declares(h.ExternalSecrets) || declares(h.rendered.ExternalSecrets)
}

// overlay applies every layer's override for name, in order, and returns the
// provenance trail: the document's source, then each layer that touched it.
// pick says which of a layer's override maps is meant.
func overlay(layers []layer, pick func(layer) map[string]Override, name, kind string, src Source, apply func(Override) error) ([]string, error) {
	origins := []string{src.String()}
	for _, l := range layers {
		o, ok := pick(l)[name]
		if !ok {
			continue
		}
		if err := apply(o); err != nil {
			return nil, fmt.Errorf("%s: override for %s %q: %w", l.label, kind, name, err)
		}
		origins = append(origins, l.label)
	}
	return origins, nil
}

// checkHostPortConflicts catches two applications publishing the same host port,
// which would otherwise show up as a container that starts and immediately dies.
func checkHostPortConflicts(apps []model.Application) []error {
	type key struct {
		ip    string
		port  int
		proto string
	}
	seen := map[key]string{}
	var problems []error
	for _, a := range apps {
		for _, p := range a.Ports {
			k := key{p.HostIP, p.Host, p.Protocol}
			if other, ok := seen[k]; ok {
				problems = append(problems, fmt.Errorf("applications %q and %q both publish host port %d/%s", other, a.Name, p.Host, p.Protocol))
				continue
			}
			seen[k] = a.Name
		}
	}
	return problems
}

func comparePorts(a, b model.Port) int {
	return cmp.Or(
		cmp.Compare(a.Host, b.Host),
		cmp.Compare(a.Container, b.Container),
		cmp.Compare(a.Protocol, b.Protocol),
		cmp.Compare(a.HostIP, b.HostIP),
	)
}

func validName(s string) bool {
	if s == "" || len(s) > 60 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-' && i > 0 && i < len(s)-1:
		default:
			return false
		}
	}
	return true
}
