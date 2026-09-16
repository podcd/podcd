// Package podman runs applications as rootless Podman containers managed by systemd through Quadlet.
//
// The agent writes .container files and asks systemd to start them.
// It never runs `podman run`: systemd owns the process, Podman owns the container, and the agent owns neither.
// Podman is only asked questions.
package podman

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/podcd/podcd/internal/atomicfile"
	"github.com/podcd/podcd/internal/subprocess"
	"github.com/podcd/podcd/pkg/config"
	"github.com/podcd/podcd/pkg/model"
	"github.com/podcd/podcd/pkg/renderer"
)

// How long WaitHealthy keeps asking after a change is applied. A pod that has
// just been played needs a moment before its containers report for duty.
const (
	waitRetries  = 15
	waitInterval = 2 * time.Second
)

// Options configures a Runtime.
type Options struct {
	UnitDir string
	// KubeDir holds the manifests played by .kube units.
	// It can contain resolved secrets and is never the unit directory.
	KubeDir string

	// PodmanBin and SystemctlBin default to the names on PATH.
	PodmanBin    string
	SystemctlBin string

	// Timeout bounds any single podman or systemctl invocation.
	Timeout time.Duration
}

// Runtime is the rootless Podman + Quadlet + systemd --user implementation.
type Runtime struct {
	unitDir string

	podman     string
	systemctl  string
	journalctl string
	timeout    time.Duration

	rend *renderer.Renderer
}

// New returns a Podman runtime writing units into opts.UnitDir.
func New(opts Options) *Runtime {
	r := &Runtime{
		unitDir:    opts.UnitDir,
		podman:     cmp.Or(opts.PodmanBin, "podman"),
		systemctl:  cmp.Or(opts.SystemctlBin, "systemctl"),
		journalctl: "journalctl",
		timeout:    cmp.Or(opts.Timeout, 2*time.Minute),
	}
	r.rend = &renderer.Renderer{UnitDir: opts.UnitDir, KubeDir: opts.KubeDir}
	return r
}

// Renderer exposes the renderer this runtime writes with.
// So the planner compares against the bytes the runtime would produce.
func (r *Runtime) Renderer() *renderer.Renderer { return r.rend }

// Name implements runtime.Runtime.
func (r *Runtime) Name() string { return "podman" }

// Available reports whether this host can run rootless Podman under systemd.
func (r *Runtime) Available(ctx context.Context) (bool, string) {
	if _, err := exec.LookPath(r.podman); err != nil {
		return false, fmt.Sprintf("podman is not installed (%v)", err)
	}
	if _, err := exec.LookPath(r.systemctl); err != nil {
		return false, fmt.Sprintf("systemctl is not installed (%v)", err)
	}
	if _, err := r.systemctlRun(ctx, "is-system-running"); err != nil {
		// is-system-running exits non-zero for "degraded", that is fine.
		// Only a total absence of a user manager is fatal.
		if !strings.Contains(err.Error(), "degraded") && !strings.Contains(err.Error(), "starting") {
			return false, "no systemd user manager: " + err.Error()
		}
	}
	if !quadletGeneratorPresent() {
		return false, "the Quadlet generator was not found; podman 4.4+ with quadlet is required"
	}
	return true, ""
}

