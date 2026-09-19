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
// Variables rather than constants so a test can wait milliseconds, not minutes.
var (
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

	// exec runs one process. Every podman, systemctl and journalctl call goes
	// through it, which is the seam a test replaces to feed the runtime
	// recorded output instead of a real host.
	exec func(context.Context, subprocess.Command) (string, error)
}

// New returns a Podman runtime writing units into opts.UnitDir.
func New(opts Options) *Runtime {
	r := &Runtime{
		unitDir:    opts.UnitDir,
		podman:     cmp.Or(opts.PodmanBin, "podman"),
		systemctl:  cmp.Or(opts.SystemctlBin, "systemctl"),
		journalctl: "journalctl",
		timeout:    cmp.Or(opts.Timeout, 2*time.Minute),
		exec:       subprocess.Run,
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
			UnitName:     renderer.ServiceNameOfFile(e.Name()),
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
			for _, w := range c {
				cur.Containers = append(cur.Containers, model.ContainerStatus{
					Name: w.name, State: w.state, Health: w.health, Restarts: w.restarts,
				})
			}
		}
		state.Apps[app] = cur
	}

	if err := r.fillUnitStates(ctx, state); err != nil {
		return state, err
	}
	if err := r.inspectNetworks(ctx, &state); err != nil {
		return state, err
	}
	return state, nil
}

type containerInfo struct {
	id    string
	name  string
	image string
	// state is podman's own word for it: running, exited, created, paused.
	state    string
	exitCode int
	// health is the healthcheck verdict `podman ps` folds into its status
	// column - "Up 2 minutes (healthy)" - or empty when there is no check.
	health   string
	restarts int
	infra    bool
}

// healthFromStatus pulls the healthcheck verdict out of a `podman ps` status
// line: "Up 2 minutes (healthy)" -> "healthy". No parenthetical, no check.
func healthFromStatus(status string) string {
	for _, v := range []string{"healthy", "unhealthy", "starting"} {
		if strings.Contains(status, "("+v+")") {
			return v
		}
	}
	return ""
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
		ID       string            `json:"Id"`
		Names    []string          `json:"Names"`
		Image    string            `json:"Image"`
		State    string            `json:"State"`
		Status   string            `json:"Status"`
		ExitCode int               `json:"ExitCode"`
		Restarts int               `json:"Restarts"`
		IsInfra  bool              `json:"IsInfra"`
		Labels   map[string]string `json:"Labels"`
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
			id: id, name: name, image: c.Image, state: c.State, exitCode: c.ExitCode,
			health: healthFromStatus(c.Status), restarts: c.Restarts, infra: c.IsInfra,
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
		app.UnitState = unitStateOf(props)
		state.Apps[n] = app
	}
	return nil
}

