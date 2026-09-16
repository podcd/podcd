package config

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/podcd/podcd/pkg/model"
)

// ResolveOptions controls one compilation of the index for one host.
type ResolveOptions struct {
	// Host is the identity of this host. It must match a Host document.
	Host string
	// ProvisionedSecrets is the output of the provision phase: secrets fetched
	// from external stores and ready to be bundled into kube manifests.
	// Keyed by Secret name; takes priority over Git-defined Secrets.
	ProvisionedSecrets map[string]corev1.Secret
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
		layers = append(layers, layer{label, spec.Overrides, values})
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
	hostSelection := SelectionSpec{Applications: host.Spec.Applications, Overrides: host.Spec.Overrides, Values: host.Spec.Values}
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

	var p problems
	var apps []model.Application
	for _, name := range slices.Sorted(maps.Keys(selected)) {
		// A pod manifest and a podcd Application are compiled by different
		// code, but they land in the same canonical type and the same plan.
		var (
			app     model.Application
			src     Source
			origins []string
			err     error
		)
		if doc, ok := docs.pod(name); ok {
			pod := doc.Spec
			src = doc.Source
			origins, err = overlay(layers, name, "pod", src, func(o Override) (e error) {
				pod, e = patchPod(pod, o)
				return e
			})
			if err == nil {
				app, err = docs.podToApplication(ctx, name, pod, opts.ProvisionedSecrets)
			}
		} else if doc, ok := docs.application(name); ok {
			spec := doc.Spec
			src = doc.Source
			origins, err = overlay(layers, name, "application", src, func(o Override) error {
				over, e := decodeAppOverride(o)
				spec = mergeAppSpec(spec, over)
				return e
			})
			if err == nil {
				var pod corev1.Pod
				pod, err = appSpecToPod(name, spec)
				if err == nil {
					app, err = docs.podToApplication(ctx, name, pod, opts.ProvisionedSecrets)
					if err == nil && spec.Resources != nil {
						app.Resources = *spec.Resources
					}
					if err == nil && len(spec.Networks) > 0 {
						app.Networks = slices.Sorted(slices.Values(spec.Networks))
					}
				}
			}
		} else {
			p.add("application %q is referenced by host %q but never defined (as an Application or a Pod)", name, opts.Host)
			continue
		}
		if err != nil {
			p.errs = append(p.errs, err)
			continue
		}
		app.SourceRepo = src.Repo
		app.Origins = origins
		apps = append(apps, app)
	}

	// Overrides that name an application this host does not run are almost
	// always a typo or a stale entry. Say so rather than ignoring them.
	for _, l := range layers {
		for name := range l.overrides {
			if !selected[name] {
				p.add("%s overrides application %q, which host %q does not run", l.label, name, opts.Host)
			}
		}
	}
	p.errs = append(p.errs, checkHostPortConflicts(apps)...)
	if err := p.err(); err != nil {
		return zero, err
	}

	return model.DesiredState{
		Host:         opts.Host,
		Environment:  host.Spec.Environment,
		Groups:       slices.Clone(host.Spec.Groups),
		Revisions:    opts.Revisions,
		Applications: apps,
	}, nil
}

// layer is one source of overrides and values, lowest precedence first: the
// environment, then each group in the order the host lists them, then the
// host itself.
type layer struct {
	label     string
	overrides map[string]Override
	values    Values
}

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

