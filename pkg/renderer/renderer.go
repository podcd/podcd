// Package renderer compiles a canonical Application into the Quadlet unit that systemd will run.
//
// Rendering is pure and deterministic, the same Application always produces the same bytes.
// That is what makes reconciliation idempotent: "has this changed?" is answered by comparing rendered bytes to the file on disk, not by asking Podman.
package renderer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/podcd/podcd/pkg/model"
)

// Prefix marks every unit podcd owns. Units without it are left strictly alone.
const Prefix = "podcd-"

// Marker comments written at the top of every generated unit.
// They are how the agent recognises its own work on the next run.
const (
	markerManaged = "# Managed by podcd - do not edit. Change Git instead."
	markerApp     = "# podcd-app: "
	markerSpec    = "# podcd-spec-hash: "
	markerSecrets = "# podcd-secrets-hash: "
	markerVersion = "# podcd-renderer: "
	// markerManifest records the hash of the played manifest.
	// The unit bytes alone say whether a kube workload changed.
	markerManifest = "# podcd-manifest-hash: "
	markerKind     = "# podcd-kind: "
)

// Version is bumped when the rendered output format changes.
// That way existing units get rewritten even when the application spec did not change.
const Version = "1"

// Renderer turns applications into unit files under UnitDir.
type Renderer struct {
	// UnitDir is where .container files are written (~/.config/containers/systemd).
	UnitDir string
	// EnvDir is where 0600 secret env files are written. It must not be in Git.
	EnvDir string
	// KubeDir is where played manifests for kube workloads are written.
	// They can contain resolved secrets, so it is not the unit directory.
	KubeDir string
}

// Unit is everything that must exist on disk for one application.
type Unit struct {
	App         string
	FileName    string // podcd-api.container
	Path        string
	ServiceName string // podcd-api.service
	Content     []byte

	SpecHash    string
	SecretsHash string

	EnvFilePath string
	EnvFile     []byte // nil when the application has no secrets

	// Kube workloads: the manifest podman plays, referenced by Yaml= in the unit.
	ManifestPath string
	Manifest     []byte
	ManifestHash string
}

// IsKube reports whether this unit plays a manifest rather than running a container.
func (u Unit) IsKube() bool { return u.ManifestPath != "" }

// ServiceName returns the systemd service name for an application.
func ServiceName(app string) string { return Prefix + app + ".service" }

// FileName returns the Quadlet file name for a container application.
func FileName(app string) string { return Prefix + app + ".container" }

// KubeFileName returns the Quadlet file name for a kube workload.
func KubeFileName(app string) string { return Prefix + app + ".kube" }

// FileNameFor returns the Quadlet file name for an application of either kind.
func FileNameFor(app model.Application) string {
	if app.IsKube() {
		return KubeFileName(app.Name)
	}
	return FileName(app.Name)
}

// ContainerName returns the container name podman will use.
func ContainerName(app string) string { return Prefix + app }

// AppFromFileName returns the application name for a managed unit file, and whether it is one of ours by name.
// Both .container and .kube files qualify, they map to the same service name.
// So one application can only ever be one of them.
func AppFromFileName(name string) (string, bool) {
	if !strings.HasPrefix(name, Prefix) {
		return "", false
	}
	for _, ext := range []string{".container", ".kube"} {
		if strings.HasSuffix(name, ext) {
			return strings.TrimSuffix(strings.TrimPrefix(name, Prefix), ext), true
		}
	}
	return "", false
}

// Render produces the unit (and secret env file) for one application.
func (r *Renderer) Render(app model.Application) (Unit, error) {
	if app.Name == "" {
		return Unit{}, fmt.Errorf("application has no name")
	}
	if app.IsKube() {
		return r.renderKube(app)
	}
	if app.Image == "" {
		return Unit{}, fmt.Errorf("application %q has no image", app.Name)
	}

	u := Unit{
		App:         app.Name,
		FileName:    FileName(app.Name),
		ServiceName: ServiceName(app.Name),
		SpecHash:    app.SpecHash(),
		SecretsHash: app.SecretsHash(),
	}
	u.Path = filepath.Join(r.UnitDir, u.FileName)

	if len(app.SecretEnv) > 0 {
		if r.EnvDir == "" {
			return Unit{}, fmt.Errorf("application %q uses secrets but no env directory is configured", app.Name)
		}
		u.EnvFilePath = filepath.Join(r.EnvDir, app.Name+".env")
		env, err := renderEnvFile(app)
		if err != nil {
			return Unit{}, err
		}
		u.EnvFile = env
	}

	content, err := r.renderUnit(app, u)
	if err != nil {
		return Unit{}, err
	}
	u.Content = content
	return u, nil
}

