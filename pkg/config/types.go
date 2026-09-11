// Package config parses the documents that live in Git and compiles them,
// deterministically, into a canonical desired state for one host.
//
// Two families of document are understood. podcd's own kinds
// (gitops.podcd.io/v1: Application, Group, Environment, Host) and a thin
// slice of Kubernetes core/v1 (Pod, ConfigMap, Secret), so a pod manifest
// people already have can be run here through `podman kube play`.
package config

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// APIVersion is podcd's own API group and version.
const APIVersion = "gitops.podcd.io/v1"

// CoreAPIVersion is the Kubernetes core group the agent accepts a slice of.
const CoreAPIVersion = "v1"

// Kinds understood by the loader.
const (
	KindApplication = "Application"
	KindGroup       = "Group"
	KindEnvironment = "Environment"
	KindHost        = "Host"

	KindPod       = "Pod"
	KindConfigMap = "ConfigMap"
	KindSecret    = "Secret"
)

// document is the envelope every file is first decoded into. It is the
// Kubernetes envelope, so both families share one header and one decoder.
type document struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              json.RawMessage `json:"spec,omitempty"`
}

// Source records where a document came from, for error messages a human can act on.
type Source struct {
	Repo string `json:"repo"`
	Path string `json:"path"` // path relative to the repository root
	Line int    `json:"line"`
}

func (s Source) String() string {
	if s.Repo == "" {
		return fmt.Sprintf("%s:%d", s.Path, s.Line)
	}
	return fmt.Sprintf("%s/%s:%d", s.Repo, s.Path, s.Line)
}

// PortSpec is a published port. It also accepts the shorthand "8080:80" or "8080".
type PortSpec struct {
	Host      int    `json:"host"`
	Container int    `json:"container"`
	Protocol  string `json:"protocol,omitempty"`
	HostIP    string `json:"hostIP,omitempty"`
}

// UnmarshalJSON accepts either an object or the "[hostIP:]host:container[/proto]" shorthand.
func (p *PortSpec) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		return p.parseShorthand(s)
	}
	type plain PortSpec
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*p = PortSpec(v)
	return nil
}

func (p *PortSpec) parseShorthand(s string) error {
	spec := s
	if i := strings.LastIndex(spec, "/"); i >= 0 {
		p.Protocol = spec[i+1:]
		spec = spec[:i]
	}
	parts := strings.Split(spec, ":")
	nums := parts
	if len(parts) == 3 {
		p.HostIP = parts[0]
		nums = parts[1:]
	} else if len(parts) > 3 || len(parts) == 0 {
		return fmt.Errorf("port %q: expected [hostIP:]host:container[/proto]", s)
	}
	vals := make([]int, 0, 2)
	for _, n := range nums {
		v, err := strconv.Atoi(strings.TrimSpace(n))
		if err != nil {
			return fmt.Errorf("port %q: %q is not a number", s, n)
		}
		vals = append(vals, v)
	}
	switch len(vals) {
	case 1:
		p.Host, p.Container = vals[0], vals[0]
	case 2:
		p.Host, p.Container = vals[0], vals[1]
	default:
		return fmt.Errorf("port %q: expected [hostIP:]host:container[/proto]", s)
	}
	return nil
}

// VolumeSpec is a mount. It also accepts the shorthand "source:destination[:options]".
type VolumeSpec struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Options     string `json:"options,omitempty"`
}

// UnmarshalJSON accepts either an object or the "source:destination[:options]" shorthand.
func (v *VolumeSpec) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		parts := strings.Split(s, ":")
		switch len(parts) {
		case 2:
			v.Source, v.Destination = parts[0], parts[1]
		case 3:
			v.Source, v.Destination, v.Options = parts[0], parts[1], parts[2]
		default:
			return fmt.Errorf("volume %q: expected source:destination[:options]", s)
		}
		return nil
	}
	type plain VolumeSpec
	var out plain
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	*v = VolumeSpec(out)
	return nil
}

// HTTPProbeSpec mirrors model.HTTPProbe.
type HTTPProbeSpec struct {
	Port    int    `json:"port"`
	Path    string `json:"path,omitempty"`
	Host    string `json:"host,omitempty"`
	Scheme  string `json:"scheme,omitempty"`
	Expect  int    `json:"expect,omitempty"`
	Timeout string `json:"timeout,omitempty"`
}

// TCPProbeSpec mirrors model.TCPProbe.
type TCPProbeSpec struct {
	Port    int    `json:"port"`
	Host    string `json:"host,omitempty"`
	Timeout string `json:"timeout,omitempty"`
}

// ExecProbeSpec mirrors model.ExecProbe.
type ExecProbeSpec struct {
	Command []string `json:"command"`
	Timeout string   `json:"timeout,omitempty"`
}

