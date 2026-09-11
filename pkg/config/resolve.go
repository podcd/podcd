package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/podcd/podcd/pkg/model"
	"github.com/podcd/podcd/pkg/secrets"
)

// ResolveOptions controls one compilation of the index for one host.
type ResolveOptions struct {
	// Host is the identity of this host. It must match a Host document.
	Host string
	// Secrets resolves secretEnv references. May be nil if no app uses secrets.
	Secrets *secrets.Resolver
	// Revisions is repo name -> commit, carried into the desired state for reporting.
	Revisions map[string]string
}

// Resolve compiles the index into the desired state for one host.
//
// Precedence, lowest to highest: the Application document, the Environment's
// override for it, each Group's override (in the order the Host lists its
// groups), then the Host's own override. The output is sorted, so the same
// commit always compiles to the same bytes.
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

	// Layers, lowest precedence first.
	type layer struct {
		label     string
		overrides map[string]Override
	}
	var layers []layer
	selected := newOrderedSet()

	env, hasEnv := EnvironmentDoc{}, false
	if host.Spec.Environment != "" {
		env, hasEnv = ix.Environments[host.Spec.Environment]
		if !hasEnv {
			return zero, fmt.Errorf("%s: environment %q is not defined (known: %s)",
				host.Source, host.Spec.Environment, strings.Join(sortedKeys(ix.Environments), ", "))
		}
		selected.addAll(env.Spec.Applications)
		layers = append(layers, layer{"environment/" + env.Metadata.Name, env.Spec.Overrides})
	}

	for _, gname := range host.Spec.Groups {
		g, ok := ix.Groups[gname]
		if !ok {
			return zero, fmt.Errorf("%s: group %q is not defined (known: %s)",
				host.Source, gname, strings.Join(sortedKeys(ix.Groups), ", "))
		}
		selected.addAll(g.Spec.Applications)
		layers = append(layers, layer{"group/" + gname, g.Spec.Overrides})
	}

	selected.addAll(host.Spec.Applications)
	layers = append(layers, layer{"host/" + host.Metadata.Name, host.Spec.Overrides})

	for _, ex := range host.Spec.ExcludeApplications {
		if !selected.has(ex) {
			return zero, fmt.Errorf("%s: excludeApplications lists %q, which this host does not run anyway",
				host.Source, ex)
		}
		selected.remove(ex)
	}

	names := selected.sorted()
	apps := make([]model.Application, 0, len(names))
	var problems []error

	for _, name := range names {
		// A pod manifest and a podcd Application are compiled by different
		// code, but they land in the same canonical type and the same plan.
		if podDoc, ok := ix.Pods[name]; ok {
			pod := podDoc.Pod
			origins := []string{podDoc.Source.String()}
			var err error
			for _, l := range layers {
				o, ok := l.overrides[name]
				if !ok {
					continue
				}
				pod, err = patchPod(pod, o)
				if err != nil {
					problems = append(problems, fmt.Errorf("%s: override for pod %q: %w", l.label, name, err))
					break
				}
				origins = append(origins, l.label)
			}
			if err != nil {
				continue
			}
			app, err := ix.podToApplication(ctx, name, pod, opts.Secrets)
			if err != nil {
				problems = append(problems, err)
				continue
			}
			app.SourceRepo = podDoc.Source.Repo
			app.Origins = origins
			apps = append(apps, app)
			continue
		}

		base, ok := ix.Applications[name]
		if !ok {
			problems = append(problems, fmt.Errorf("application %q is referenced by host %q but never defined (as an Application or a Pod)", name, opts.Host))
			continue
		}
		spec := base.Spec
		origins := []string{base.Source.String()}
		var err error
		for _, l := range layers {
			o, ok := l.overrides[name]
			if !ok {
				continue
			}
			var over AppSpec
			if over, err = decodeAppOverride(o); err != nil {
				problems = append(problems, fmt.Errorf("%s: override for application %q: %w", l.label, name, err))
				break
			}
			spec = mergeAppSpec(spec, over)
			origins = append(origins, l.label)
		}
		if err != nil {
			continue
		}
		app, err := specToApplication(ctx, name, spec, opts.Secrets)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		app.SourceRepo = base.Source.Repo
		app.Origins = origins
		apps = append(apps, app)
	}

	// Overrides that name an application this host does not run are almost
	// always a typo or a stale entry. Say so rather than ignoring them.
	for _, l := range layers {
		for name := range l.overrides {
			if !selected.has(name) {
				problems = append(problems, fmt.Errorf("%s overrides application %q, which host %q does not run", l.label, name, opts.Host))
			}
		}
	}

	problems = append(problems, checkHostPortConflicts(apps)...)
	if len(problems) > 0 {
		sort.Slice(problems, func(i, j int) bool { return problems[i].Error() < problems[j].Error() })
		return zero, errors.Join(problems...)
	}

	groups := append([]string(nil), host.Spec.Groups...)
	return model.DesiredState{
		Host:         opts.Host,
		Environment:  host.Spec.Environment,
		Groups:       groups,
		Revisions:    opts.Revisions,
		Applications: apps,
	}, nil
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
	if over.SecretEnv != nil {
		out.SecretEnv = mergeMap(base.SecretEnv, over.SecretEnv)
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
	if over.AllowMutableImage != nil {
		out.AllowMutableImage = over.AllowMutableImage
	}
	return out
}