// renderKube produces a .kube unit and the manifest it plays.
// podman does the pod interpretation, the unit only says where the YAML is and how systemd should supervise it.
func (r *Renderer) renderKube(app model.Application) (Unit, error) {
	if len(app.Manifest) == 0 {
		return Unit{}, fmt.Errorf("pod %q has no manifest", app.Name)
	}
	if r.KubeDir == "" {
		return Unit{}, fmt.Errorf("pod %q: no kube directory is configured", app.Name)
	}
	u := Unit{
		App:          app.Name,
		FileName:     KubeFileName(app.Name),
		ServiceName:  ServiceName(app.Name),
		SpecHash:     app.SpecHash(),
		ManifestPath: filepath.Join(r.KubeDir, app.Name+".yaml"),
		Manifest:     app.Manifest,
		ManifestHash: app.ManifestHash,
	}
	u.Path = filepath.Join(r.UnitDir, u.FileName)

	var b bytes.Buffer
	b.WriteString(markerManaged + "\n")
	b.WriteString(markerApp + app.Name + "\n")
	b.WriteString(markerKind + model.KindKube + "\n")
	b.WriteString(markerSpec + u.SpecHash + "\n")
	b.WriteString(markerManifest + u.ManifestHash + "\n")
	b.WriteString(markerVersion + Version + "\n")
	b.WriteString("\n")

	b.WriteString("[Unit]\n")
	fmt.Fprintf(&b, "Description=podcd pod %s\n", app.Name)
	b.WriteString("\n")

	b.WriteString("[Kube]\n")
	fmt.Fprintf(&b, "Yaml=%s\n", u.ManifestPath)
	b.WriteString("\n")

	// Quadlet generates `podman kube play --replace` and `podman kube down`.
	// Restart= applies to the service container that stands for the pod.
	b.WriteString("[Service]\n")
	switch app.RestartPolicy {
	case "", "always":
		b.WriteString("Restart=always\n")
	default:
		fmt.Fprintf(&b, "Restart=%s\n", app.RestartPolicy)
	}
	b.WriteString("RestartSec=5\n")
	b.WriteString("\n")

	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=default.target\n")

	u.Content = b.Bytes()
	return u, nil
}

func (r *Renderer) renderUnit(app model.Application, u Unit) ([]byte, error) {
	var b bytes.Buffer

	b.WriteString(markerManaged + "\n")
	b.WriteString(markerApp + app.Name + "\n")
	b.WriteString(markerSpec + u.SpecHash + "\n")
	if u.SecretsHash != "" {
		b.WriteString(markerSecrets + u.SecretsHash + "\n")
	}
	b.WriteString(markerVersion + Version + "\n")
	b.WriteString("\n")

	b.WriteString("[Unit]\n")
	fmt.Fprintf(&b, "Description=podcd application %s\n", app.Name)
	b.WriteString("\n")

	b.WriteString("[Container]\n")
	fmt.Fprintf(&b, "ContainerName=%s\n", ContainerName(app.Name))
	fmt.Fprintf(&b, "Image=%s\n", app.Image)

	// Labels: podcd's own first, then the application's, both sorted.
	labels := map[string]string{
		"io.podcd.managed":   "true",
		"io.podcd.app":       app.Name,
		"io.podcd.spec-hash": u.SpecHash,
	}
	for k, v := range app.Labels {
		labels[k] = v
	}
	for _, k := range sortedKeys(labels) {
		if err := checkUnitValue("label "+k, labels[k]); err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "Label=%s\n", quoteIfNeeded(k+"="+labels[k]))
	}

	for _, k := range sortedKeys(app.Env) {
		if err := checkUnitValue("environment variable "+k, app.Env[k]); err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "Environment=%s\n", quoteIfNeeded(k+"="+app.Env[k]))
	}
	if u.EnvFilePath != "" {
		fmt.Fprintf(&b, "EnvironmentFile=%s\n", u.EnvFilePath)
	}

	for _, p := range app.Ports {
		fmt.Fprintf(&b, "PublishPort=%s\n", publishPort(p))
	}
	for _, v := range app.Volumes {
		spec := v.Source + ":" + v.Destination
		if v.Options != "" {
			spec += ":" + v.Options
		}
		fmt.Fprintf(&b, "Volume=%s\n", spec)
	}
	for _, n := range app.Networks {
		fmt.Fprintf(&b, "Network=%s\n", n)
	}
	if app.User != "" {
		user, group, found := strings.Cut(app.User, ":")
		fmt.Fprintf(&b, "User=%s\n", user)
		if found && group != "" {
			fmt.Fprintf(&b, "Group=%s\n", group)
		}
	}
	if len(app.Command) > 0 {
		fmt.Fprintf(&b, "Exec=%s\n", commandLine(app.Command))
	}
	// Entrypoint and WorkingDir have no Quadlet key in podman 4.x, so they go through PodmanArgs.
	// One line, fixed order, so the output stays stable.
	if extra := podmanArgs(app); extra != "" {
		fmt.Fprintf(&b, "PodmanArgs=%s\n", extra)
	}
	b.WriteString("\n")

	b.WriteString("[Service]\n")
	switch app.RestartPolicy {
	case "", "always":
		b.WriteString("Restart=always\n")
	default:
		fmt.Fprintf(&b, "Restart=%s\n", app.RestartPolicy)
	}
	b.WriteString("RestartSec=5\n")
	if app.StopTimeout > 0 {
		fmt.Fprintf(&b, "TimeoutStopSec=%d\n", app.StopTimeout)
	}
	// Resource limits belong to systemd, not the container runtime.
	// The unit is what systemd supervises, and cgroup limits survive a restart.
	if app.Resources.Memory != "" {
		fmt.Fprintf(&b, "MemoryMax=%s\n", app.Resources.Memory)
	}
	if app.Resources.CPU != "" {
		fmt.Fprintf(&b, "CPUQuota=%s\n", app.Resources.CPU)
	}
	b.WriteString("\n")

	// WantedBy makes the application come back after a reboot.
	// With lingering enabled for the agent user, that is all "survives reboot" needs.
	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=default.target\n")

	return b.Bytes(), nil
}