// unitStateOf maps one `systemctl show` block to podcd's own words for it.
func unitStateOf(props map[string]string) model.UnitState {
	switch {
	case props == nil:
		return model.UnitMissing
	case props["LoadState"] == "not-found":
		return model.UnitMissing
	case props["ActiveState"] == "active":
		return model.UnitActive
	case props["ActiveState"] == "activating":
		return model.UnitActivating
	case props["ActiveState"] == "failed":
		return model.UnitFailed
	case props["ActiveState"] == "inactive":
		return model.UnitInactive
	case props["ActiveState"] == "":
		return model.UnitUnknown
	default:
		// deactivating, reloading, or a systemd state podcd does not know.
		return model.UnitState(props["ActiveState"])
	}
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

	// A unit for this application under another file name - written by an
	// older podcd - would be a second unit claiming the same app once ours
	// is in place. Retire it first.
	for _, stale := range r.unitFilesFor(app.Name) {
		if stale == unit.Path {
			continue
		}
		if err := r.stopUnitFile(ctx, stale); err != nil {
			return err
		}
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
	// The unit is whichever file claims this app, not just the name we would
	// write today: Inspect finds units by their header, so Remove must too,
	// or an older file name is reported as managed and then never removed.
	files := r.unitFilesFor(app)
	if len(files) == 0 {
		files = []string{filepath.Join(r.unitDir, renderer.KubeFileName(app))}
	}
	for _, path := range files {
		if err := r.stopUnitFile(ctx, path); err != nil {
			return err
		}
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

// unitFilesFor returns every unit file in the unit directory that belongs to
// app: named after it, or carrying its name in the header marker.
func (r *Runtime) unitFilesFor(app string) []string {
	entries, err := os.ReadDir(r.unitDir)
	if err != nil {
		return nil
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name, ok := renderer.AppFromFileName(e.Name())
		if !ok {
			continue
		}
		path := filepath.Join(r.unitDir, e.Name())
		if name != app {
			content, err := os.ReadFile(path)
			if err != nil || renderer.ParseMarkers(content).App != app {
				continue
			}
		}
		files = append(files, path)
	}
	return files
}

// stopUnitFile stops the service a unit file defines and deletes the file
// and the manifest it plays. The service name follows the file name, as
// Quadlet derives it.
func (r *Runtime) stopUnitFile(ctx context.Context, path string) error {
	service := renderer.ServiceNameOfFile(filepath.Base(path))
	if _, err := r.systemctlRun(ctx, "stop", service); err != nil && !unitUnknown(err) {
		// A unit that is not loaded is already stopped; anything else matters.
		return fmt.Errorf("stopping %s: %w", service, err)
	}
	if content, err := os.ReadFile(path); err == nil {
		_ = removeIfExists(yamlPathOf(content))
	}
	if err := removeIfExists(path); err != nil {
		return fmt.Errorf("removing unit %s: %w", path, err)
	}
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

// Health reports whether the init containers completed successfully and every
// regular workload container is running. Podman healthchecks are intentionally
// not part of podcd's readiness decision.
func (r *Runtime) Health(ctx context.Context, app model.Application) (model.Health, error) {
	containers, err := r.listContainers(ctx, app.Name)
	if err != nil {
		return model.Health{App: app.Name, CheckedAt: time.Now().UTC()}, err
	}
	running := workload(containers[app.Name])
	h := healthForContainers(app.Name, running, app.InitContainers, time.Now().UTC())
	if h.OK() {
		return h, nil
	}
	// `podman ps` gives the verdict; the reason takes one more call, and only
	// on this path. A container that still exists can be inspected - the
	// probe's own output, the failing streak, an OOM kill. One that Quadlet
	// already tore down has only what it wrote to the journal.
	var why string
	if len(running) == 0 {
		why = r.lastContainerOutput(ctx, app.Name)
	} else {
		why = r.explain(ctx, troubled(running, app.InitContainers))
	}
	if why != "" {
		h.Message += "; " + why
	}
	return h, nil
}

// lastContainerOutput returns the last thing the application's containers
// wrote before they went away. The unit's journal carries podman's own
// events too (died, cleanup, removed - dozens of lines per pod), so this
// keeps only what came through conmon, which is container stdout and stderr.
func (r *Runtime) lastContainerOutput(ctx context.Context, app string) string {
	out, err := r.run(ctx, r.journalctl, "--user", "-u", renderer.ServiceName(app),
		"_COMM=conmon", "-n", "3", "--no-pager", "--output=cat")
	if err != nil {
		return ""
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		l = strings.TrimSpace(l)
		// conmon's own warnings ("conmon <id> <nwarn>: ...") travel the same
		// way as the container's output; they are about podman, not the app.
		if l == "" || strings.HasPrefix(l, "-- ") || strings.HasPrefix(l, "conmon ") {
			continue
		}
		lines = append(lines, l)
	}
	if len(lines) == 0 {
		return ""
	}
	return "last output: " + strings.Join(lines, " | ")
}

// troubled names the containers a bad verdict is about: not running, or
// running with a healthcheck that is failing or has not passed.
func troubled(containers []containerInfo, initNames []string) []string {
	var out []string
	for _, c := range containers {
		if model.IsInitContainer(initNames, c.name) && c.state == "exited" && c.exitCode == 0 {
			continue
		}
		if c.state != "running" || c.health == "unhealthy" || c.health == "starting" {
			out = append(out, c.name)
		}
	}
	return out
}

// explain asks podman why the named containers are in the state they are in,
// in one call, and returns a short human line per container.
func (r *Runtime) explain(ctx context.Context, names []string) string {
	if len(names) == 0 {
		return ""
	}
	out, err := r.podmanRun(ctx, append([]string{"inspect", "--format", "json"}, names...)...)
	if err != nil {
		return ""
	}
	var raw []struct {
		Name  string `json:"Name"`
		State struct {
			Error     string `json:"Error"`
			OOMKilled bool   `json:"OOMKilled"`
			ExitCode  int    `json:"ExitCode"`
			Health    *struct {
				FailingStreak int `json:"FailingStreak"`
				Log           []struct {
					ExitCode int    `json:"ExitCode"`
					Output   string `json:"Output"`
				} `json:"Log"`
			} `json:"Health"`
		} `json:"State"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return ""
	}
	var parts []string
	for _, c := range raw {
		var why []string
		if c.State.OOMKilled {
			why = append(why, "OOM killed")
		}
		if c.State.Error != "" {
			why = append(why, c.State.Error)
		}
		if h := c.State.Health; h != nil {
			if h.FailingStreak > 0 {
				why = append(why, fmt.Sprintf("%d consecutive check failure%s", h.FailingStreak, plural(h.FailingStreak)))
			}
			if n := len(h.Log); n > 0 && h.Log[n-1].ExitCode != 0 {
				why = append(why, "last check: "+lastLine(h.Log[n-1].Output))
			}
		}
		if len(why) > 0 {
			parts = append(parts, strings.TrimPrefix(c.Name, "/")+": "+strings.Join(why, ", "))
		}
	}
	return strings.Join(parts, "; ")
}

// lastLine returns the final non-empty line of command output, trimmed, since
// that is where a failing probe says what went wrong.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return strings.TrimSpace(s)
}

func healthForContainers(app string, containers []containerInfo, initNames []string, checkedAt time.Time) model.Health {
	h := model.Health{App: app, CheckedAt: checkedAt}
	regular, init := splitInitContainers(containers, initNames)
	if len(regular) == 0 {
		h.Status, h.Message = model.HealthUnhealthy, "no containers"
		return h
	}
	// Only an init container podman still shows can say anything. kube play
	// creates them as type "once" and removes them when they finish, so one
	// that is absent has completed - and one that never ran leaves the regular
	// containers un-started, which the check below catches.
	var incomplete, failed []string
	for _, c := range init {
		switch c.state {
		case "exited":
			if c.exitCode != 0 {
				failed = append(failed, fmt.Sprintf("%s exited with code %d", c.name, c.exitCode))
			}
		case "running", "created", "configured":
			incomplete = append(incomplete, c.name)
		default:
			failed = append(failed, fmt.Sprintf("%s is %s", c.name, cmp.Or(c.state, "in an unknown state")))
		}
	}
	if len(failed) > 0 {
		h.Status, h.Message = model.HealthUnhealthy, "init container failed: "+strings.Join(failed, ", ")
		return h
	}
	if len(incomplete) > 0 {
		h.Status, h.Message = model.HealthUnknown, "init container still running: "+strings.Join(incomplete, ", ")
		return h
	}
	var notRunning []string
	for _, c := range regular {
		if c.state != "running" {
			notRunning = append(notRunning, fmt.Sprintf("%s is %s", c.name, cmp.Or(c.state, "in an unknown state")))
		}
	}
	if len(notRunning) > 0 {
		h.Status, h.Message = model.HealthUnhealthy, strings.Join(notRunning, ", ")
		return h
	}

	// Everything is up. Now podman's own verdict, for the containers that
	// declare a healthcheck. A restart count travels with it: "starting" on a
	// container that has restarted before is a crash loop, not a warm-up.
	var unhealthy, starting, restarted []string
	for _, c := range regular {
		if c.restarts > 0 {
			restarted = append(restarted, fmt.Sprintf("%s restarted %d time%s", c.name, c.restarts, plural(c.restarts)))
		}
		switch c.health {
		case "unhealthy":
			unhealthy = append(unhealthy, c.name)
		case "starting":
			starting = append(starting, c.name)
		}
	}
	switch {
	case len(unhealthy) > 0:
		h.Status, h.Message = model.HealthUnhealthy, "healthcheck failing: "+strings.Join(unhealthy, ", ")
	case len(starting) > 0:
		h.Status, h.Message = model.HealthUnknown, "healthcheck not passed yet: "+strings.Join(starting, ", ")
	default:
		h.Status, h.Message = model.HealthHealthy, "all workload containers running"
	}
	if len(restarted) > 0 {
		h.Message += " (" + strings.Join(restarted, ", ") + ")"
	}
	return h
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// splitInitContainers separates completed setup work from the regular workload.
// podman kube play prefixes a Kubernetes container name with the pod name, so
// matching the final "-<container>" portion works for both Podman-generated
// names and plain names returned by other Podman versions.
func splitInitContainers(containers []containerInfo, initNames []string) (regular, init []containerInfo) {
	for _, c := range containers {
		if model.IsInitContainer(initNames, c.name) {
			init = append(init, c)
		} else {
			regular = append(regular, c)
		}
	}
	return regular, init
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
	return r.exec(ctx, subprocess.Command{Bin: bin, Args: args, Env: sessionEnv(), Timeout: r.timeout})
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
