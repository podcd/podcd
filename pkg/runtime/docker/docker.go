// Package docker runs applications as Compose projects on a Docker daemon.
//
// Docker has no Quadlet and no `kube play`, so the podman shape is kept where
// it matters and translated where it does not. The renderer's unit for an
// application is still written to disk, as the record of what was applied:
// the planner compares those bytes to what it would render, the same as for
// podman, and this runtime never has to be asked "is this current?". What
// runs is a Compose project built from the played manifest (see compose.go),
// and Docker itself is what the containers are read back from.
package docker

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// How long WaitHealthy keeps asking after a change is applied.
var (
	waitRetries  = 15
	waitInterval = 2 * time.Second
)

// labelNetwork is the label a podcd-created network carries, the same one
// the podman runtime uses.
const labelNetwork = "io.podcd.network"

// Options configures a Runtime.
type Options struct {
	// UnitDir holds the rendered units this runtime keeps as its record of
	// what was applied, and one project directory per application. It must
	// not be a directory Quadlet reads.
	UnitDir string
	// KubeDir holds the played manifests. It can contain resolved secrets.
	KubeDir string

	// DockerBin defaults to "docker" on PATH. Compose is used through it.
	DockerBin string
	// PauseImage is what the pod's infra container runs.
	PauseImage string

	// Timeout bounds any single docker invocation.
	Timeout time.Duration
}

// DefaultPauseImage is the infra container's image when none is configured.
const DefaultPauseImage = "registry.k8s.io/pause:3.10"

// Runtime is the Docker + Compose implementation.
type Runtime struct {
	unitDir    string
	docker     string
	pauseImage string
	timeout    time.Duration

	rend *renderer.Renderer

	// exec runs one process: the seam a test replaces with recorded output.
	exec func(context.Context, subprocess.Command) (string, error)
}

// New returns a Docker runtime keeping its records under opts.UnitDir.
func New(opts Options) *Runtime {
	return &Runtime{
		unitDir:    opts.UnitDir,
		docker:     cmp.Or(opts.DockerBin, "docker"),
		pauseImage: cmp.Or(opts.PauseImage, DefaultPauseImage),
		timeout:    cmp.Or(opts.Timeout, 5*time.Minute),
		rend:       &renderer.Renderer{UnitDir: opts.UnitDir, KubeDir: opts.KubeDir},
		exec:       subprocess.Run,
	}
}

// Renderer exposes the renderer this runtime records with, so the planner
// compares against the bytes the runtime would write.
func (r *Runtime) Renderer() *renderer.Renderer { return r.rend }

// Name implements runtime.Runtime.
func (r *Runtime) Name() string { return "docker" }

// Available reports whether a Docker daemon with Compose can be reached.
func (r *Runtime) Available(ctx context.Context) (bool, string) {
	if _, err := exec.LookPath(r.docker); err != nil {
		return false, fmt.Sprintf("docker is not installed (%v)", err)
	}
	if _, err := r.dockerRun(ctx, "version", "--format", "{{.Server.Version}}"); err != nil {
		return false, "no docker daemon: " + err.Error()
	}
	if _, err := r.dockerRun(ctx, "compose", "version", "--short"); err != nil {
		return false, "docker compose is not installed: " + err.Error()
	}
	return true, ""
}

// projectDir is where an application's compose file and mounted files live.
func (r *Runtime) projectDir(app string) string { return filepath.Join(r.unitDir, app) }

// composePath is the compose file for an application.
func (r *Runtime) composePath(app string) string {
	return filepath.Join(r.projectDir(app), "compose.yaml")
}

