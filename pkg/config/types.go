// Package config parses the documents in Git and compiles them into the desired
// state for one host: podcd's kinds (gitops.podcd.io/v1: Host, Group,
// Environment, Network), core/v1 (Pod, ConfigMap, Secret), and ExternalSecret/SecretStore.
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
	KindNetwork     = "Network"

	KindPod       = "Pod"
	KindConfigMap = "ConfigMap"
	KindSecret    = "Secret"
)

// document is the Kubernetes envelope every document is first decoded into.
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

// Override is a raw strategic merge patch for one Pod (or merge for a Network).
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

// Doc is one loaded document: name, source, and body (the spec for podcd's
// kinds, the whole strictly-decoded object for core/v1).
type Doc[T any] struct {
	Name   string
	Spec   T
	Source Source
}

// SelectionSpec is a Group or Environment body: applications, overrides and network overrides.
type SelectionSpec struct {
	Applications     []string            `json:"applications,omitempty"`
	Overrides        map[string]Override `json:"overrides,omitempty"`
	NetworkOverrides map[string]Override `json:"networkOverrides,omitempty"`
	Values           []string            `json:"values,omitempty"`
}

// NetworkSpec is a Network body, following `podman network create`; empty is a bridge network.
type NetworkSpec struct {
	Driver     string            `json:"driver,omitempty"`
	Subnet     string            `json:"subnet,omitempty"`
	Gateway    string            `json:"gateway,omitempty"`
	IPRange    string            `json:"ipRange,omitempty"`
	Internal   bool              `json:"internal,omitempty"`
	IPv6       bool              `json:"ipv6,omitempty"`
	DisableDNS bool              `json:"disableDNS,omitempty"`
	DNS        []string          `json:"dns,omitempty"`
	Options    map[string]string `json:"options,omitempty"`
}

// HostSpec is a kind: Host document body.
type HostSpec struct {
	Environment         string              `json:"environment,omitempty"`
	Groups              []string            `json:"groups,omitempty"`
	Applications        []string            `json:"applications,omitempty"`
	ExcludeApplications []string            `json:"excludeApplications,omitempty"`
	Overrides           map[string]Override `json:"overrides,omitempty"`
	NetworkOverrides    map[string]Override `json:"networkOverrides,omitempty"`
	Values              []string            `json:"values,omitempty"`
}