// renderEnvFile writes resolved secrets in systemd EnvironmentFile syntax.
func renderEnvFile(app model.Application) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("# Managed by podcd - do not edit. Generated from secret references.\n")
	for _, k := range sortedKeys(app.SecretEnv) {
		v := app.SecretEnv[k]
		if strings.ContainsAny(v, "\n\r") {
			return nil, fmt.Errorf("application %q: secret %s contains a newline, which systemd cannot carry in an environment file", app.Name, k)
		}
		fmt.Fprintf(&b, "%s=%s\n", k, unitQuote(v))
	}
	return b.Bytes(), nil
}

// Markers describes what a unit file on disk says about itself.
type Markers struct {
	Managed      bool
	App          string
	Kind         string // "" or "container" for .container units, "kube" for .kube units
	SpecHash     string
	SecretsHash  string
	ManifestHash string
	Version      string
}

// ParseMarkers reads podcd's marker comments from a unit file.
func ParseMarkers(content []byte) Markers {
	var m Markers
	for _, line := range strings.Split(string(content), "\n") {
		switch {
		case strings.HasPrefix(line, markerManaged):
			m.Managed = true
		case strings.HasPrefix(line, markerApp):
			m.App = strings.TrimSpace(strings.TrimPrefix(line, markerApp))
		case strings.HasPrefix(line, markerSpec):
			m.SpecHash = strings.TrimSpace(strings.TrimPrefix(line, markerSpec))
		case strings.HasPrefix(line, markerSecrets):
			m.SecretsHash = strings.TrimSpace(strings.TrimPrefix(line, markerSecrets))
		case strings.HasPrefix(line, markerVersion):
			m.Version = strings.TrimSpace(strings.TrimPrefix(line, markerVersion))
		case strings.HasPrefix(line, markerManifest):
			m.ManifestHash = strings.TrimSpace(strings.TrimPrefix(line, markerManifest))
		case strings.HasPrefix(line, markerKind):
			m.Kind = strings.TrimSpace(strings.TrimPrefix(line, markerKind))
		case strings.HasPrefix(line, "["):
			return m // markers are only ever in the header
		}
	}
	return m
}

func publishPort(p model.Port) string {
	var sb strings.Builder
	if p.HostIP != "" {
		sb.WriteString(p.HostIP)
		sb.WriteString(":")
	}
	sb.WriteString(strconv.Itoa(p.Host))
	sb.WriteString(":")
	sb.WriteString(strconv.Itoa(p.Container))
	if p.Protocol != "" && p.Protocol != "tcp" {
		sb.WriteString("/")
		sb.WriteString(p.Protocol)
	}
	return sb.String()
}

// podmanArgs renders the settings Quadlet has no key for.
//
// Podman's Quadlet only gained Entrypoint= and WorkingDir= keys in 5.x.
// Passing them as raw podman flags works on every version that has Quadlet at all.
// That keeps one code path instead of two that diverge by podman release.
func podmanArgs(app model.Application) string {
	var args []string
	if len(app.Entrypoint) > 0 {
		// podman wants a JSON array for a multi-word entrypoint.
		// The whole flag is quoted as one unit value so the inner quotes and spaces survive systemd's parser.
		encoded, err := json.Marshal(app.Entrypoint)
		if err == nil {
			args = append(args, unitQuote("--entrypoint="+string(encoded)))
		}
	}
	if app.WorkingDir != "" {
		args = append(args, quoteIfNeeded("--workdir="+app.WorkingDir))
	}
	return strings.Join(args, " ")
}

// commandLine renders argv the way systemd parses it.
func commandLine(args []string) string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		out = append(out, quoteIfNeeded(a))
	}
	return strings.Join(out, " ")
}

// quoteIfNeeded quotes a unit value only when it has to be quoted.
// Simple values stay readable to whoever is debugging on the host at 3am.
func quoteIfNeeded(s string) string {
	if s == "" {
		return `""`
	}
	if !strings.ContainsAny(s, " \t\"'\\$%") {
		return s
	}
	return unitQuote(s)
}

// unitQuote wraps a value in double quotes the way systemd reads them.
//
// strconv.Quote does not work here: it escapes non-ASCII into \u sequences systemd cannot decode.
// That would corrupt any value with an accent in it.
// systemd only needs \\ and \" escaped inside double quotes.
func unitQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

// checkUnitValue rejects values that cannot survive a unit file.
func checkUnitValue(what, v string) error {
	if strings.ContainsAny(v, "\n\r") {
		return fmt.Errorf("%s contains a newline, which a systemd unit cannot represent", what)
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
