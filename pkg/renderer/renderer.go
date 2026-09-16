// Package renderer compiles a canonical Application into the Quadlet unit that systemd will run.
//
// Rendering is pure and deterministic, the same Application always produces the same bytes.
// That is what makes reconciliation idempotent: "has this changed?" is answered by comparing rendered bytes to the file on disk, not by asking Podman.
package renderer

import (
	"bytes"
	"cmp"
	"fmt"
	"path/filepath"
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
	// UnitDir is where .kube unit files are written (~/.config/containers/systemd).
	UnitDir string
	// KubeDir is where played manifests for kube workloads are written.
	KubeDir string
}

// Unit is everything that must exist on disk for one application.
type Unit struct {
	App         string
	FileName    string // podcd-api.kube
	Path        string
	ServiceName string // podcd-api.service
	Content     []byte

	SpecHash string

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

// UnitPath is where the unit for an application of either kind lives.
func (r *Renderer) UnitPath(app model.Application) string {
	return filepath.Join(r.UnitDir, FileNameFor(app))
}

// ManifestPath is where a kube workload's played manifest goes, or "" when
// no kube directory is configured.
func (r *Renderer) ManifestPath(app string) string {
	if r.KubeDir == "" {
		return ""
	}
	return filepath.Join(r.KubeDir, app+".yaml")
}

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

// Render produces the .kube unit and manifest for one application.
func (r *Renderer) Render(app model.Application) (Unit, error) {
	if app.Name == "" {
		return Unit{}, fmt.Errorf("application has no name")
	}
	u := Unit{
		App:         app.Name,
		FileName:    KubeFileName(app.Name),
		Path:        filepath.Join(r.UnitDir, KubeFileName(app.Name)),
		ServiceName: ServiceName(app.Name),
		SpecHash:    app.SpecHash(),
	}
	return r.renderKube(app, u)
}

// renderKube produces a .kube unit and the manifest it plays.
// podman does the pod interpretation, the unit only says where the YAML is and how systemd should supervise it.
func (r *Renderer) renderKube(app model.Application, u Unit) (Unit, error) {
	if len(app.Manifest) == 0 {
		return Unit{}, fmt.Errorf("pod %q has no manifest", app.Name)
	}
	if u.ManifestPath = r.ManifestPath(app.Name); u.ManifestPath == "" {
		return Unit{}, fmt.Errorf("pod %q: no kube directory is configured", app.Name)
	}
	u.Manifest = app.Manifest
	u.ManifestHash = app.ManifestHash

	var b bytes.Buffer
	writeHeader(&b, u, model.KindKube)
	fmt.Fprintf(&b, "[Unit]\nDescription=podcd pod %s\n\n", app.Name)
	fmt.Fprintf(&b, "[Kube]\nYaml=%s\n", u.ManifestPath)
	for _, n := range app.Networks {
		fmt.Fprintf(&b, "Network=%s\n", n)
	}
	b.WriteString("\n")
	// Quadlet generates `podman kube play --replace` and `podman kube down`.
	// Restart= applies to the service container that stands for the pod.
	writeService(&b, app)
	writeInstall(&b)
	u.Content = b.Bytes()
	return u, nil
}

// writeHeader writes the marker comments the agent recognises its own work by.
func writeHeader(b *bytes.Buffer, u Unit, kind string) {
	b.WriteString(markerManaged + "\n")
	b.WriteString(markerApp + u.App + "\n")
	if kind != "" {
		b.WriteString(markerKind + kind + "\n")
	}
	b.WriteString(markerSpec + u.SpecHash + "\n")
	if u.ManifestHash != "" {
		b.WriteString(markerManifest + u.ManifestHash + "\n")
	}
	b.WriteString(markerVersion + Version + "\n\n")
}

// writeService writes the [Service] section: how systemd supervises the unit.
// Resource limits belong to systemd, not the container runtime: the unit is
// what systemd supervises, and cgroup limits survive a restart.
func writeService(b *bytes.Buffer, app model.Application) {
	b.WriteString("[Service]\n")
	fmt.Fprintf(b, "Restart=%s\n", cmp.Or(app.RestartPolicy, "always"))
	b.WriteString("RestartSec=5\n")
	if app.StopTimeout > 0 {
		fmt.Fprintf(b, "TimeoutStopSec=%d\n", app.StopTimeout)
	}
	if app.Resources.Memory != "" {
		fmt.Fprintf(b, "MemoryMax=%s\n", app.Resources.Memory)
	}
	if app.Resources.CPU != "" {
		fmt.Fprintf(b, "CPUQuota=%s\n", app.Resources.CPU)
	}
	b.WriteString("\n")
}

// writeInstall makes the unit come back after a reboot. With lingering
// enabled for the agent user, that is all "survives reboot" needs.
func writeInstall(b *bytes.Buffer) {
	b.WriteString("[Install]\nWantedBy=default.target\n")
}

// Markers describes what a unit file on disk says about itself.
type Markers struct {
	Managed      bool
	App          string
	Kind         string
	SpecHash     string
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
