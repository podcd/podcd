// Package model holds the desired, actual and plan types. Runtime output and
// user YAML are both translated into these before anything is compared.
package model

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Port is a published port mapping; JSON accepts an object or "[hostIP:]host:container[/proto]".
type Port struct {
	Host      int    `json:"host"`
	Container int    `json:"container"`
	Protocol  string `json:"protocol,omitempty"` // tcp (default) or udp
	HostIP    string `json:"hostIP,omitempty"`   // empty means all interfaces
}

// UnmarshalJSON accepts either an object or the shorthand.
func (p *Port) UnmarshalJSON(data []byte) error {
	s, ok, err := shorthand(data)
	if err != nil || !ok {
		type plain Port
		return json.Unmarshal(data, (*plain)(p))
	}
	spec := s
	if i := strings.LastIndex(spec, "/"); i >= 0 {
		p.Protocol = spec[i+1:]
		spec = spec[:i]
	}
	parts := strings.Split(spec, ":")
	if len(parts) == 3 {
		p.HostIP = parts[0]
		parts = parts[1:]
	}
	if len(parts) > 2 {
		return fmt.Errorf("port %q: expected [hostIP:]host:container[/proto]", s)
	}
	nums := make([]int, 0, 2)
	for _, n := range parts {
		v, err := strconv.Atoi(strings.TrimSpace(n))
		if err != nil {
			return fmt.Errorf("port %q: %q is not a number", s, n)
		}
		nums = append(nums, v)
	}
	p.Host, p.Container = nums[0], nums[len(nums)-1]
	return nil
}

// String renders the port the way podman publishes it: [hostIP:]host:container[/proto].
func (p Port) String() string {
	var sb strings.Builder
	if p.HostIP != "" {
		sb.WriteString(p.HostIP + ":")
	}
	sb.WriteString(strconv.Itoa(p.Host) + ":" + strconv.Itoa(p.Container))
	if p.Protocol != "" && p.Protocol != "tcp" {
		sb.WriteString("/" + p.Protocol)
	}
	return sb.String()
}

// Volume is a bind mount or named volume; JSON accepts an object or "source:destination[:options]".
type Volume struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Options     string `json:"options,omitempty"` // e.g. ro,Z
}