// checkNoOverlap is put's "defined twice" rule across the two sets, plus
// the Application/Pod shared-namespace rule.
func (ix *Index) checkNoOverlap(rendered *documents) error {
	for name, doc := range rendered.Applications {
		if prev, ok := ix.Applications[name]; ok {
			return fmt.Errorf("Application %q is defined twice: %s and %s (rendered)", name, prev.Source, doc.Source)
		}
		if prev, ok := ix.Pods[name]; ok {
			return fmt.Errorf("%q is both a Pod (%s) and an Application (%s, rendered); a host lists both under applications, so the name must be unique", name, prev.Source, doc.Source)
		}
	}
	for name, doc := range rendered.Pods {
		if prev, ok := ix.Pods[name]; ok {
			return fmt.Errorf("Pod %q is defined twice: %s and %s (rendered)", name, prev.Source, doc.Source)
		}
		if prev, ok := ix.Applications[name]; ok {
			return fmt.Errorf("%q is both an Application (%s) and a Pod (%s, rendered); a host lists both under applications, so the name must be unique", name, prev.Source, doc.Source)
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

func (h hostDocuments) application(name string) (Doc[AppSpec], bool) {
	if d, ok := h.Applications[name]; ok {
		return d, true
	}
	d, ok := h.rendered.Applications[name]
	return d, ok
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

func (h hostDocuments) secret(name string) (Doc[corev1.Secret], bool) {
	if d, ok := h.Secrets[name]; ok {
		return d, true
	}
	d, ok := h.rendered.Secrets[name]
	return d, ok
}

// overlay applies every layer's override for name, in order, and returns the
// provenance trail: the document's source, then each layer that touched it.
func overlay(layers []layer, name, kind string, src Source, apply func(Override) error) ([]string, error) {
	origins := []string{src.String()}
	for _, l := range layers {
		o, ok := l.overrides[name]
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

// decodeAppOverride reads an override aimed at an Application, strictly.
func decodeAppOverride(o Override) (AppSpec, error) {
	var spec AppSpec
	if len(o) == 0 {
		return spec, nil
	}
	dec := json.NewDecoder(bytes.NewReader(o))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return spec, err
	}
	return spec, nil
}

// mergeAppSpec layers over on top of base. See AppSpec for the rules.
func mergeAppSpec(base, over AppSpec) AppSpec {
	out := base
	if over.Image != "" {
		out.Image = over.Image
	}
	if over.Command != nil {
		out.Command = over.Command
	}
	if over.Entrypoint != nil {
		out.Entrypoint = over.Entrypoint
	}
	if over.Env != nil {
		out.Env = mergeMap(base.Env, over.Env)
	}
	if over.Labels != nil {
		out.Labels = mergeMap(base.Labels, over.Labels)
	}
	if over.Ports != nil {
		out.Ports = over.Ports
	}
	if over.Volumes != nil {
		out.Volumes = over.Volumes
	}
	if over.Networks != nil {
		out.Networks = over.Networks
	}
	if over.RestartPolicy != "" {
		out.RestartPolicy = over.RestartPolicy
	}
	if over.User != "" {
		out.User = over.User
	}
	if over.WorkingDir != "" {
		out.WorkingDir = over.WorkingDir
	}
	if over.StopTimeout != nil {
		out.StopTimeout = over.StopTimeout
	}
	if over.Healthcheck != nil {
		out.Healthcheck = over.Healthcheck
	}
	if over.Resources != nil {
		out.Resources = over.Resources
	}
	return out
}

func mergeMap(base, over map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(over))
	maps.Copy(out, base)
	maps.Copy(out, over)
	return out
}

// appSpecToPod validates a merged AppSpec and converts it to a corev1.Pod.
// The result is passed to podToApplication, which handles Pod-level validation
// and manifest assembly.
func appSpecToPod(name string, spec AppSpec) (corev1.Pod, error) {
	p := problems{prefix: fmt.Sprintf("application %q: ", name)}

	if !validName(name) {
		p.add("name must be lowercase letters, digits and dashes")
	}
	if spec.Image == "" {
		p.add("no image")
	}

	container := corev1.Container{
		Name:       name,
		Image:      spec.Image,
		Args:       slices.Clone(spec.Command),
		Command:    slices.Clone(spec.Entrypoint),
		WorkingDir: spec.WorkingDir,
	}

	// env vars — sorted for determinism
	for _, k := range slices.Sorted(maps.Keys(spec.Env)) {
		if !validEnvName(k) {
			p.add("environment variable name %q is not valid", k)
			continue
		}
		container.Env = append(container.Env, corev1.EnvVar{Name: k, Value: spec.Env[k]})
	}

	// envFrom (configmap / secret injection)
	for _, ef := range spec.EnvFrom {
		src := corev1.EnvFromSource{Prefix: ef.Prefix}
		if ef.ConfigMapRef != nil {
			src.ConfigMapRef = &corev1.ConfigMapEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: ef.ConfigMapRef.Name},
			}
		}
		if ef.SecretRef != nil {
			src.SecretRef = &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: ef.SecretRef.Name},
			}
		}
		container.EnvFrom = append(container.EnvFrom, src)
	}

	// ports — sort for deterministic manifest
	sortedPorts := slices.Clone(spec.Ports)
	slices.SortFunc(sortedPorts, func(a, b model.Port) int {
		return cmp.Or(cmp.Compare(a.Host, b.Host), cmp.Compare(a.Container, b.Container))
	})
	for _, port := range sortedPorts {
		proto := corev1.Protocol(strings.ToUpper(cmp.Or(port.Protocol, "tcp")))
		if proto != corev1.ProtocolTCP && proto != corev1.ProtocolUDP {
			p.add("port protocol %q must be tcp or udp", port.Protocol)
		}
		if !validPort(port.Host) {
			p.add("host port %d is out of range", port.Host)
		}
		if !validPort(port.Container) {
			p.add("container port %d is out of range", port.Container)
		}
		container.Ports = append(container.Ports, corev1.ContainerPort{
			HostPort:      int32(port.Host),
			ContainerPort: int32(port.Container),
			Protocol:      proto,
			HostIP:        port.HostIP,
		})
	}

	// volumes — sort first for a deterministic manifest regardless of input order
	sortedVols := slices.Clone(spec.Volumes)
	slices.SortFunc(sortedVols, func(a, b model.Volume) int {
		return strings.Compare(a.Destination, b.Destination)
	})
	var podVolumes []corev1.Volume
	for i, v := range sortedVols {
		if v.Source == "" || v.Destination == "" {
			p.add("volume needs both source and destination")
			continue
		}
		if !strings.HasPrefix(v.Destination, "/") {
			p.add("volume destination %q must be an absolute path", v.Destination)
			continue
		}
		volName := fmt.Sprintf("vol-%d", i)
		podVolumes = append(podVolumes, corev1.Volume{
			Name: volName,
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: v.Source},
			},
		})
		mount := corev1.VolumeMount{Name: volName, MountPath: v.Destination}
		for _, opt := range strings.Split(v.Options, ",") {
			if strings.TrimSpace(opt) == "ro" {
				mount.ReadOnly = true
			}
		}
		container.VolumeMounts = append(container.VolumeMounts, mount)
	}

	// user — numeric UID only; kube play does not resolve usernames
	if spec.User != "" {
		uid := spec.User
		if i := strings.Index(uid, ":"); i >= 0 {
			uid = uid[:i]
		}
		n, err := strconv.ParseInt(uid, 10, 64)
		if err != nil {
			p.add("user %q must be a numeric UID (or UID:GID); names are not supported in kube manifests", spec.User)
		} else {
			container.SecurityContext = &corev1.SecurityContext{RunAsUser: &n}
		}
	}

	// healthcheck → readiness probe
	// The AppSpec healthcheck port is a host port; we need the matching container port.
	if hc := spec.Healthcheck; hc != nil {
		set := 0
		if hc.HTTP != nil {
			set++
			if !validPort(hc.HTTP.Port) {
				p.add("healthcheck http port %d is out of range", hc.HTTP.Port)
			}
		}
		if hc.TCP != nil {
			set++
			if !validPort(hc.TCP.Port) {
				p.add("healthcheck tcp port %d is out of range", hc.TCP.Port)
			}
		}
		if hc.Exec != nil {
			set++
			if len(hc.Exec.Command) == 0 {
				p.add("healthcheck exec needs a command")
			}
		}
		if set > 1 {
			p.add("healthcheck must set at most one of http, tcp, exec")
		}
		for _, d := range []string{hc.Interval, probeTimeout(hc)} {
			if d == "" {
				continue
			}
			if err := checkDuration(d); err != nil {
				p.add("healthcheck: %v", err)
			}
		}
		if hc.Retries < 0 {
			p.add("healthcheck retries must not be negative")
		}

		// Build the probe only when validation passed.
		if probe := appHealthcheckToProbe(hc, spec.Ports); probe != nil {
			container.ReadinessProbe = probe
		}
	}

	restartPolicy := appRestartPolicyToKube(cmp.Or(spec.RestartPolicy, "always"))
	switch spec.RestartPolicy {
	case "", "always", "on-failure", "no":
	default:
		p.add("restartPolicy %q must be always, on-failure or no", spec.RestartPolicy)
	}

	var gracePeriod *int64
	if spec.StopTimeout != nil {
		t := int64(*spec.StopTimeout)
		gracePeriod = &t
	}

	labels := make(map[string]string, len(spec.Labels))
	for k, v := range spec.Labels {
		labels[k] = v
	}

	if err := p.err(); err != nil {
		return corev1.Pod{}, err
	}

	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec: corev1.PodSpec{
			Containers:                    []corev1.Container{container},
			Volumes:                       podVolumes,
			RestartPolicy:                 restartPolicy,
			TerminationGracePeriodSeconds: gracePeriod,
		},
	}, nil
}