// HealthcheckSpec mirrors model.Healthcheck.
type HealthcheckSpec struct {
	HTTP     *HTTPProbeSpec `json:"http,omitempty"`
	TCP      *TCPProbeSpec  `json:"tcp,omitempty"`
	Exec     *ExecProbeSpec `json:"exec,omitempty"`
	Retries  int            `json:"retries,omitempty"`
	Interval string         `json:"interval,omitempty"`
}

// ResourcesSpec mirrors model.Resources.
type ResourcesSpec struct {
	Memory string `json:"memory,omitempty"`
	CPU    string `json:"cpu,omitempty"`
}

// AppSpec is an application definition, or a partial override of one.
//
// Merge rules, applied in the documented precedence order:
//   - scalars: a non-empty value in the higher layer wins
//   - maps (env, secretEnv, labels): merged key by key, higher layer wins per key
//   - slices (ports, volumes, networks, command): replaced wholesale, never appended
//   - healthcheck, resources: replaced wholesale when present
//
// Slices replace rather than merge because appending has no sane semantics for
// a port list and hides what is actually running.
type AppSpec struct {
	Image      string   `json:"image,omitempty"`
	Command    []string `json:"command,omitempty"`
	Entrypoint []string `json:"entrypoint,omitempty"`

	Env       map[string]string `json:"env,omitempty"`
	SecretEnv map[string]string `json:"secretEnv,omitempty"` // env var name -> secret reference

	Ports    []PortSpec        `json:"ports,omitempty"`
	Volumes  []VolumeSpec      `json:"volumes,omitempty"`
	Networks []string          `json:"networks,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`

	RestartPolicy string `json:"restartPolicy,omitempty"`
	User          string `json:"user,omitempty"`
	WorkingDir    string `json:"workingDir,omitempty"`
	StopTimeout   *int   `json:"stopTimeout,omitempty"`

	Healthcheck *HealthcheckSpec `json:"healthcheck,omitempty"`
	Resources   *ResourcesSpec   `json:"resources,omitempty"`

	AllowMutableImage *bool `json:"allowMutableImage,omitempty"`
}

// Override is a partial change to one workload, kept raw until resolution
// because how it is applied depends on what it targets: an Application takes
// an AppSpec merge, a Pod takes a strategic merge patch.
type Override json.RawMessage

// UnmarshalJSON keeps the raw bytes.
func (o *Override) UnmarshalJSON(data []byte) error {
	*o = append((*o)[:0], data...)
	return nil
}

// MarshalJSON returns the raw bytes.
func (o Override) MarshalJSON() ([]byte, error) {
	if len(o) == 0 {
		return []byte("null"), nil
	}
	return json.RawMessage(o), nil
}

// ApplicationDoc is a kind: Application document.
type ApplicationDoc struct {
	Metadata metav1.ObjectMeta
	Spec     AppSpec
	Source   Source
}

// GroupSpec is a kind: Group document body.
type GroupSpec struct {
	Applications []string            `json:"applications,omitempty"`
	Overrides    map[string]Override `json:"overrides,omitempty"`
}

// GroupDoc is a kind: Group document.
type GroupDoc struct {
	Metadata metav1.ObjectMeta
	Spec     GroupSpec
	Source   Source
}

// EnvironmentSpec is a kind: Environment document body.
type EnvironmentSpec struct {
	Applications []string            `json:"applications,omitempty"`
	Overrides    map[string]Override `json:"overrides,omitempty"`
}

// EnvironmentDoc is a kind: Environment document.
type EnvironmentDoc struct {
	Metadata metav1.ObjectMeta
	Spec     EnvironmentSpec
	Source   Source
}

// HostSpec is a kind: Host document body.
type HostSpec struct {
	Environment         string              `json:"environment,omitempty"`
	Groups              []string            `json:"groups,omitempty"`
	Applications        []string            `json:"applications,omitempty"`
	ExcludeApplications []string            `json:"excludeApplications,omitempty"`
	Overrides           map[string]Override `json:"overrides,omitempty"`
}

// HostDoc is a kind: Host document.
type HostDoc struct {
	Metadata metav1.ObjectMeta
	Spec     HostSpec
	Source   Source
}

// PodDoc is a Kubernetes core/v1 Pod, decoded strictly against the real type so
// a misspelled field fails here rather than being silently ignored by podman.
type PodDoc struct {
	Pod    corev1.Pod
	Source Source
}

// ConfigMapDoc is a Kubernetes core/v1 ConfigMap.
type ConfigMapDoc struct {
	ConfigMap corev1.ConfigMap
	Source    Source
}

// SecretDoc is a Kubernetes core/v1 Secret. Its values must be references
// (env:NAME, file:path), never plaintext; the loader enforces that.
type SecretDoc struct {
	Secret corev1.Secret
	Source Source
}
