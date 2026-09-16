// Package model holds the canonical desired/actual/plan types.
// Everything the agent reasons about lives here.
// Raw Podman output and raw user YAML both get translated into these types before anything is compared.
// The planner never diffs a string from `podman inspect` against a string a human typed into Git.
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

// Port is a published port mapping.
// In YAML it is an object or the shorthand "[hostIP:]host:container[/proto]".
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

// Volume is a bind mount or named volume.
// In YAML it is an object or the shorthand "source:destination[:options]".
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

// HTTPProbe checks an HTTP endpoint published by the container.
type HTTPProbe struct {
	Port    int    `json:"port"`
	Path    string `json:"path,omitempty"`
	Host    string `json:"host,omitempty"`   // default 127.0.0.1
	Scheme  string `json:"scheme,omitempty"` // http (default) or https
	Expect  int    `json:"expect,omitempty"` // expected status, default 200
	Timeout string `json:"timeout,omitempty"`
}

// TCPProbe checks that a TCP port accepts a connection.
type TCPProbe struct {
	Port    int    `json:"port"`
	Host    string `json:"host,omitempty"`
	Timeout string `json:"timeout,omitempty"`
}

// ExecProbe runs a command inside the container.
type ExecProbe struct {
	Command []string `json:"command"`
	Timeout string   `json:"timeout,omitempty"`
}

// Healthcheck describes how to tell whether an application is working.
// Exactly one probe may be set; none means "systemd says the unit is active".
type Healthcheck struct {
	HTTP *HTTPProbe `json:"http,omitempty"`
	TCP  *TCPProbe  `json:"tcp,omitempty"`
	Exec *ExecProbe `json:"exec,omitempty"`

	// Retries and interval to use while waiting for an app to become healthy after it is applied.
	Retries  int    `json:"retries,omitempty"`
	Interval string `json:"interval,omitempty"`
}

// Resources are the optional systemd resource limits for the unit.
type Resources struct {
	Memory string `json:"memory,omitempty"` // e.g. 512M -> MemoryMax
	CPU    string `json:"cpu,omitempty"`    // e.g. 150% -> CPUQuota
}

// Workload kinds.
// A container is one Quadlet .container unit.
// A kube workload is a pod manifest played by podman through a Quadlet .kube unit.
const (
	KindContainer = "container"
	KindKube      = "kube"
)

// Application is a fully resolved application.
// It is the result of compiling environment + group + host + application definitions for one host.
// Nothing here is inherited or templated any more, it is what should exist on this host.
type Application struct {
	Name string `json:"name"`

	// Kind is KindContainer (the default when empty) or KindKube.
	Kind string `json:"kind,omitempty"`

	// Manifest is the multi-document YAML played by podman for a kube workload.
	// It holds the Pod, its ConfigMaps, and its Secrets with values resolved.
	// It may contain secrets, so it never appears in JSON output.
	// ManifestHash stands in for it everywhere, including in the spec hash.
	Manifest     []byte `json:"-"`
	ManifestHash string `json:"manifestHash,omitempty"`
	// Images lists every image a kube workload runs, for reporting.
	// Image holds the first one, so the common code paths have something to show.
	Images []string `json:"images,omitempty"`

	Image      string   `json:"image"`
	Command    []string `json:"command,omitempty"`
	Entrypoint []string `json:"entrypoint,omitempty"`

	Env map[string]string `json:"env,omitempty"`

	Ports    []Port            `json:"ports,omitempty"`
	Volumes  []Volume          `json:"volumes,omitempty"`
	Networks []string          `json:"networks,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`

	RestartPolicy string `json:"restartPolicy,omitempty"` // always (default), on-failure, no
	User          string `json:"user,omitempty"`          // user[:group] inside the container
	WorkingDir    string `json:"workingDir,omitempty"`
	StopTimeout   int    `json:"stopTimeout,omitempty"`

	Healthcheck *Healthcheck `json:"healthcheck,omitempty"`
	Resources   Resources    `json:"resources,omitempty"`

	// Provenance, for humans debugging on the host.
	SourceRepo string   `json:"sourceRepo,omitempty"`
	Origins    []string `json:"origins,omitempty"`
}

