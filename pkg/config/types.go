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
)

// APIVersion is podcd's own API group and version.
const APIVersion = "gitops.podcd.io/v1"

// CoreAPIVersion is the Kubernetes core group the agent accepts a slice of.
const CoreAPIVersion = "v1"

// Kinds understood by the loader.
const (
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

// Override is a partial change to one workload, kept raw until resolution.
// It is applied to the Pod it targets as a strategic merge patch.
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
