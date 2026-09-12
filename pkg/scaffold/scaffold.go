// Package scaffold writes the documents a Git repository needs to get started
package scaffold

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sigyaml "sigs.k8s.io/yaml"

	"github.com/podcd/podcd/pkg/config"
	"github.com/podcd/podcd/pkg/model"
)

// ApplicationOptions describes an Application to generate.
type ApplicationOptions struct {
	Name  string
	Image string
	// Ports are "host:container" pairs, published on 127.0.0.1.
	Ports []string
	// Env are "KEY=value" pairs.
	Env []string
	// HealthPath, when set, adds an HTTP health check on the first port.
	HealthPath string
}

// Application renders an Application document.
func Application(o ApplicationOptions) ([]byte, error) {
	if err := checkImage(o.Image); err != nil {
		return nil, err
	}
	spec := config.AppSpec{Image: o.Image, RestartPolicy: "always"}
	for _, p := range o.Ports {
		host, container, err := parsePort(p)
		if err != nil {
			return nil, err
		}
		spec.Ports = append(spec.Ports, model.Port{Host: host, Container: container, HostIP: "127.0.0.1"})
	}
	if len(o.Env) > 0 {
		spec.Env = map[string]string{}
		for _, e := range o.Env {
			k, v, ok := strings.Cut(e, "=")
			if !ok || k == "" {
				return nil, fmt.Errorf("--env %q: expected KEY=value", e)
			}
			spec.Env[k] = v
		}
	}
	if o.HealthPath != "" {
		if len(spec.Ports) == 0 {
			return nil, errors.New("--health-path needs a --port to probe")
		}
		spec.Healthcheck = &model.Healthcheck{HTTP: &model.HTTPProbe{Port: spec.Ports[0].Host, Path: o.HealthPath}}
	}
	return render(config.APIVersion, config.KindApplication, o.Name, spec)
}

// PodOptions describes a single-container Pod to generate.
type PodOptions struct {
	Name  string
	Image string
	Ports []string
}

// Pod renders a core/v1 Pod with one container, the shape people already have.
func Pod(o PodOptions) ([]byte, error) {
	if err := checkImage(o.Image); err != nil {
		return nil, err
	}
	container := corev1.Container{Name: o.Name, Image: o.Image}
	for _, p := range o.Ports {
		host, cport, err := parsePort(p)
		if err != nil {
			return nil, err
		}
		container.Ports = append(container.Ports, corev1.ContainerPort{ContainerPort: int32(cport), HostPort: int32(host), HostIP: "127.0.0.1"})
	}
	pod := corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: config.CoreAPIVersion, Kind: config.KindPod},
		ObjectMeta: metav1.ObjectMeta{Name: o.Name},
		Spec:       corev1.PodSpec{RestartPolicy: corev1.RestartPolicyAlways, Containers: []corev1.Container{container}},
	}
	return toYAML(pod)
}

func checkImage(image string) error {
	if image == "" {
		return errors.New("--image is required")
	}
	return nil
}

// HostOptions describes a Host to generate.
type HostOptions struct {
	Name         string
	Environment  string
	Groups       []string
	Applications []string
}

// Host renders a Host document.
func Host(o HostOptions) ([]byte, error) {
	return render(config.APIVersion, config.KindHost, o.Name, config.HostSpec{
		Environment: o.Environment, Groups: o.Groups, Applications: o.Applications,
	})
}

// Group renders a Group document selecting the given applications.
func Group(name string, applications []string) ([]byte, error) {
	return render(config.APIVersion, config.KindGroup, name, config.SelectionSpec{Applications: applications})
}

// Environment renders an Environment document selecting the given applications.
func Environment(name string, applications []string) ([]byte, error) {
	return render(config.APIVersion, config.KindEnvironment, name, config.SelectionSpec{Applications: applications})
}

// document is the envelope every podcd kind is written in.
// metav1.ObjectMeta is not used here because it marshals fields nobody should see in Git.
type document struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec any `json:"spec"`
}

func render(apiVersion, kind, name string, spec any) ([]byte, error) {
	if name == "" {
		return nil, fmt.Errorf("a %s needs a name", strings.ToLower(kind))
	}
	doc := document{APIVersion: apiVersion, Kind: kind, Spec: spec}
	doc.Metadata.Name = name
	return toYAML(doc)
}