// ImageList returns every image the workload runs.
// One for a container, one per container for a pod.
func (a Application) ImageList() []string {
	if len(a.Images) > 0 {
		return a.Images
	}
	if a.Image == "" {
		return nil
	}
	return []string{a.Image}
}

// IsKube reports whether the workload is a pod manifest rather than a container.
func (a Application) IsKube() bool { return a.Kind == KindKube }

// SetManifest stores a kube manifest and its hash together, so the two can never disagree.
func (a *Application) SetManifest(manifest []byte) {
	a.Manifest = manifest
	a.ManifestHash = HashBytes(manifest)
}


// DesiredState is everything that should exist on this host at a given Git revision.
type DesiredState struct {
	Host        string            `json:"host"`
	Environment string            `json:"environment"`
	Groups      []string          `json:"groups,omitempty"`
	Revisions   map[string]string `json:"revisions,omitempty"` // repo name -> commit sha

	Applications []Application `json:"applications"`
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
	UnitActive   UnitState = "active"
	UnitInactive UnitState = "inactive"
	UnitFailed   UnitState = "failed"
	UnitMissing  UnitState = "missing"
	UnitUnknown  UnitState = "unknown"
)

// ActualApp is what really exists on the host for one application.
type ActualApp struct {
	Name string `json:"name"`

	// Managed is false for units podcd did not write. podcd never touches those.
	Managed bool `json:"managed"`

	UnitFile     string `json:"unitFile,omitempty"`
	UnitFileHash string `json:"unitFileHash,omitempty"` // sha256 of the on-disk unit
	// UnitContent is the raw unit as read from disk.
	// It stays out of the state file, but lets the planner explain a change without re-reading disk.
	UnitContent []byte `json:"-"`
	// ManifestContent is the played manifest of a kube workload as read from disk.
	// Same rules as UnitContent: used for explaining changes, never stored.
	ManifestContent []byte `json:"-"`
	SpecHash string `json:"specHash,omitempty"` // marker written by the renderer

	UnitName  string    `json:"unitName,omitempty"`
	UnitState UnitState `json:"unitState,omitempty"`
	SubState  string    `json:"subState,omitempty"`

	ContainerID    string `json:"containerId,omitempty"`
	ContainerImage string `json:"containerImage,omitempty"`
	ContainerState string `json:"containerState,omitempty"`
}

// ActualState is the observed state of the whole host.
type ActualState struct {
	Runtime string               `json:"runtime"`
	Apps    map[string]ActualApp `json:"apps"`
}

// Names returns the observed application names, sorted.
func (s ActualState) Names() []string { return slices.Sorted(maps.Keys(s.Apps)) }

// ActionType is the kind of change the planner decided on.
type ActionType string

const (
	ActionCreate  ActionType = "create"
	ActionUpdate  ActionType = "update"
	ActionDelete  ActionType = "delete"
	ActionRestart ActionType = "restart"
	ActionNoOp    ActionType = "noop"
)

// Action is one change to one application.
type Action struct {
	Type    ActionType `json:"type"`
	App     string     `json:"app"`
	Reason  string     `json:"reason"`
	Details []string   `json:"details,omitempty"`

	// Destructive marks actions that remove something a human might miss.
	Destructive bool `json:"destructive,omitempty"`

	// Application is the desired spec; empty for deletes.
	Application *Application `json:"-"`
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
	Probe     string       `json:"probe"`
	Message   string       `json:"message,omitempty"`
	CheckedAt time.Time    `json:"checkedAt"`
}

// OK reports whether the application is healthy.
func (h Health) OK() bool { return h.Status == HealthHealthy }
