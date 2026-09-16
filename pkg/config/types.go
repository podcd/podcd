// Package config parses the documents that live in Git and compiles them, deterministically, into a canonical desired state for one host.
//
// Two families of document are understood.
// podcd's own kinds (gitops.podcd.io/v1: Application, Group, Environment, Host).
// A thin slice of Kubernetes core/v1 (Pod, ConfigMap, Secret), so a pod manifest people already have can be run here through `podman kube play`.
package config

import (
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/podcd/podcd/pkg/model"
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

// document is the envelope every file is first decoded into.
// It is the Kubernetes envelope, so both families share one header and one decoder.
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
	// Raw is the document's YAML as loaded
	Raw []byte `json:"-"`
}

func (s Source) String() string {
	if s.Repo == "" {
		return fmt.Sprintf("%s:%d", s.Path, s.Line)
	}
	return fmt.Sprintf("%s/%s:%d", s.Repo, s.Path, s.Line)
}

// AppSpec is an application definition, or a partial override of one.
//
// Merge rules, applied in the documented precedence order:
//   - scalars: a non-empty value in the higher layer wins
//   - maps (env, labels): merged key by key, higher layer wins per key
//   - slices (ports, volumes, networks, command, envFrom): replaced wholesale, never appended
//   - healthcheck, resources: replaced wholesale when present
//
// Slices replace rather than merge, appending has no sane semantics for a port list and hides what is actually running.
type AppSpec struct {
	Image      string   `json:"image,omitempty"`
	Command    []string `json:"command,omitempty"`
	Entrypoint []string `json:"entrypoint,omitempty"`

	Env     map[string]string `json:"env,omitempty"`
	EnvFrom []AppEnvFrom      `json:"envFrom,omitempty"`

	Ports    []model.Port      `json:"ports,omitempty"`
	Volumes  []model.Volume    `json:"volumes,omitempty"`
	Networks []string          `json:"networks,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`

	RestartPolicy string `json:"restartPolicy,omitempty"`
	User          string `json:"user,omitempty"`
	WorkingDir    string `json:"workingDir,omitempty"`
	StopTimeout   *int   `json:"stopTimeout,omitempty"`

	Healthcheck *model.Healthcheck `json:"healthcheck,omitempty"`
	Resources   *model.Resources   `json:"resources,omitempty"`
}

// AppEnvFrom injects all keys from a ConfigMap or Secret as environment variables.
// Mirrors corev1.EnvFromSource for the Application sugar layer.
type AppEnvFrom struct {
	ConfigMapRef *AppLocalObjectRef `json:"configMapRef,omitempty"`
	SecretRef    *AppLocalObjectRef `json:"secretRef,omitempty"`
	// Prefix prepends a string to every key from the source.
	Prefix string `json:"prefix,omitempty"`
}

// AppLocalObjectRef names a ConfigMap or Secret by name.
type AppLocalObjectRef struct {
	Name string `json:"name"`
}

// Override is a partial change to one workload, kept raw until resolution.
// How it is applied depends on what it targets: an Application takes an AppSpec merge, a Pod takes a strategic merge patch.
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

// Doc is one loaded document: its name, where it came from, and its body.
// For podcd's kinds the body is the spec.
// For core/v1 kinds it is the whole object, decoded strictly against the real Kubernetes type.
// A misspelled field fails here rather than being silently ignored by podman.
type Doc[T any] struct {
	Name   string
	Spec   T
	Source Source
}

// SelectionSpec is the body of a Group or an Environment: the applications
// every member runs, and overrides for any application a member runs.
type SelectionSpec struct {
	Applications []string            `json:"applications,omitempty"`
	Overrides    map[string]Override `json:"overrides,omitempty"`
	Values       []string            `json:"values,omitempty"`
}

// HostSpec is a kind: Host document body.
type HostSpec struct {
	Environment         string              `json:"environment,omitempty"`
	Groups              []string            `json:"groups,omitempty"`
	Applications        []string            `json:"applications,omitempty"`
	ExcludeApplications []string            `json:"excludeApplications,omitempty"`
	Overrides           map[string]Override `json:"overrides,omitempty"`
	Values              []string            `json:"values,omitempty"`
}
