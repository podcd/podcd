// Package podman runs pods as rootless Podman under systemd --user, via Quadlet .kube units.
// The agent writes units and asks systemd to run them; it only queries podman.
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

// WaitHealthy polling; variables so tests can shorten them.
var (
	waitRetries  = 15
	waitInterval = 2 * time.Second
)

// Options configures a Runtime.
type Options struct {
	UnitDir string
	// KubeDir holds played manifests, which may contain resolved secrets.
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

	// exec runs every podman, systemctl and journalctl call; tests replace it.
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

// Renderer is what this runtime writes with, so the planner compares the same bytes.
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
		// Non-zero for "degraded" and "starting" too; only no user manager is fatal.
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
			// One file's header names an app another file is named after; don't guess which systemd picked.
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
		// An edited or deleted manifest needs a re-apply even if the unit's bytes match.
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

	// Labelled containers without a unit (half-removed app, unit deleted by hand) are still ours.
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
	id       string
	name     string
	image    string
	state    string // running, exited, created, paused
	exitCode int
	health   string // healthcheck verdict, empty without a check
	restarts int
	infra    bool
}

// healthFromStatus: "Up 2 minutes (healthy)" -> "healthy".
func healthFromStatus(status string) string {
	for _, v := range []string{"healthy", "unhealthy", "starting"} {
		if strings.Contains(status, "("+v+")") {
			return v
		}
	}
	return ""
}

// listContainers returns every podcd-labelled container (infra included), grouped by app; app narrows it to one.
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

// workload drops infra containers.
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

// Apply writes the unit for one application and (re)starts it.
func (r *Runtime) Apply(ctx context.Context, app model.Application) error {
	unit, err := r.rend.Render(app)
	if err != nil {
		return err
	}

	// Manifest may contain resolved secrets: readable by the agent user only.
	if err := atomicfile.Write(unit.ManifestPath, unit.Manifest, 0o600); err != nil {
		return fmt.Errorf("writing manifest for %s: %w", app.Name, err)
	}

	// Retire units for this app under another file name (older podcd).
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

// Remove stops an application and deletes its unit and manifest. Volumes (data) are kept.
func (r *Runtime) Remove(ctx context.Context, app string) error {
	// Match by header as Inspect does, so older file names are removed too.
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
	// Quadlet normally plays the pod down on stop; force it in case that was interrupted.
	_, _ = r.podmanRun(ctx, "pod", "rm", "--force", "--time", "10", app)
	return nil
}

// unitFilesFor returns unit files named after app or naming it in their header.
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

// stopUnitFile stops a unit's service and deletes the unit and its manifest.
func (r *Runtime) stopUnitFile(ctx context.Context, path string) error {
	service := renderer.ServiceNameOfFile(filepath.Base(path))
	if _, err := r.systemctlRun(ctx, "stop", service); err != nil && !unitUnknown(err) {
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

// removeIfExists ignores a missing file or empty path.
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

// Health: init containers completed and every regular container running.
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
	// Explain why: inspect containers that still exist, else read the journal.
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

// lastContainerOutput returns the last container stdout/stderr lines (via conmon) from the unit's journal.
func (r *Runtime) lastContainerOutput(ctx context.Context, app string) string {
	out, err := r.run(ctx, r.journalctl, "--user", "-u", renderer.ServiceName(app),
		"_COMM=conmon", "-n", "3", "--no-pager", "--output=cat")
	if err != nil {
		return ""
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		l = strings.TrimSpace(l)
		// skip conmon's own warnings
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

// troubled names containers not running, or with a failing or not-yet-passed healthcheck.
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

// explain returns one short reason per container from a single podman inspect.
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

// lastLine returns the final non-empty line, where a failing probe says why.
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
	// kube play removes finished init containers, so an absent one has completed.
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

	// All running; now healthchecks. "starting" with restarts > 0 is a crash loop.
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

// WaitHealthy polls Health until healthy or out of retries.
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

// diagnose returns the tail of the unit's journal, to append to an error.
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

// sessionEnv fills in the user session bus for systemctl --user when the environment lacks it.
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