// appRestartPolicyToKube maps podcd restart policy names to Kubernetes RestartPolicy values.
func appRestartPolicyToKube(s string) corev1.RestartPolicy {
	switch s {
	case "on-failure":
		return corev1.RestartPolicyOnFailure
	case "no":
		return corev1.RestartPolicyNever
	default:
		return corev1.RestartPolicyAlways
	}
}

// appHealthcheckToProbe converts a model healthcheck to a Kubernetes probe.
// The AppSpec healthcheck port is the host port; we look up the container port from ports.
func appHealthcheckToProbe(hc *model.Healthcheck, ports []model.Port) *corev1.Probe {
	containerPort := func(hostPort int) (int, bool) {
		for _, p := range ports {
			if p.Host == hostPort {
				return p.Container, true
			}
		}
		return 0, false
	}

	var probe corev1.Probe
	switch {
	case hc.HTTP != nil:
		cp, ok := containerPort(hc.HTTP.Port)
		if !ok {
			return nil
		}
		probe.HTTPGet = &corev1.HTTPGetAction{
			Path: cmp.Or(hc.HTTP.Path, "/"),
			Port: intstr.FromInt32(int32(cp)),
		}
		if strings.EqualFold(hc.HTTP.Scheme, "https") {
			probe.HTTPGet.Scheme = corev1.URISchemeHTTPS
		}
	case hc.TCP != nil:
		cp, ok := containerPort(hc.TCP.Port)
		if !ok {
			return nil
		}
		probe.TCPSocket = &corev1.TCPSocketAction{Port: intstr.FromInt32(int32(cp))}
	case hc.Exec != nil:
		probe.Exec = &corev1.ExecAction{Command: hc.Exec.Command}
	default:
		return nil
	}
	return &probe
}

func validPort(n int) bool { return n >= 1 && n <= 65535 }

func probeTimeout(h *model.Healthcheck) string {
	switch {
	case h.HTTP != nil:
		return h.HTTP.Timeout
	case h.TCP != nil:
		return h.TCP.Timeout
	case h.Exec != nil:
		return h.Exec.Timeout
	}
	return ""
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

func checkDuration(s string) error {
	if _, err := time.ParseDuration(s); err != nil {
		return fmt.Errorf("%q is not a duration (try 5s, 500ms, 1m)", s)
	}
	return nil
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

func validEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