// toYAML renders a value with its struct fields in declared order (image
// before ports before healthcheck, not alphabetical), lists indented, and the
// noise the k8s types add (a null creationTimestamp, an empty status) removed.
// encoding/json keeps field order; yaml.v3 keeps node order.
func toYAML(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var node yaml.Node
	if err := yaml.Unmarshal(raw, &node); err != nil {
		return nil, err
	}
	prune(&node)
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&node); err != nil {
		return nil, err
	}
	return check(b.Bytes())
}

// prune turns the parsed JSON into the block-style YAML a person expects and
// drops what the k8s types marshal but nobody wants in Git: null timestamps
// and empty sub-objects such as status or a container's resources.
// A top-level empty spec is kept, since a Host or Group may legitimately be nothing but a name.
func prune(n *yaml.Node) {
	n.Style = 0 // block style, not the JSON flow style the parser recorded
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			prune(c)
		}
	case yaml.MappingNode:
		kept := n.Content[:0]
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			k.Style = 0
			prune(v)
			empty := (v.Kind == yaml.MappingNode || v.Kind == yaml.SequenceNode) && len(v.Content) == 0
			if v.Tag == "!!null" || (empty && k.Value != "spec") {
				continue
			}
			kept = append(kept, k, v)
		}
		n.Content = kept
	}
}

// check round-trips a document through the loader, so a scaffolded file is
// one the agent accepts.
func check(doc []byte) ([]byte, error) {
	if err := config.NewIndex().LoadBytes("", "document.yaml", doc); err != nil {
		return nil, fmt.Errorf("generated document is not valid: %w", err)
	}
	return doc, nil
}

func parsePort(s string) (host, container int, err error) {
	var p model.Port
	if err := sigyaml.Unmarshal([]byte(fmt.Sprintf("%q", s)), &p); err != nil {
		return 0, 0, fmt.Errorf("--port %q: expected host:container", s)
	}
	if p.Host < 1 || p.Container < 1 {
		return 0, 0, fmt.Errorf("--port %q: expected host:container", s)
	}
	return p.Host, p.Container, nil
}

const nginx = "docker.io/library/nginx@sha256:72ba65eb42c10344912a84ff42408db7d34f2feb642204570ab8fc5ffd29f1d3"

// Init writes the smallest useful repository into dir
// Existing files are not overwritten unless force is set. It returns the files it wrote.
func Init(dir, host string, force bool) ([]string, error) {
	app, err := Application(ApplicationOptions{Name: "nginx", Image: nginx, Ports: []string{"8080:80"}, HealthPath: "/"})
	if err != nil {
		return nil, err
	}
	hostDoc, err := Host(HostOptions{Name: host, Applications: []string{"nginx"}})
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{
		"apps.yaml":  append([]byte("# What can run. An Application says what it is, not where it runs.\n"), app...),
		"hosts.yaml": append([]byte("# Which host runs what. One Host document per machine, named as the agent\n# identifies itself (its hostname, or `host:` in agent.yaml).\n"), hostDoc...),
		"README.md":  []byte(readme(host)),
	}

	if !force {
		var clash []string
		for name := range files {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				clash = append(clash, name)
			}
		}
		if len(clash) > 0 {
			sort.Strings(clash)
			return nil, fmt.Errorf("%s already has %s; use --force to overwrite", dir, strings.Join(clash, ", "))
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var written []string
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return nil, err
		}
		written = append(written, name)
	}
	sort.Strings(written)

	// The result must pass the same lint a user would run on it.
	if _, findings, err := config.LintPaths(context.Background(), nil, dir); err != nil {
		return nil, err
	} else if len(findings) > 0 {
		return nil, fmt.Errorf("scaffolded repository does not lint: %s", findings[0].Err)
	}
	return written, nil
}

func readme(host string) string {
	return fmt.Sprintf(`# podcd repository

This repository is the desired state for hosts managed by podcd. There is no
prescribed layout: any .yaml file anywhere is read, and documents are found by
kind and name.

- `+"`apps.yaml`"+`  - what can run: Application (and Pod) documents
- `+"`hosts.yaml`"+` - which host runs what: one Host per machine

Point a host to this repository:

    podcd config create --host %s --repo-url <local path or url to this repository>

Manual reconciliation:
    podcd validate
    podcd plan
    podcd reconcile

To grow it, add documents with podcd or by hand:

    podcd create application api --image ghcr.io/you/api:1.2.3 --port 8081:8080 >> apps.yaml
    podcd create group web --app nginx --app api >> groups.yaml
    podcd create host web-02 --group web >> hosts.yaml
    podcd lint

Secrets are references (env:NAME, file:path, vault:mount/path/key)
`, host)
}
