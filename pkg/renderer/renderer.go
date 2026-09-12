// Package renderer compiles a canonical Application into the Quadlet unit that systemd will run.
//
// Rendering is pure and deterministic, the same Application always produces the same bytes.
// That is what makes reconciliation idempotent: "has this changed?" is answered by comparing rendered bytes to the file on disk, not by asking Podman.
package renderer

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
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

// UnitPath is where the unit for an application of either kind lives.
func (r *Renderer) UnitPath(app model.Application) string {
	return filepath.Join(r.UnitDir, FileNameFor(app))
}

// EnvFilePath is where an application's resolved secrets go, or "" when no
// env directory is configured.
func (r *Renderer) EnvFilePath(app string) string {
	if r.EnvDir == "" {
		return ""
	}
	return filepath.Join(r.EnvDir, app+".env")
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

// Render produces the unit (and secret env file) for one application.
func (r *Renderer) Render(app model.Application) (Unit, error) {
	if app.Name == "" {
		return Unit{}, fmt.Errorf("application has no name")
	}
	u := Unit{
		App:         app.Name,
		FileName:    FileNameFor(app),
		Path:        r.UnitPath(app),
		ServiceName: ServiceName(app.Name),
		SpecHash:    app.SpecHash(),
	}
	if app.IsKube() {
		return r.renderKube(app, u)
	}
	if app.Image == "" {
		return Unit{}, fmt.Errorf("application %q has no image", app.Name)
	}
	u.SecretsHash = app.SecretsHash()
	if len(app.SecretEnv) > 0 {
		if u.EnvFilePath = r.EnvFilePath(app.Name); u.EnvFilePath == "" {
			return Unit{}, fmt.Errorf("application %q uses secrets but no env directory is configured", app.Name)
		}
		env, err := renderEnvFile(app)
		if err != nil {
			return Unit{}, err
		}
		u.EnvFile = env
	}
	content, err := r.renderContainer(app, u)
	if err != nil {
		return Unit{}, err
	}
	u.Content = content
	return u, nil
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
	fmt.Fprintf(&b, "[Kube]\nYaml=%s\n\n", u.ManifestPath)
	// Quadlet generates `podman kube play --replace` and `podman kube down`.
	// Restart= applies to the service container that stands for the pod.
	writeService(&b, app)
	writeInstall(&b)
	u.Content = b.Bytes()
	return u, nil
}

func (r *Renderer) renderContainer(app model.Application, u Unit) ([]byte, error) {
	var b bytes.Buffer
	writeHeader(&b, u, "")
	fmt.Fprintf(&b, "[Unit]\nDescription=podcd application %s\n\n", app.Name)

	b.WriteString("[Container]\n")
	fmt.Fprintf(&b, "ContainerName=%s\n", ContainerName(app.Name))
	fmt.Fprintf(&b, "Image=%s\n", app.Image)

	// Labels: podcd's own first, then the application's, both sorted.
	labels := map[string]string{
		"io.podcd.managed":   "true",
		"io.podcd.app":       app.Name,
		"io.podcd.spec-hash": u.SpecHash,
	}
	maps.Copy(labels, app.Labels)
	if err := writeKeyValues(&b, "Label", "label ", labels); err != nil {
		return nil, err
	}
	if err := writeKeyValues(&b, "Environment", "environment variable ", app.Env); err != nil {
		return nil, err
	}
	if u.EnvFilePath != "" {
		fmt.Fprintf(&b, "EnvironmentFile=%s\n", u.EnvFilePath)
	}

	for _, p := range app.Ports {
		fmt.Fprintf(&b, "PublishPort=%s\n", p)
	}
	for _, v := range app.Volumes {
		fmt.Fprintf(&b, "Volume=%s\n", v)
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

	writeService(&b, app)
	writeInstall(&b)
	return b.Bytes(), nil
}

// writeHeader writes the marker comments the agent recognises its own work by.
func writeHeader(b *bytes.Buffer, u Unit, kind string) {
	b.WriteString(markerManaged + "\n")
	b.WriteString(markerApp + u.App + "\n")
	if kind != "" {
		b.WriteString(markerKind + kind + "\n")
	}
	b.WriteString(markerSpec + u.SpecHash + "\n")
	if u.SecretsHash != "" {
		b.WriteString(markerSecrets + u.SecretsHash + "\n")
	}
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

// writeKeyValues writes one key=value line per map entry, sorted.
func writeKeyValues(b *bytes.Buffer, unitKey, what string, m map[string]string) error {
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if err := checkUnitValue(what+k, m[k]); err != nil {
			return err
		}
		fmt.Fprintf(b, "%s=%s\n", unitKey, quoteIfNeeded(k+"="+m[k]))
	}
	return nil
}

// renderEnvFile writes resolved secrets in systemd EnvironmentFile syntax.
func renderEnvFile(app model.Application) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("# Managed by podcd - do not edit. Generated from secret references.\n")
	for _, k := range slices.Sorted(maps.Keys(app.SecretEnv)) {
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