// Inspect reads the real state of the host: the unit files on disk, what systemd thinks of them, and what Podman actually has running.
func (r *Runtime) Inspect(ctx context.Context) (model.ActualState, error) {
	state := model.ActualState{Runtime: r.Name(), Apps: map[string]model.ActualApp{}}

	entries, err := os.ReadDir(r.unitDir)
	if err != nil && !os.IsNotExist(err) {
		return state, fmt.Errorf("reading unit directory %s: %w", r.unitDir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name, ok := renderer.AppFromFileName(e.Name())
		if !ok {
			continue // not ours, by name
		}
		path := filepath.Join(r.unitDir, e.Name())
		content, err := os.ReadFile(path)
		if err != nil {
			return state, fmt.Errorf("reading %s: %w", path, err)
		}
		m := renderer.ParseMarkers(content)
		if m.App != "" {
			name = m.App
		}
		if prev, ok := state.Apps[name]; ok {
			// Two files claiming one podcd-<name>.service: the header inside one
			// of them names an app that another file is already named after.
			// Refuse to guess which one systemd picked.
			return state, fmt.Errorf("application %q has two unit files: %s and %s; remove one", name, prev.UnitFile, path)
		}
		cur := model.ActualApp{
			Name:         name,
			Managed:      m.Managed,
			UnitFile:     path,
			UnitFileHash: model.HashBytes(content),
			UnitContent:  content,
			SpecHash:     m.SpecHash,
			UnitName:     renderer.ServiceName(name),
			UnitState:    model.UnitUnknown,
		}
		// A unit is only as current as the manifest it points at.
		// If that file was edited or deleted, the unit must be re-applied even though its own bytes still match.
		if m.Managed {
			if manifestPath := yamlPathOf(content); manifestPath != "" {
				data, err := os.ReadFile(manifestPath)
				if err != nil || model.HashBytes(data) != m.ManifestHash {
					cur.UnitFileHash = "manifest-drift:" + cur.UnitFileHash
				}
				cur.ManifestContent = data
			}
		}
		state.Apps[name] = cur
	}

	// Containers we own but have no unit for: a half-removed application, or a unit file someone deleted by hand.
	// They are still ours to clean up.
	containers, err := r.listContainers(ctx, "")
	if err != nil {
		return state, err
	}
	for app, all := range containers {
		cur, ok := state.Apps[app]
		if !ok {
			cur = model.ActualApp{Name: app, Managed: true, UnitName: renderer.ServiceName(app), UnitState: model.UnitMissing}
		}
		// One container stands for the workload in the reporting fields.
		if c := workload(all); len(c) > 0 {
			cur.ContainerID = c[0].id
			cur.ContainerImage = c[0].image
			cur.ContainerState = c[0].state
		}
		state.Apps[app] = cur
	}

	if err := r.fillUnitStates(ctx, state); err != nil {
		return state, err
	}
	return state, nil
}

type containerInfo struct {
	id    string
	name  string
	image string
	// state is podman's own word for it: running, exited, created, paused.
	state string
	infra bool
}

// listContainers asks Podman for everything labelled as ours, grouped by app.
//
// A pod contributes several containers under one app name, infra included, so
// the caller gets all of them rather than an arbitrary winner. Passing an app
// name narrows the query to that one workload.
func (r *Runtime) listContainers(ctx context.Context, app string) (map[string][]containerInfo, error) {
	args := []string{"ps", "--all", "--filter", "label=" + config.LabelManaged + "=true"}
	if app != "" {
		args = append(args, "--filter", "label="+config.LabelApp+"="+app)
	}
	out, err := r.podmanRun(ctx, append(args, "--format", "json")...)
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}
	var raw []struct {
		ID      string            `json:"Id"`
		Names   []string          `json:"Names"`
		Image   string            `json:"Image"`
		State   string            `json:"State"`
		IsInfra bool              `json:"IsInfra"`
		Labels  map[string]string `json:"Labels"`
	}
	trimmed := strings.TrimSpace(out)
	if trimmed == "" || trimmed == "null" {
		return map[string][]containerInfo{}, nil
	}
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return nil, fmt.Errorf("parsing podman ps output: %w", err)
	}
	result := make(map[string][]containerInfo, len(raw))
	for _, c := range raw {
		app := c.Labels[config.LabelApp]
		if app == "" {
			continue
		}
		id := c.ID
		if len(id) > 12 {
			id = id[:12]
		}
		var name string
		if len(c.Names) > 0 {
			name = c.Names[0]
		}
		result[app] = append(result[app], containerInfo{
			id: id, name: name, image: c.Image, state: c.State, infra: c.IsInfra,
		})
	}
	for app := range result {
		slices.SortFunc(result[app], func(a, b containerInfo) int { return strings.Compare(a.name, b.name) })
	}
	return result, nil
}

// workload drops the infra container, which is podman's own plumbing and says
// nothing about whether the application is running.
func workload(containers []containerInfo) []containerInfo {
	var out []containerInfo
	for _, c := range containers {
		if !c.infra {
			out = append(out, c)
		}
	}
	return out
}