// Inspect reads the records on disk and what Docker has running.
func (r *Runtime) Inspect(ctx context.Context) (model.ActualState, error) {
	state := model.ActualState{Runtime: r.Name(), Apps: map[string]model.ActualApp{}, Networks: map[string]model.ActualNetwork{}}

	entries, err := os.ReadDir(r.unitDir)
	if err != nil && !os.IsNotExist(err) {
		return state, fmt.Errorf("reading %s: %w", r.unitDir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := filepath.Join(r.unitDir, e.Name())
		if name, ok := renderer.AppFromFileName(e.Name()); ok {
			content, err := os.ReadFile(path)
			if err != nil {
				return state, fmt.Errorf("reading %s: %w", path, err)
			}
			m := renderer.ParseMarkers(content)
			if m.App != "" {
				name = m.App
			}
			if prev, ok := state.Apps[name]; ok {
				return state, fmt.Errorf("application %q has two unit files: %s and %s; remove one", name, prev.UnitFile, path)
			}
			cur := model.ActualApp{
				Name:         name,
				Managed:      m.Managed,
				UnitFile:     path,
				UnitFileHash: model.HashBytes(content),
				UnitContent:  content,
				SpecHash:     m.SpecHash,
				UnitName:     projectName(name),
				UnitState:    model.UnitInactive,
			}
			// The record is only as current as the manifest it stands for.
			if m.Managed {
				data, err := os.ReadFile(r.rend.ManifestPath(name))
				if err != nil || model.HashBytes(data) != m.ManifestHash {
					cur.UnitFileHash = "manifest-drift:" + cur.UnitFileHash
				}
				cur.ManifestContent = data
			}
			state.Apps[name] = cur
			continue
		}
		if name, ok := renderer.NetworkFromFileName(e.Name()); ok {
			content, err := os.ReadFile(path)
			if err != nil {
				return state, fmt.Errorf("reading %s: %w", path, err)
			}
			m := renderer.ParseMarkers(content)
			if m.Network != "" {
				name = m.Network
			}
			if prev, ok := state.Networks[name]; ok {
				return state, fmt.Errorf("network %q has two unit files: %s and %s; remove one", name, prev.UnitFile, path)
			}
			state.Networks[name] = model.ActualNetwork{
				Name:         name,
				Managed:      m.Managed,
				UnitFile:     path,
				UnitFileHash: model.HashBytes(content),
				UnitContent:  content,
				SpecHash:     m.SpecHash,
				UnitState:    model.UnitInactive,
			}
		}
	}

	// Containers labelled as ours, with or without a record: a record whose
	// project was taken down by hand, or a project whose record is gone, are
	// both still ours.
	containers, err := r.listContainers(ctx, "")
	if err != nil {
		return state, err
	}
	for app, all := range containers {
		cur, ok := state.Apps[app]
		if !ok {
			cur = model.ActualApp{Name: app, Managed: true, UnitName: projectName(app), UnitState: model.UnitMissing}
		}
		cur.UnitState, cur.SubState = unitStateOf(all)
		if c := workload(all); len(c) > 0 {
			cur.ContainerID = c[0].id
			cur.ContainerImage = c[0].image
			cur.ContainerState = c[0].state
			for _, w := range c {
				cur.Containers = append(cur.Containers, model.ContainerStatus{Name: w.name, State: w.state, Health: w.health})
			}
		}
		state.Apps[app] = cur
	}

	existing, err := r.listNetworks(ctx)
	if err != nil {
		return state, err
	}
	for name, ours := range existing {
		cur, ok := state.Networks[name]
		if !ok {
			if !ours {
				continue
			}
			cur = model.ActualNetwork{Name: name, Managed: true, UnitState: model.UnitMissing}
		}
		cur.Exists = true
		cur.UnitState = model.UnitActive
		state.Networks[name] = cur
	}
	return state, nil
}

// unitStateOf stands in for what systemd would say: there is no unit, so the
// containers speak for the project. Running is active; present but stopped is
// inactive, and the planner restarts it.
func unitStateOf(containers []containerInfo) (model.UnitState, string) {
	var states []string
	for _, c := range workload(containers) {
		states = append(states, c.state)
	}
	if slices.Contains(states, "running") {
		return model.UnitActive, "running"
	}
	if len(states) == 0 {
		return model.UnitInactive, "dead"
	}
	return model.UnitInactive, strings.Join(slices.Compact(slices.Sorted(slices.Values(states))), ",")
}

type containerInfo struct {
	id    string
	name  string
	image string
	// state is docker's own word for it: running, exited, created, restarting, paused, dead.
	state    string
	exitCode int
	health   string
	infra    bool
}

// exitedStatus reads the exit code out of docker's status column:
// "Exited (1) 3 minutes ago".
var exitedStatus = regexp.MustCompile(`^Exited \((\d+)\)`)

// healthFromStatus pulls the healthcheck verdict out of the status column:
// "Up 2 minutes (healthy)" -> "healthy". Docker says "health: starting".
func healthFromStatus(status string) string {
	switch {
	case strings.Contains(status, "(healthy)"):
		return "healthy"
	case strings.Contains(status, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(status, "(health: starting)"):
		return "starting"
	}
	return ""
}

// listContainers asks Docker for everything labelled as ours, grouped by
// app and sorted by name. Passing an app narrows it to that one.
func (r *Runtime) listContainers(ctx context.Context, app string) (map[string][]containerInfo, error) {
	args := []string{"ps", "--all", "--no-trunc", "--filter", "label=" + config.LabelManaged + "=true"}
	if app != "" {
		args = append(args, "--filter", "label="+config.LabelApp+"="+app)
	}
	out, err := r.dockerRun(ctx, append(args, "--format", "{{json .}}")...)
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}
	result := map[string][]containerInfo{}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var raw struct {
			ID     string `json:"ID"`
			Names  string `json:"Names"`
			Image  string `json:"Image"`
			State  string `json:"State"`
			Status string `json:"Status"`
			Labels string `json:"Labels"`
		}
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			return nil, fmt.Errorf("parsing docker ps output: %w", err)
		}
		labels := parseLabels(raw.Labels)
		app := labels[config.LabelApp]
		if app == "" {
			continue
		}
		id := raw.ID
		if len(id) > 12 {
			id = id[:12]
		}
		c := containerInfo{id: id, name: raw.Names, image: raw.Image, state: raw.State,
			health: healthFromStatus(raw.Status), infra: labels[labelInfra] == "true"}
		if m := exitedStatus.FindStringSubmatch(raw.Status); m != nil {
			c.exitCode, _ = strconv.Atoi(m[1])
		}
		result[app] = append(result[app], c)
	}
	for app := range result {
		slices.SortFunc(result[app], func(a, b containerInfo) int { return strings.Compare(a.name, b.name) })
	}
	return result, nil
}