// UnmarshalJSON accepts either an object or the shorthand.
func (v *Volume) UnmarshalJSON(data []byte) error {
	s, ok, err := shorthand(data)
	if err != nil || !ok {
		type plain Volume
		return json.Unmarshal(data, (*plain)(v))
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

// String renders the mount the way podman takes it: source:destination[:options].
func (v Volume) String() string {
	if v.Options != "" {
		return v.Source + ":" + v.Destination + ":" + v.Options
	}
	return v.Source + ":" + v.Destination
}

// shorthand reports whether a JSON value is a string, and returns it.
func shorthand(data []byte) (string, bool, error) {
	if len(data) == 0 || data[0] != '"' {
		return "", false, nil
	}
	var s string
	err := json.Unmarshal(data, &s)
	return s, true, err
}

// Resources are the optional systemd resource limits for the unit.
type Resources struct {
	Memory string `json:"memory,omitempty"` // e.g. 512M -> MemoryMax
	CPU    string `json:"cpu,omitempty"`    // e.g. 150% -> CPUQuota
}

// Application is one workload fully resolved for one host: no inheritance or templating left.
type Application struct {
	Name string `json:"name"`

	// Manifest is the played YAML (Pod, ConfigMaps, resolved Secrets). It may hold
	// secrets, so it never reaches JSON; ManifestHash stands in for it.
	Manifest     []byte `json:"-"`
	ManifestHash string `json:"manifestHash,omitempty"`
	// Images lists every image, for reporting; Image is the first.
	Images []string `json:"images,omitempty"`
	// InitContainers names the pod's init containers; runtime-only, never persisted.
	InitContainers []string `json:"-"`

	Image      string   `json:"image"`
	Command    []string `json:"command,omitempty"`
	Entrypoint []string `json:"entrypoint,omitempty"`

	Env map[string]string `json:"env,omitempty"`

	Ports   []Port   `json:"ports,omitempty"`
	Volumes []Volume `json:"volumes,omitempty"`
	// Networks the pod joins. Those with a Network document are also in
	// ManagedNetworks and referenced through their Quadlet unit; others must already exist.
	Networks        []string          `json:"networks,omitempty"`
	ManagedNetworks []string          `json:"managedNetworks,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	// UserNS is podman's --userns value; "" for the default.
	UserNS string `json:"userns,omitempty"`

	RestartPolicy string `json:"restartPolicy,omitempty"` // always (default), on-failure, no
	User          string `json:"user,omitempty"`          // user[:group] inside the container
	WorkingDir    string `json:"workingDir,omitempty"`
	StopTimeout   int    `json:"stopTimeout,omitempty"`

	Resources Resources `json:"resources,omitempty"`

	// Provenance, for humans debugging on the host.
	SourceRepo string   `json:"sourceRepo,omitempty"`
	Origins    []string `json:"origins,omitempty"`
}

// ImageList returns every image the workload runs, one per container.
func (a Application) ImageList() []string {
	if len(a.Images) > 0 {
		return a.Images
	}
	if a.Image == "" {
		return nil
	}
	return []string{a.Image}
}

// SetManifest stores a kube manifest and its hash together, so the two can never disagree.
func (a *Application) SetManifest(manifest []byte) {
	a.Manifest = manifest
	a.ManifestHash = HashBytes(manifest)
}

// Network is a Network document resolved for one host; it exists there only
// while a desired application joins it.
type Network struct {
	Name string `json:"name"`

	Driver     string            `json:"driver,omitempty"` // bridge (default), macvlan, ipvlan
	Subnet     string            `json:"subnet,omitempty"`
	Gateway    string            `json:"gateway,omitempty"`
	IPRange    string            `json:"ipRange,omitempty"`
	Internal   bool              `json:"internal,omitempty"`
	IPv6       bool              `json:"ipv6,omitempty"`
	DisableDNS bool              `json:"disableDNS,omitempty"`
	DNS        []string          `json:"dns,omitempty"`
	Options    map[string]string `json:"options,omitempty"` // driver options, --opt k=v

	// Provenance, as on Application.
	SourceRepo string   `json:"sourceRepo,omitempty"`
	Origins    []string `json:"origins,omitempty"`
}

// DesiredState is everything that should exist on this host at a given Git revision.
type DesiredState struct {
	Host        string            `json:"host"`
	Environment string            `json:"environment"`
	Groups      []string          `json:"groups,omitempty"`
	Revisions   map[string]string `json:"revisions,omitempty"` // repo name -> commit sha

	Applications []Application `json:"applications"`
	Networks     []Network     `json:"networks,omitempty"`
}

// App returns the application with the given name.
func (d DesiredState) App(name string) (Application, bool) {
	for _, a := range d.Applications {
		if a.Name == name {
			return a, true
		}
	}
	return Application{}, false
}

// Names returns the application names, sorted.
func (d DesiredState) Names() []string {
	names := make([]string, 0, len(d.Applications))
	for _, a := range d.Applications {
		names = append(names, a.Name)
	}
	slices.Sort(names)
	return names
}

// RevisionString renders the pinned revisions as a stable single-line string.
func (d DesiredState) RevisionString() string {
	parts := make([]string, 0, len(d.Revisions))
	for _, n := range slices.Sorted(maps.Keys(d.Revisions)) {
		parts = append(parts, n+"="+ShortRev(d.Revisions[n]))
	}
	return strings.Join(parts, " ")
}

// ShortRev abbreviates a commit sha for display.
func ShortRev(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// UnitState is what systemd reports about a unit.
type UnitState string

const (
	UnitActive     UnitState = "active"
	UnitActivating UnitState = "activating"
	UnitInactive   UnitState = "inactive"
	UnitFailed     UnitState = "failed"
	UnitMissing    UnitState = "missing"
	UnitUnknown    UnitState = "unknown"
)

// ActualApp is what really exists on the host for one application.
type ActualApp struct {
	Name string `json:"name"`

	// Managed is false for units podcd did not write. podcd never touches those.
	Managed bool `json:"managed"`

	UnitFile     string `json:"unitFile,omitempty"`
	UnitFileHash string `json:"unitFileHash,omitempty"` // sha256 of the on-disk unit
	// UnitContent and ManifestContent are read from disk to explain changes; never stored.
	UnitContent []byte `json:"-"`
	ManifestContent []byte `json:"-"`
	SpecHash        string `json:"specHash,omitempty"` // marker written by the renderer

	UnitName  string    `json:"unitName,omitempty"`
	UnitState UnitState `json:"unitState,omitempty"`
	SubState  string    `json:"subState,omitempty"`

	ContainerID    string `json:"containerId,omitempty"`
	ContainerImage string `json:"containerImage,omitempty"`
	ContainerState string `json:"containerState,omitempty"`

	// Containers excludes infra; lets the planner see one that died inside an active unit.
	Containers []ContainerStatus `json:"containers,omitempty"`
}

// ContainerStatus is what the runtime reports about one container.
type ContainerStatus struct {
	Name  string `json:"name"`
	State string `json:"state"` // running, exited, created, paused
	// Health is the healthcheck verdict (healthy, unhealthy, starting), empty without one.
	Health string `json:"health,omitempty"`
	// Restarts > 0 while "starting" means crash-looping.
	Restarts int `json:"restarts,omitempty"`
}

// IsInitContainer matches the bare name or a "-<name>" suffix (kube play prefixes the pod name).
func IsInitContainer(initNames []string, containerName string) bool {
	for _, n := range initNames {
		if containerName == n || strings.HasSuffix(containerName, "-"+n) {
			return true
		}
	}
	return false
}

// ActualNetwork is one network's unit, its systemd state, and whether the runtime has it.
type ActualNetwork struct {
	Name string `json:"name"`

	// Managed is false for .network units podcd did not write. Never touched.
	Managed bool `json:"managed"`

	UnitFile     string `json:"unitFile,omitempty"`
	UnitFileHash string `json:"unitFileHash,omitempty"`
	UnitContent  []byte `json:"-"`
	SpecHash     string `json:"specHash,omitempty"`

	UnitName  string    `json:"unitName,omitempty"`
	UnitState UnitState `json:"unitState,omitempty"`

	// Exists reports whether podman has a network of this name right now.
	Exists bool `json:"exists"`
}

// ActualState is the observed state of the whole host.
type ActualState struct {
	Runtime  string                   `json:"runtime"`
	Apps     map[string]ActualApp     `json:"apps"`
	Networks map[string]ActualNetwork `json:"networks,omitempty"`
}

// Names returns the observed application names, sorted.
func (s ActualState) Names() []string { return slices.Sorted(maps.Keys(s.Apps)) }

// NetworkNames returns the observed network names, sorted.
func (s ActualState) NetworkNames() []string { return slices.Sorted(maps.Keys(s.Networks)) }

// ActionType is the kind of change the planner decided on.
type ActionType string

const (
	ActionCreate  ActionType = "create"
	ActionUpdate  ActionType = "update"
	ActionDelete  ActionType = "delete"
	ActionRestart ActionType = "restart"
	ActionNoOp    ActionType = "noop"
)

// ActionKind says what an action is about; empty means an application.
type ActionKind string

const (
	KindApplication ActionKind = ""
	KindNetwork     ActionKind = "network"
)

// Action is one change to one application or network.
type Action struct {
	Type ActionType `json:"type"`
	Kind ActionKind `json:"kind,omitempty"`
	// App names the application, or the network when Kind is set (name kept for JSON compatibility).
	App     string   `json:"app"`
	Reason  string   `json:"reason"`
	Details []string `json:"details,omitempty"`

	// Destructive marks actions that remove something a human might miss.
	Destructive bool `json:"destructive,omitempty"`

	// Application is the desired spec; empty for deletes and for networks.
	Application *Application `json:"-"`
	// Network is the desired spec; empty for deletes and for applications.
	Network *Network `json:"-"`
}

// Subject names what the action is about the way a human would say it.
func (a Action) Subject() string {
	if a.Kind == KindNetwork {
		return "network " + a.App
	}
	return a.App
}

// Plan is the ordered set of actions to make reality match Git.
type Plan struct {
	Actions []Action `json:"actions"`
}

// Empty reports whether the plan would change anything. No-ops do not count.
func (p Plan) Empty() bool {
	for _, a := range p.Actions {
		if a.Type != ActionNoOp {
			return false
		}
	}
	return true
}

// Changes returns only the actions that change something.
func (p Plan) Changes() []Action {
	out := make([]Action, 0, len(p.Actions))
	for _, a := range p.Actions {
		if a.Type != ActionNoOp {
			out = append(out, a)
		}
	}
	return out
}

// Destructive reports whether the plan removes anything.
func (p Plan) Destructive() bool {
	for _, a := range p.Actions {
		if a.Destructive {
			return true
		}
	}
	return false
}

// HealthStatus is the outcome of a probe.
type HealthStatus string

const (
	HealthHealthy   HealthStatus = "healthy"
	HealthUnhealthy HealthStatus = "unhealthy"
	HealthUnknown   HealthStatus = "unknown"
)

// Health is one application's health at a point in time.
type Health struct {
	App       string       `json:"app"`
	Status    HealthStatus `json:"status"`
	Message   string       `json:"message,omitempty"`
	CheckedAt time.Time    `json:"checkedAt"`
}

// OK reports whether the application is healthy.
func (h Health) OK() bool { return h.Status == HealthHealthy }