// fillUnitStates asks systemd about every unit in one call.
func (r *Runtime) fillUnitStates(ctx context.Context, state model.ActualState) error {
	names := state.Names()
	if len(names) == 0 {
		return nil
	}
	args := []string{"show", "--property=Id", "--property=ActiveState", "--property=SubState", "--property=LoadState"}
	for _, n := range names {
		args = append(args, renderer.ServiceName(n))
	}
	out, err := r.systemctlRun(ctx, args...)
	if err != nil {
		return fmt.Errorf("querying systemd: %w", err)
	}
	byUnit := parseShowBlocks(out)
	for _, n := range names {
		app := state.Apps[n]
		props, ok := byUnit[renderer.ServiceName(n)]
		if !ok {
			app.UnitState = model.UnitMissing
			state.Apps[n] = app
			continue
		}
		app.SubState = props["SubState"]
		switch {
		case props["LoadState"] == "not-found":
			app.UnitState = model.UnitMissing
		case props["ActiveState"] == "active":
			app.UnitState = model.UnitActive
		case props["ActiveState"] == "failed":
			app.UnitState = model.UnitFailed
		case props["ActiveState"] == "inactive":
			app.UnitState = model.UnitInactive
		case props["ActiveState"] == "":
			app.UnitState = model.UnitUnknown
		default:
			// activating, deactivating, reloading: not yet running.
			app.UnitState = model.UnitState(props["ActiveState"])
		}
		state.Apps[n] = app
	}
	return nil
}

// parseShowBlocks splits `systemctl show` output into one property map per unit.
func parseShowBlocks(out string) map[string]map[string]string {
	result := map[string]map[string]string{}
	current := map[string]string{}
	flush := func() {
		if id := current["Id"]; id != "" {
			result[id] = current
		}
		current = map[string]string{}
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		current[k] = v
	}
	flush()
	return result
}

// Apply writes the unit for one application and makes systemd run it.
//
// It is safe to call on an application that is already correct.
// Writing the same bytes and restarting is the worst it can do, and the planner makes sure it is not called in that case.
func (r *Runtime) Apply(ctx context.Context, app model.Application) error {
	unit, err := r.rend.Render(app)
	if err != nil {
		return err
	}

	// Manifest may contain resolved secrets: readable by the agent user only.
	if err := atomicfile.Write(unit.ManifestPath, unit.Manifest, 0o600); err != nil {
		return fmt.Errorf("writing manifest for %s: %w", app.Name, err)
	}

	if err := atomicfile.Write(unit.Path, unit.Content, 0o644); err != nil {
		return fmt.Errorf("writing unit for %s: %w", app.Name, err)
	}

	if err := r.daemonReload(ctx); err != nil {
		return err
	}
	if _, err := r.systemctlRun(ctx, "restart", unit.ServiceName); err != nil {
		return fmt.Errorf("starting %s: %w%s", unit.ServiceName, err, r.diagnose(ctx, app.Name))
	}
	return nil
}

// Remove stops an application and deletes its definition.
//
// Volumes are deliberately left alone.
// Removing an application from Git is a configuration change, deleting its data is not, and podcd will not do it.
func (r *Runtime) Remove(ctx context.Context, app string) error {
	service := renderer.ServiceName(app)
	if _, err := r.systemctlRun(ctx, "stop", service); err != nil {
		// A unit that is not loaded is already stopped; anything else matters.
		if !strings.Contains(err.Error(), "not loaded") && !strings.Contains(err.Error(), "not found") {
			return fmt.Errorf("stopping %s: %w", service, err)
		}
	}
	if err := removeIfExists(filepath.Join(r.unitDir, renderer.KubeFileName(app))); err != nil {
		return fmt.Errorf("removing unit for %s: %w", app, err)
	}
	_ = removeIfExists(r.rend.ManifestPath(app))
	if err := r.daemonReload(ctx); err != nil {
		return err
	}
	// Quadlet normally removes the container (or plays the pod down) on stop.
	// If something interrupted that, the names must still be free for the next reconcile.
	// Volumes are untouched either way.
	_, _ = r.podmanRun(ctx, "pod", "rm", "--force", "--time", "10", app)
	return nil
}