func mergeMap(base, over map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

// specToApplication validates a merged spec and converts it to the canonical form.
func specToApplication(ctx context.Context, name string, spec AppSpec, sec *secrets.Resolver) (model.Application, error) {
	var problems []error
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf("application %q: "+format, append([]any{name}, args...)...))
	}

	if !validName(name) {
		add("name must be lowercase letters, digits and dashes")
	}

	app := model.Application{
		Name:              name,
		Image:             spec.Image,
		Command:           append([]string(nil), spec.Command...),
		Entrypoint:        append([]string(nil), spec.Entrypoint...),
		Env:               mergeMap(nil, spec.Env),
		Labels:            mergeMap(nil, spec.Labels),
		RestartPolicy:     spec.RestartPolicy,
		User:              spec.User,
		WorkingDir:        spec.WorkingDir,
		AllowMutableImage: spec.AllowMutableImage != nil && *spec.AllowMutableImage,
	}
	if spec.StopTimeout != nil {
		app.StopTimeout = *spec.StopTimeout
	}
	if app.RestartPolicy == "" {
		app.RestartPolicy = "always"
	}
	switch app.RestartPolicy {
	case "always", "on-failure", "no":
	default:
		add("restartPolicy %q must be always, on-failure or no", app.RestartPolicy)
	}

	switch {
	case spec.Image == "":
		add("no image")
	case !strings.Contains(spec.Image, "@sha256:") && !app.AllowMutableImage:
		add("image %q is not pinned to a digest; use image@sha256:... or set allowMutableImage: true to accept a moving target", spec.Image)
	}

	for k := range app.Env {
		if !validEnvName(k) {
			add("environment variable name %q is not valid", k)
		}
	}

	for _, p := range spec.Ports {
		port := model.Port{Host: p.Host, Container: p.Container, Protocol: strings.ToLower(p.Protocol), HostIP: p.HostIP}
		if port.Protocol == "" {
			port.Protocol = "tcp"
		}
		if port.Protocol != "tcp" && port.Protocol != "udp" {
			add("port protocol %q must be tcp or udp", p.Protocol)
		}
		if port.Host < 1 || port.Host > 65535 {
			add("host port %d is out of range", port.Host)
		}
		if port.Container < 1 || port.Container > 65535 {
			add("container port %d is out of range", port.Container)
		}
		app.Ports = append(app.Ports, port)
	}
	sort.Slice(app.Ports, func(i, j int) bool { return portLess(app.Ports[i], app.Ports[j]) })

	for _, v := range spec.Volumes {
		if v.Source == "" || v.Destination == "" {
			add("volume needs both source and destination")
			continue
		}
		if !strings.HasPrefix(v.Destination, "/") {
			add("volume destination %q must be an absolute path", v.Destination)
		}
		app.Volumes = append(app.Volumes, model.Volume{Source: v.Source, Destination: v.Destination, Options: v.Options})
	}
	sort.Slice(app.Volumes, func(i, j int) bool {
		if app.Volumes[i].Destination != app.Volumes[j].Destination {
			return app.Volumes[i].Destination < app.Volumes[j].Destination
		}
		return app.Volumes[i].Source < app.Volumes[j].Source
	})

	app.Networks = append([]string(nil), spec.Networks...)
	sort.Strings(app.Networks)

	if spec.Resources != nil {
		app.Resources = model.Resources{Memory: spec.Resources.Memory, CPU: spec.Resources.CPU}
	}

	if hc := spec.Healthcheck; hc != nil {
		set := 0
		out := &model.Healthcheck{Retries: hc.Retries, Interval: hc.Interval}
		if hc.HTTP != nil {
			set++
			out.HTTP = &model.HTTPProbe{Port: hc.HTTP.Port, Path: hc.HTTP.Path, Host: hc.HTTP.Host,
				Scheme: hc.HTTP.Scheme, Expect: hc.HTTP.Expect, Timeout: hc.HTTP.Timeout}
			if out.HTTP.Port < 1 || out.HTTP.Port > 65535 {
				add("healthcheck http port %d is out of range", out.HTTP.Port)
			}
		}
		if hc.TCP != nil {
			set++
			out.TCP = &model.TCPProbe{Port: hc.TCP.Port, Host: hc.TCP.Host, Timeout: hc.TCP.Timeout}
			if out.TCP.Port < 1 || out.TCP.Port > 65535 {
				add("healthcheck tcp port %d is out of range", out.TCP.Port)
			}
		}
		if hc.Exec != nil {
			set++
			out.Exec = &model.ExecProbe{Command: append([]string(nil), hc.Exec.Command...), Timeout: hc.Exec.Timeout}
			if len(out.Exec.Command) == 0 {
				add("healthcheck exec needs a command")
			}
		}
		if set > 1 {
			add("healthcheck must set at most one of http, tcp, exec")
		}
		for _, d := range []string{hc.Interval, probeTimeout(out)} {
			if d == "" {
				continue
			}
			if err := checkDuration(d); err != nil {
				add("healthcheck: %v", err)
			}
		}
		if hc.Retries < 0 {
			add("healthcheck retries must not be negative")
		}
		app.Healthcheck = out
	}

	if len(spec.SecretEnv) > 0 {
		if sec == nil {
			add("uses secretEnv but no secret provider is configured")
		} else {
			app.SecretEnv = make(map[string]string, len(spec.SecretEnv))
			refs := make([]string, 0, len(spec.SecretEnv))
			for k := range spec.SecretEnv {
				refs = append(refs, k)
			}
			sort.Strings(refs)
			for _, k := range refs {
				if !validEnvName(k) {
					add("secret environment variable name %q is not valid", k)
					continue
				}
				v, err := sec.Resolve(ctx, spec.SecretEnv[k])
				if err != nil {
					add("%v", err)
					continue
				}
				app.SecretEnv[k] = v
			}
		}
	}

	if len(problems) > 0 {
		return model.Application{}, errors.Join(problems...)
	}
	return app, nil
}

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

func portLess(a, b model.Port) bool {
	if a.Host != b.Host {
		return a.Host < b.Host
	}
	if a.Container != b.Container {
		return a.Container < b.Container
	}
	if a.Protocol != b.Protocol {
		return a.Protocol < b.Protocol
	}
	return a.HostIP < b.HostIP
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

// orderedSet keeps insertion order for reporting but returns sorted output, so
// nothing downstream depends on the order documents happened to be written in.
type orderedSet struct {
	order []string
	set   map[string]bool
}

func newOrderedSet() *orderedSet { return &orderedSet{set: map[string]bool{}} }

func (o *orderedSet) addAll(names []string) {
	for _, n := range names {
		if !o.set[n] {
			o.set[n] = true
			o.order = append(o.order, n)
		}
	}
}

func (o *orderedSet) has(n string) bool { return o.set[n] }

func (o *orderedSet) remove(n string) { delete(o.set, n) }

func (o *orderedSet) sorted() []string {
	out := make([]string, 0, len(o.set))
	for _, n := range o.order {
		if o.set[n] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}