// parseLabels reads docker's "k=v,k=v" label column.
func parseLabels(s string) map[string]string {
	out := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

// workload drops the infra container.
func workload(containers []containerInfo) []containerInfo {
	var out []containerInfo
	for _, c := range containers {
		if !c.infra {
			out = append(out, c)
		}
	}
	return out
}

// Apply writes the record and manifest for one application, builds its
// Compose project and brings it up. Every container is recreated, as podman
// replays a pod: init containers run again, in order, before the rest.
func (r *Runtime) Apply(ctx context.Context, app model.Application) error {
	unit, err := r.rend.Render(app)
	if err != nil {
		return err
	}
	if err := atomicfile.Write(unit.ManifestPath, unit.Manifest, 0o600); err != nil {
		return fmt.Errorf("writing manifest for %s: %w", app.Name, err)
	}
	if err := atomicfile.Write(unit.Path, unit.Content, 0o644); err != nil {
		return fmt.Errorf("writing unit for %s: %w", app.Name, err)
	}
	return r.play(ctx, app.Name, app.Manifest)
}

// play builds the Compose project for an application from its manifest and
// brings it up. The manifest is the whole input: the pod carries its restart
// policy, its ports and, as an annotation, its networks.
func (r *Runtime) play(ctx context.Context, app string, manifest []byte) error {
	m, err := parseManifest(manifest)
	if err != nil {
		return fmt.Errorf("%s: %w", app, err)
	}
	dir := r.projectDir(app)
	cf, files, err := compose(app, m, dir, r.pauseImage)
	if err != nil {
		return fmt.Errorf("%s: %w", app, err)
	}
	data, err := marshalCompose(cf)
	if err != nil {
		return fmt.Errorf("%s: rendering compose file: %w", app, err)
	}
	// Mounted ConfigMaps and Secrets are laid out fresh: a key removed in
	// Git must not linger as a file.
	for _, sub := range []string{"configmaps", "secrets"} {
		if err := os.RemoveAll(filepath.Join(dir, sub)); err != nil {
			return fmt.Errorf("%s: clearing %s: %w", app, filepath.Join(dir, sub), err)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("%s: creating %s: %w", app, dir, err)
	}
	for _, f := range files {
		if err := atomicfile.Write(f.path, f.data, f.mode); err != nil {
			return fmt.Errorf("%s: writing %s: %w", app, f.path, err)
		}
	}
	// The compose file carries resolved secrets: agent user only.
	if err := atomicfile.Write(r.composePath(app), data, 0o600); err != nil {
		return fmt.Errorf("%s: writing compose file: %w", app, err)
	}
	if _, err := r.composeRun(ctx, app, "up", "--detach", "--force-recreate", "--remove-orphans", "--quiet-pull"); err != nil {
		return fmt.Errorf("starting %s: %w%s", projectName(app), err, r.diagnose(ctx, app))
	}
	return nil
}

// Remove takes the project down and deletes its record, manifest and files.
//
// Volumes are deliberately left alone: `down` is run without --volumes.
func (r *Runtime) Remove(ctx context.Context, app string) error {
	if _, err := os.Stat(r.composePath(app)); err == nil {
		if _, err := r.composeRun(ctx, app, "down", "--remove-orphans"); err != nil {
			return fmt.Errorf("stopping %s: %w", projectName(app), err)
		}
	}
	// A project without its compose file, or one `down` left behind: the
	// containers are found by label so the names are free next time.
	containers, err := r.listContainers(ctx, app)
	if err != nil {
		return err
	}
	if left := containers[app]; len(left) > 0 {
		args := []string{"rm", "--force"}
		for _, c := range left {
			args = append(args, c.id)
		}
		if _, err := r.dockerRun(ctx, args...); err != nil {
			return fmt.Errorf("removing containers of %s: %w", app, err)
		}
	}
	for _, path := range []string{filepath.Join(r.unitDir, renderer.KubeFileName(app)), r.rend.ManifestPath(app)} {
		if err := removeIfExists(path); err != nil {
			return fmt.Errorf("removing %s: %w", path, err)
		}
	}
	if err := os.RemoveAll(r.projectDir(app)); err != nil {
		return fmt.Errorf("removing %s: %w", r.projectDir(app), err)
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

// Restart replays an application whose record is already correct, from the
// manifest on disk. The project is rebuilt rather than reused, so a compose
// file that was edited or deleted by hand does not matter.
func (r *Runtime) Restart(ctx context.Context, app string) error {
	manifest, err := os.ReadFile(r.rend.ManifestPath(app))
	if err != nil {
		return fmt.Errorf("restarting %s: %w", app, err)
	}
	return r.play(ctx, app, manifest)
}

// Health reports whether the init containers completed and every regular
// workload container is running, the same verdict the podman runtime gives.
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
	if why := r.explain(ctx, troubled(running, app.InitContainers)); why != "" {
		h.Message += "; " + why
	}
	return h, nil
}

func healthForContainers(app string, containers []containerInfo, initNames []string, checkedAt time.Time) model.Health {
	h := model.Health{App: app, CheckedAt: checkedAt}
	var regular, init []containerInfo
	for _, c := range containers {
		if model.IsInitContainer(initNames, c.name) {
			init = append(init, c)
		} else {
			regular = append(regular, c)
		}
	}
	if len(regular) == 0 {
		h.Status, h.Message = model.HealthUnhealthy, "no containers"
		return h
	}
	var incomplete, failed []string
	for _, c := range init {
		switch c.state {
		case "exited":
			if c.exitCode != 0 {
				failed = append(failed, fmt.Sprintf("%s exited with code %d", c.name, c.exitCode))
			}
		case "running", "created":
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
	var notRunning, unhealthy, starting []string
	for _, c := range regular {
		if c.state != "running" {
			notRunning = append(notRunning, fmt.Sprintf("%s is %s", c.name, cmp.Or(c.state, "in an unknown state")))
		}
		switch c.health {
		case "unhealthy":
			unhealthy = append(unhealthy, c.name)
		case "starting":
			starting = append(starting, c.name)
		}
	}
	switch {
	case len(notRunning) > 0:
		h.Status, h.Message = model.HealthUnhealthy, strings.Join(notRunning, ", ")
	case len(unhealthy) > 0:
		h.Status, h.Message = model.HealthUnhealthy, "healthcheck failing: "+strings.Join(unhealthy, ", ")
	case len(starting) > 0:
		h.Status, h.Message = model.HealthUnknown, "healthcheck not passed yet: "+strings.Join(starting, ", ")
	default:
		h.Status, h.Message = model.HealthHealthy, "all workload containers running"
	}
	return h
}

// troubled names the containers a bad verdict is about.
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

// explain asks docker why the named containers are in the state they are
// in, in one call, and returns a short human line per container.
func (r *Runtime) explain(ctx context.Context, names []string) string {
	if len(names) == 0 {
		return ""
	}
	out, err := r.dockerRun(ctx, append([]string{"inspect", "--format", "{{json .}}"}, names...)...)
	if err != nil {
		return ""
	}
	var parts []string
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var c struct {
			Name         string `json:"Name"`
			RestartCount int    `json:"RestartCount"`
			State        struct {
				Error     string `json:"Error"`
				OOMKilled bool   `json:"OOMKilled"`
				Health    *struct {
					FailingStreak int `json:"FailingStreak"`
					Log           []struct {
						ExitCode int    `json:"ExitCode"`
						Output   string `json:"Output"`
					} `json:"Log"`
				} `json:"Health"`
			} `json:"State"`
		}
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			continue
		}
		var why []string
		if c.State.OOMKilled {
			why = append(why, "OOM killed")
		}
		if c.State.Error != "" {
			why = append(why, c.State.Error)
		}
		if c.RestartCount > 0 {
			why = append(why, fmt.Sprintf("restarted %d time%s", c.RestartCount, plural(c.RestartCount)))
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

// lastLine returns the final non-empty line of command output, trimmed.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return strings.TrimSpace(s)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// WaitHealthy asks again until the application is healthy or the retries run
// out; the reconciler uses it right after applying a change.
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

// Logs returns the most recent lines every container of the project wrote.
func (r *Runtime) Logs(ctx context.Context, app string, lines int) (string, error) {
	if lines <= 0 {
		lines = 50
	}
	return r.composeRun(ctx, app, "logs", "--no-color", "--timestamps", "--tail", strconv.Itoa(lines))
}

// diagnose adds the tail of the project's logs to an error.
func (r *Runtime) diagnose(ctx context.Context, app string) string {
	logs, err := r.Logs(ctx, app, 15)
	if err != nil || strings.TrimSpace(logs) == "" {
		return ""
	}
	return "\nlast log lines for " + projectName(app) + ":\n" + strings.TrimRight(logs, "\n")
}

// listNetworks asks docker for every network, and reports for each whether
// it carries podcd's label.
func (r *Runtime) listNetworks(ctx context.Context) (map[string]bool, error) {
	out, err := r.dockerRun(ctx, "network", "ls", "--format", "{{json .}}")
	if err != nil {
		return nil, fmt.Errorf("listing networks: %w", err)
	}
	result := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var raw struct {
			Name   string `json:"Name"`
			Labels string `json:"Labels"`
		}
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			return nil, fmt.Errorf("parsing docker network ls output: %w", err)
		}
		result[raw.Name] = parseLabels(raw.Labels)[labelNetwork] != ""
	}
	return result, nil
}

// ApplyNetwork writes the record for one network and makes docker have it.
//
// Docker cannot change a network in place either. A network podcd already
// recorded is being changed: the containers on it are stopped - the planner
// restarts their applications afterwards - and it is removed and created
// again. One with no record yet is only created; if docker already has one
// of that name it is adopted as it is.
func (r *Runtime) ApplyNetwork(ctx context.Context, net model.Network) error {
	if net.DisableDNS || len(net.DNS) > 0 {
		return fmt.Errorf("network %s: disableDNS and dns are podman options docker does not have", net.Name)
	}
	unit, err := r.rend.RenderNetwork(net)
	if err != nil {
		return err
	}
	_, hadUnit := os.Stat(unit.Path)
	recreate := hadUnit == nil

	if err := atomicfile.Write(unit.Path, unit.Content, 0o644); err != nil {
		return fmt.Errorf("writing unit for network %s: %w", net.Name, err)
	}
	existing, err := r.listNetworks(ctx)
	if err != nil {
		return err
	}
	if _, exists := existing[net.Name]; exists {
		if !recreate {
			return nil
		}
		out, err := r.dockerRun(ctx, "ps", "--quiet", "--filter", "network="+net.Name)
		if err != nil {
			return fmt.Errorf("listing containers on network %s: %w", net.Name, err)
		}
		if ids := strings.Fields(out); len(ids) > 0 {
			if _, err := r.dockerRun(ctx, append([]string{"stop"}, ids...)...); err != nil {
				return fmt.Errorf("stopping containers on network %s: %w", net.Name, err)
			}
		}
		if _, err := r.dockerRun(ctx, "network", "rm", net.Name); err != nil {
			return fmt.Errorf("removing network %s so it can be recreated: %w", net.Name, err)
		}
	}
	args := []string{"network", "create",
		"--label", config.LabelManaged + "=true", "--label", labelNetwork + "=" + net.Name}
	if net.Driver != "" {
		args = append(args, "--driver", net.Driver)
	}
	if net.Subnet != "" {
		args = append(args, "--subnet", net.Subnet)
	}
	if net.Gateway != "" {
		args = append(args, "--gateway", net.Gateway)
	}
	if net.IPRange != "" {
		args = append(args, "--ip-range", net.IPRange)
	}
	if net.Internal {
		args = append(args, "--internal")
	}
	if net.IPv6 {
		args = append(args, "--ipv6")
	}
	for _, k := range slices.Sorted(maps.Keys(net.Options)) {
		args = append(args, "--opt", k+"="+net.Options[k])
	}
	if _, err := r.dockerRun(ctx, append(args, net.Name)...); err != nil {
		return fmt.Errorf("creating network %s: %w", net.Name, err)
	}
	return nil
}

// RemoveNetwork deletes a network's record and the network itself. It never
// forces: docker refuses to remove a network something is still on.
func (r *Runtime) RemoveNetwork(ctx context.Context, network string) error {
	if err := removeIfExists(filepath.Join(r.unitDir, renderer.NetworkFileName(network))); err != nil {
		return fmt.Errorf("removing unit for network %s: %w", network, err)
	}
	if _, err := r.dockerRun(ctx, "network", "rm", network); err != nil && !networkMissing(err) {
		return fmt.Errorf("removing network %s: %w", network, err)
	}
	return nil
}

// networkMissing reports docker's error for a network that does not exist.
func networkMissing(err error) bool {
	return strings.Contains(err.Error(), "No such network") || strings.Contains(err.Error(), "network not found")
}

// composeRun runs a compose command against one application's project.
func (r *Runtime) composeRun(ctx context.Context, app string, args ...string) (string, error) {
	base := []string{"compose", "--project-name", projectName(app)}
	if _, err := os.Stat(r.composePath(app)); err == nil {
		base = append(base, "--file", r.composePath(app))
	}
	return r.dockerRun(ctx, append(base, args...)...)
}

func (r *Runtime) dockerRun(ctx context.Context, args ...string) (string, error) {
	return r.exec(ctx, subprocess.Command{Bin: r.docker, Args: args, Env: append(os.Environ(), "LC_ALL=C"), Timeout: r.timeout})
}