// removeIfExists deletes a file; a missing file, or no path at all, is not an error.
func removeIfExists(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// yamlPathOf reads the Yaml= line back out of a .kube unit.
func yamlPathOf(content []byte) string {
	for _, line := range strings.Split(string(content), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "Yaml="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// Restart restarts an application whose unit is already correct.
func (r *Runtime) Restart(ctx context.Context, app string) error {
	service := renderer.ServiceName(app)
	if _, err := r.systemctlRun(ctx, "restart", service); err != nil {
		return fmt.Errorf("restarting %s: %w%s", service, err, r.diagnose(ctx, app))
	}
	return nil
}

// Health reports whether every workload container is running. Podman
// healthchecks are intentionally not part of podcd's readiness decision.
func (r *Runtime) Health(ctx context.Context, app model.Application) (model.Health, error) {
	containers, err := r.listContainers(ctx, app.Name)
	if err != nil {
		return model.Health{App: app.Name, CheckedAt: time.Now().UTC()}, err
	}
	return healthForContainers(app.Name, workload(containers[app.Name]), time.Now().UTC()), nil
}

func healthForContainers(app string, containers []containerInfo, checkedAt time.Time) model.Health {
	h := model.Health{App: app, CheckedAt: checkedAt}
	if len(containers) == 0 {
		h.Status, h.Message = model.HealthUnhealthy, "no containers"
		return h
	}
	var notRunning []string
	for _, c := range containers {
		if c.state != "running" {
			notRunning = append(notRunning, fmt.Sprintf("%s is %s", c.name, cmp.Or(c.state, "in an unknown state")))
		}
	}
	if len(notRunning) > 0 {
		h.Status, h.Message = model.HealthUnhealthy, strings.Join(notRunning, ", ")
		return h
	}
	h.Status, h.Message = model.HealthHealthy, "all workload containers running"
	return h
}

// WaitHealthy asks again until the application is healthy or the retries run
// out. It is what the reconciler uses right after applying a change: an app
// that never comes up should fail the reconcile, not quietly stay broken.
func (r *Runtime) WaitHealthy(ctx context.Context, app model.Application) model.Health {
	var last model.Health
	for attempt := 0; attempt <= waitRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				last.Message = "gave up waiting: " + ctx.Err().Error()
				last.Status = model.HealthUnknown
				return last
			case <-time.After(waitInterval):
			}
		}
		h, err := r.Health(ctx, app)
		if err != nil {
			h = model.Health{App: app.Name, Status: model.HealthUnknown, Message: err.Error(), CheckedAt: time.Now().UTC()}
		}
		last = h
		if last.OK() {
			return last
		}
	}
	return last
}

// Logs returns the most recent journal lines for an application.
func (r *Runtime) Logs(ctx context.Context, app string, lines int) (string, error) {
	if lines <= 0 {
		lines = 50
	}
	return r.run(ctx, r.journalctl, "--user", "-u", renderer.ServiceName(app),
		"-n", strconv.Itoa(lines), "--no-pager", "--output=short-iso")
}

func (r *Runtime) daemonReload(ctx context.Context) error {
	if _, err := r.systemctlRun(ctx, "daemon-reload"); err != nil {
		return fmt.Errorf("systemd daemon-reload: %w", err)
	}
	return nil
}

// diagnose adds the tail of the unit's journal to an error.
// "Job failed" on its own has never helped anybody at 3am.
func (r *Runtime) diagnose(ctx context.Context, app string) string {
	logs, err := r.Logs(ctx, app, 15)
	if err != nil || strings.TrimSpace(logs) == "" {
		return ""
	}
	return "\nlast log lines for " + renderer.ServiceName(app) + ":\n" + strings.TrimRight(logs, "\n")
}

func (r *Runtime) systemctlRun(ctx context.Context, args ...string) (string, error) {
	return r.run(ctx, r.systemctl, append([]string{"--user"}, args...)...)
}

func (r *Runtime) podmanRun(ctx context.Context, args ...string) (string, error) {
	return r.run(ctx, r.podman, args...)
}

func (r *Runtime) run(ctx context.Context, bin string, args ...string) (string, error) {
	return subprocess.Run(ctx, subprocess.Command{Bin: bin, Args: args, Env: sessionEnv(), Timeout: r.timeout})
}

// sessionEnv makes sure systemctl --user can find the user's session bus, even when the agent was started from cron, a shell over a serial console, or a systemd service without a full session environment.
func sessionEnv() []string {
	env := os.Environ()
	uid := os.Getuid()
	if os.Getenv("XDG_RUNTIME_DIR") == "" {
		env = append(env, "XDG_RUNTIME_DIR=/run/user/"+strconv.Itoa(uid))
	}
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
		if runtimeDir == "" {
			runtimeDir = "/run/user/" + strconv.Itoa(uid)
		}
		env = append(env, "DBUS_SESSION_BUS_ADDRESS=unix:path="+runtimeDir+"/bus")
	}
	return append(env, "LC_ALL=C")
}

// quadletGeneratorPresent reports whether podman's Quadlet generator is installed.
func quadletGeneratorPresent() bool {
	for _, p := range []string{
		"/usr/lib/systemd/user-generators/podman-user-generator",
		"/usr/libexec/podman/quadlet",
		"/usr/lib/podman/quadlet",
		"/usr/local/lib/systemd/user-generators/podman-user-generator",
	} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}
