package podman

// These tests drive the runtime with recorded podman, systemctl and journalctl
// output instead of a host, through the exec seam. The recordings are verbatim
// from a real rootless podman 5.3 on Fedora CoreOS, so a change in how podcd
// reads them is caught here, on any machine, without a container in sight.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/podcd/podcd/internal/subprocess"
	"github.com/podcd/podcd/pkg/model"
	"github.com/podcd/podcd/pkg/renderer"
)

// A call is one process the runtime asked for.
type call struct {
	bin  string
	args string
}

// fake answers process calls from a script and records every one.
type fake struct {
	calls   []call
	respond func(bin string, args []string) (string, error)
}

func (f *fake) run(_ context.Context, c subprocess.Command) (string, error) {
	f.calls = append(f.calls, call{c.Bin, strings.Join(c.Args, " ")})
	if f.respond == nil {
		return "", nil
	}
	return f.respond(c.Bin, c.Args)
}

// has reports whether a call with this bin and argument substring was made.
func (f *fake) has(bin, argSub string) bool {
	for _, c := range f.calls {
		if c.bin == bin && strings.Contains(c.args, argSub) {
			return true
		}
	}
	return false
}

func (f *fake) count(bin string) int {
	n := 0
	for _, c := range f.calls {
		if c.bin == bin {
			n++
		}
	}
	return n
}

// newRuntime builds a runtime on temp directories wired to the fake.
func newRuntime(t *testing.T, respond func(bin string, args []string) (string, error)) (*Runtime, *fake) {
	t.Helper()
	f := &fake{respond: respond}
	r := New(Options{UnitDir: filepath.Join(t.TempDir(), "units"), KubeDir: filepath.Join(t.TempDir(), "kube")})
	if err := os.MkdirAll(r.unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	r.exec = f.run
	return r, f
}

// --- recorded podman output -------------------------------------------------

// psJSON is `podman ps --all --filter label=io.podcd.managed=true --format json`
// for a two-container pod plus a single-container one, as podman 5.3 prints it.
// Fields podcd does not read are omitted; the ones it reads are exact.
const psJSON = `[
 {"Id":"daaef2fa2c13aaaa","Names":["947a5a828fed-infra"],"Image":"localhost/podman-pause:5.3.1","State":"running","Status":"Up 22 seconds","ExitCode":0,"Restarts":0,"IsInfra":true,"Labels":{"io.podcd.app":"vault-dev","io.podcd.managed":"true"}},
 {"Id":"88f54f1c0dc1bbbb","Names":["vault-dev-vault"],"Image":"docker.io/hashicorp/vault:1.17","State":"running","Status":"Up 22 seconds (starting)","ExitCode":0,"Restarts":165,"IsInfra":false,"Labels":{"io.podcd.app":"vault-dev","io.podcd.managed":"true"}},
 {"Id":"05be03bb1c1bcccc","Names":["vault-dev-vault-seed"],"Image":"docker.io/curlimages/curl:8.8.0","State":"running","Status":"Up 22 seconds","ExitCode":0,"Restarts":0,"IsInfra":false,"Labels":{"io.podcd.app":"vault-dev","io.podcd.managed":"true"}},
 {"Id":"73b46a5f1513dddd","Names":["demo-app-demo"],"Image":"docker.io/library/busybox:1.36","State":"running","Status":"Up 16 minutes (healthy)","ExitCode":0,"Restarts":0,"IsInfra":false,"Labels":{"io.podcd.app":"demo-app","io.podcd.managed":"true"}},
 {"Id":"eeeeeeeeeeeeeeee","Names":["stray"],"Image":"x","State":"running","Status":"Up","ExitCode":0,"Restarts":0,"IsInfra":false,"Labels":{"io.podcd.managed":"true"}}
]`

// inspectJSON is `podman inspect --format json vault-dev-vault` for the
// container above: a liveness probe that keeps failing because the image has
// no curl, restarted by podman every failureThreshold.
const inspectJSON = `[{"Name":"vault-dev-vault","RestartCount":165,"State":{"Status":"running","Running":true,"ExitCode":0,"Error":"","OOMKilled":false,
 "Health":{"Status":"starting","FailingStreak":14,"Log":[
   {"Start":"2026-09-17T13:42:16","End":"2026-09-17T13:42:16","ExitCode":1,"Output":"/bin/sh: curl: not found\n"},
   {"Start":"2026-09-17T13:42:18","End":"2026-09-17T13:42:18","ExitCode":1,"Output":"/bin/sh: curl: not found\n"}]}}}]`

func TestListContainersReadsPodmanPsJSON(t *testing.T) {
	r, f := newRuntime(t, func(bin string, args []string) (string, error) {
		return psJSON, nil
	})
	got, err := r.listContainers(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !f.has("podman", "ps --all --filter label=io.podcd.managed=true") || f.has("podman", "label=io.podcd.app=") {
		t.Fatalf("an unscoped listing must filter on the managed label only: %+v", f.calls)
	}
	// A container without an app label is not anybody's.
	if _, ok := got[""]; ok || len(got) != 2 {
		t.Fatalf("want vault-dev and demo-app only, got %v", keys(got))
	}
	vault := got["vault-dev"]
	if len(vault) != 3 {
		t.Fatalf("vault-dev has three containers including infra, got %d", len(vault))
	}
	// Sorted by name, so the runtime is deterministic whatever podman's order.
	if vault[0].name != "947a5a828fed-infra" || vault[1].name != "vault-dev-vault" || vault[2].name != "vault-dev-vault-seed" {
		t.Fatalf("containers should be sorted by name: %v", names(vault))
	}
	v := vault[1]
	if v.id != "88f54f1c0dc1" || v.image != "docker.io/hashicorp/vault:1.17" || v.state != "running" ||
		v.health != "starting" || v.restarts != 165 || v.infra {
		t.Fatalf("vault container misread: %+v", v)
	}
	if !vault[0].infra {
		t.Fatal("the infra container must be flagged")
	}
	if got["demo-app"][0].health != "healthy" {
		t.Fatalf("(healthy) in the status column should be read: %+v", got["demo-app"][0])
	}
}

func TestListContainersScopesToOneAppAndTolerantOfEmptyOutput(t *testing.T) {
	for _, out := range []string{"", "null", "  \n"} {
		r, f := newRuntime(t, func(string, []string) (string, error) { return out, nil })
		got, err := r.listContainers(context.Background(), "demo-app")
		if err != nil || len(got) != 0 {
			t.Fatalf("output %q: want an empty map, got %v %v", out, got, err)
		}
		if !f.has("podman", "--filter label=io.podcd.app=demo-app") {
			t.Fatalf("a scoped listing must filter on the app: %+v", f.calls)
		}
	}
}

func TestListContainersReportsAPodmanFailure(t *testing.T) {
	r, _ := newRuntime(t, func(string, []string) (string, error) {
		return "", errors.New("cannot connect to podman")
	})
	if _, err := r.listContainers(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "listing containers") {
		t.Fatalf("a podman failure must surface, got %v", err)
	}
}

// --- Inspect -----------------------------------------------------------------

func writeUnit(t *testing.T, r *Runtime, app model.Application, manifestOK bool) renderer.Unit {
	t.Helper()
	u, err := r.rend.Render(app)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(u.ManifestPath), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := u.Manifest
	if !manifestOK {
		manifest = []byte("# edited by hand\n" + string(manifest))
	}
	if err := os.WriteFile(u.ManifestPath, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(u.Path, u.Content, 0o644); err != nil {
		t.Fatal(err)
	}
	return u
}

func app(name string) model.Application {
	a := model.Application{Name: name, Image: "example.com/" + name + "@sha256:aaaa", RestartPolicy: "always"}
	a.SetManifest([]byte("apiVersion: v1\nkind: Pod\nmetadata:\n  name: " + name + "\n"))
	return a
}

func TestInspectReadsUnitsContainersAndSystemdTogether(t *testing.T) {
	r, f := newRuntime(t, nil)
	writeUnit(t, r, app("vault-dev"), true)
	writeUnit(t, r, app("demo-app"), false) // manifest edited on disk
	// A unit with podcd's name but no header: somebody else's.
	if err := os.WriteFile(filepath.Join(r.unitDir, "podcd-foreign.kube"), []byte("[Kube]\nYaml=/x.yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Not ours by name at all.
	if err := os.WriteFile(filepath.Join(r.unitDir, "other.container"), []byte("[Container]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.respond = func(bin string, args []string) (string, error) {
		switch {
		case bin == "podman":
			return psJSON, nil
		case bin == "systemctl" && strings.Contains(strings.Join(args, " "), "show"):
			return "Id=podcd-vault-dev.service\nActiveState=active\nSubState=running\nLoadState=loaded\n\n" +
				"Id=podcd-demo-app.service\nActiveState=inactive\nSubState=dead\nLoadState=loaded\n\n" +
				"Id=podcd-foreign.service\nActiveState=failed\nSubState=failed\nLoadState=loaded\n", nil
		}
		return "", nil
	}

	st, err := r.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(st.Names(), ","); got != "demo-app,foreign,vault-dev" {
		t.Fatalf("want the two managed units and the foreign one, got %s", got)
	}

	vd := st.Apps["vault-dev"]
	if !vd.Managed || vd.UnitState != model.UnitActive || vd.SubState != "running" {
		t.Fatalf("vault-dev: %+v", vd)
	}
	if strings.HasPrefix(vd.UnitFileHash, "manifest-drift:") {
		t.Fatal("vault-dev's manifest matches its unit; no drift expected")
	}
	if vd.ContainerImage != "docker.io/hashicorp/vault:1.17" || vd.ContainerState != "running" {
		t.Fatalf("the first workload container stands for the app: %+v", vd)
	}
	if len(vd.Containers) != 2 || vd.Containers[0].Name != "vault-dev-vault" || vd.Containers[0].Restarts != 165 || vd.Containers[0].Health != "starting" {
		t.Fatalf("every workload container should be carried, infra excluded: %+v", vd.Containers)
	}

	da := st.Apps["demo-app"]
	if !strings.HasPrefix(da.UnitFileHash, "manifest-drift:") {
		t.Fatal("an edited manifest must mark the unit as drifted even though the unit file itself matches")
	}
	if da.UnitState != model.UnitInactive {
		t.Fatalf("demo-app should be inactive, got %s", da.UnitState)
	}

	if st.Apps["foreign"].Managed {
		t.Fatal("a unit without podcd's header must not be claimed")
	}
	if _, ok := st.Apps["other"]; ok {
		t.Fatal("a file without the podcd- prefix must be ignored entirely")
	}
}

func TestInspectSeesAContainerWithNoUnitAsManagedButMissing(t *testing.T) {
	r, f := newRuntime(t, nil)
	f.respond = func(bin string, args []string) (string, error) {
		if bin == "podman" {
			return psJSON, nil
		}
		// systemd knows neither unit.
		return "Id=podcd-vault-dev.service\nLoadState=not-found\nActiveState=inactive\n\nId=podcd-demo-app.service\nLoadState=not-found\n", nil
	}
	st, err := r.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"vault-dev", "demo-app"} {
		a := st.Apps[name]
		if !a.Managed || a.UnitState != model.UnitMissing {
			t.Fatalf("%s: a labelled container with no unit is ours to clean up: %+v", name, a)
		}
	}
}

func TestInspectRefusesTwoUnitFilesForOneApp(t *testing.T) {
	r, _ := newRuntime(t, nil)
	writeUnit(t, r, app("api"), true)
	// A second file whose header claims the same app.
	other := filepath.Join(r.unitDir, "podcd-api-copy.kube")
	content, _ := os.ReadFile(filepath.Join(r.unitDir, renderer.KubeFileName("api")))
	if err := os.WriteFile(other, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Inspect(context.Background()); err == nil || !strings.Contains(err.Error(), "two unit files") {
		t.Fatalf("two files claiming one service must be refused, not guessed at: %v", err)
	}
}

func TestParseShowBlocksAndUnitStateMapping(t *testing.T) {
	blocks := parseShowBlocks("Id=a.service\nActiveState=active\n\n\nId=b.service\nActiveState=activating\nSubState=auto-restart\n\nId=c.service\nActiveState=deactivating\n")
	if len(blocks) != 3 || blocks["b.service"]["SubState"] != "auto-restart" {
		t.Fatalf("got %v", blocks)
	}

	r, f := newRuntime(t, nil)
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		writeUnit(t, r, app(name), true)
	}
	f.respond = func(bin string, args []string) (string, error) {
		if bin == "systemctl" {
			return "Id=podcd-a.service\nActiveState=active\nLoadState=loaded\n\n" +
				"Id=podcd-b.service\nActiveState=activating\nLoadState=loaded\n\n" +
				"Id=podcd-c.service\nActiveState=failed\nLoadState=loaded\n\n" +
				"Id=podcd-d.service\nActiveState=deactivating\nLoadState=loaded\n", nil
			// e is absent from the answer entirely
		}
		return "[]", nil
	}
	st, err := r.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]model.UnitState{
		"a": model.UnitActive, "b": model.UnitActivating, "c": model.UnitFailed,
		"d": model.UnitState("deactivating"), "e": model.UnitMissing,
	}
	for name, w := range want {
		if got := st.Apps[name].UnitState; got != w {
			t.Errorf("%s: unit state %q, want %q", name, got, w)
		}
	}
}

// --- Health ------------------------------------------------------------------

func TestHealthOnTheHappyPathNeverInspects(t *testing.T) {
	r, f := newRuntime(t, func(bin string, args []string) (string, error) {
		return strings.Replace(psJSON, `"Status":"Up 22 seconds (starting)","ExitCode":0,"Restarts":165`, `"Status":"Up 22 seconds (healthy)","ExitCode":0,"Restarts":0`, 1), nil
	})
	h, err := r.Health(context.Background(), model.Application{Name: "vault-dev"})
	if err != nil || !h.OK() {
		t.Fatalf("got %+v %v", h, err)
	}
	if f.has("podman", "inspect") || f.count("journalctl") > 0 {
		t.Fatalf("a healthy app must cost exactly one podman ps: %+v", f.calls)
	}
}

func TestHealthExplainsABadVerdictWithOneInspect(t *testing.T) {
	r, f := newRuntime(t, func(bin string, args []string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "inspect") {
			return inspectJSON, nil
		}
		return psJSON, nil
	})
	h, err := r.Health(context.Background(), model.Application{Name: "vault-dev"})
	if err != nil {
		t.Fatal(err)
	}
	if h.Status != model.HealthUnknown {
		t.Fatalf("a probe still starting is unknown, got %s: %s", h.Status, h.Message)
	}
	for _, want := range []string{
		"healthcheck not passed yet: vault-dev-vault",
		"vault-dev-vault restarted 165 times",
		"14 consecutive check failures",
		"last check: /bin/sh: curl: not found",
	} {
		if !strings.Contains(h.Message, want) {
			t.Errorf("message should say %q:\n%s", want, h.Message)
		}
	}
	if !f.has("podman", "inspect --format json vault-dev-vault") || strings.Contains(callsOf(f, "podman", "inspect"), "vault-dev-vault-seed") {
		t.Fatalf("inspect only the troubled container, in one call: %+v", f.calls)
	}
}

func TestHealthReadsTheJournalWhenThePodIsGone(t *testing.T) {
	r, f := newRuntime(t, func(bin string, args []string) (string, error) {
		switch bin {
		case "podman":
			return "[]", nil
		case "journalctl":
			return "conmon 53a9f5274007 <nwarn>: Failed to open cgroups file\nfatal: config missing\n", nil
		}
		return "", nil
	})
	h, err := r.Health(context.Background(), model.Application{Name: "broken-exit"})
	if err != nil {
		t.Fatal(err)
	}
	if h.Status != model.HealthUnhealthy || !strings.Contains(h.Message, "no containers") {
		t.Fatalf("got %+v", h)
	}
	if !strings.Contains(h.Message, "last output: fatal: config missing") {
		t.Fatalf("the container's last words should be quoted: %s", h.Message)
	}
	if strings.Contains(h.Message, "conmon") {
		t.Fatalf("conmon's own warnings are noise: %s", h.Message)
	}
	if !f.has("journalctl", "-u podcd-broken-exit.service _COMM=conmon") {
		t.Fatalf("container output is what came through conmon: %+v", f.calls)
	}
}

func TestTroubledNamesOnlyWhatABadVerdictIsAbout(t *testing.T) {
	cs := []containerInfo{
		{name: "app-web", state: "running", health: "healthy"},
		{name: "app-db", state: "running", health: "unhealthy"},
		{name: "app-cache", state: "exited"},
		{name: "app-init", state: "exited", exitCode: 0},
		{name: "app-init2", state: "exited", exitCode: 7},
	}
	got := troubled(cs, []string{"init", "init2"})
	if strings.Join(got, ",") != "app-db,app-cache,app-init2" {
		t.Fatalf("got %v", got)
	}
}

func TestLastLineTakesTheFinalNonEmptyLine(t *testing.T) {
	if got := lastLine("Connecting to 127.0.0.1\nremote file exists\n\n"); got != "remote file exists" {
		t.Fatalf("got %q", got)
	}
	if got := lastLine("  single  "); got != "single" {
		t.Fatalf("got %q", got)
	}
}

// --- Apply / Restart / Remove ------------------------------------------------

func TestApplyWritesFilesThenReloadsThenRestarts(t *testing.T) {
	r, f := newRuntime(t, nil)
	a := app("api")
	if err := r.Apply(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	u, _ := r.rend.Render(a)
	for path, perm := range map[string]os.FileMode{u.ManifestPath: 0o600, u.Path: 0o644} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s not written: %v", path, err)
		}
		if info.Mode().Perm() != perm {
			t.Errorf("%s has mode %v, want %v", path, info.Mode().Perm(), perm)
		}
	}
	if len(f.calls) != 2 || f.calls[0].args != "--user daemon-reload" || f.calls[1].args != "--user restart podcd-api.service" {
		t.Fatalf("want daemon-reload then restart, got %+v", f.calls)
	}
}

func TestApplyReportsAFailedStartWithTheJournal(t *testing.T) {
	r, _ := newRuntime(t, func(bin string, args []string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "restart"):
			return "", errors.New("Job for podcd-api.service failed")
		case bin == "journalctl":
			return "Failed to start podcd-api.service - podcd pod api.\n", nil
		}
		return "", nil
	})
	err := r.Apply(context.Background(), app("api"))
	if err == nil || !strings.Contains(err.Error(), "starting podcd-api.service") || !strings.Contains(err.Error(), "Failed to start") {
		t.Fatalf("a failed start should carry the journal tail, got: %v", err)
	}
}

func TestRestartAsksSystemd(t *testing.T) {
	r, f := newRuntime(t, nil)
	if err := r.Restart(context.Background(), "api"); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0].args != "--user restart podcd-api.service" {
		t.Fatalf("got %+v", f.calls)
	}
}

func TestRemoveStopsCleansAndTakesThePodDown(t *testing.T) {
	r, f := newRuntime(t, nil)
	u := writeUnit(t, r, app("api"), true)
	if err := r.Remove(context.Background(), "api"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{u.Path, u.ManifestPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s should be gone", p)
		}
	}
	want := []string{
		"systemctl --user stop podcd-api.service",
		"systemctl --user daemon-reload",
		"podman pod rm --force --time 10 api",
	}
	var got []string
	for _, c := range f.calls {
		got = append(got, c.bin+" "+c.args)
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("want %v\n got %v", want, got)
	}
}

func TestRemoveToleratesAUnitSystemdDoesNotKnow(t *testing.T) {
	r, _ := newRuntime(t, func(bin string, args []string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "stop") {
			return "", errors.New("Unit podcd-api.service not loaded.")
		}
		return "", nil
	})
	if err := r.Remove(context.Background(), "api"); err != nil {
		t.Fatalf("a unit that is already gone is already stopped: %v", err)
	}
	r2, _ := newRuntime(t, func(bin string, args []string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "stop") {
			return "", errors.New("permission denied")
		}
		return "", nil
	})
	if err := r2.Remove(context.Background(), "api"); err == nil {
		t.Fatal("any other stop failure must be reported")
	}
}

// --- WaitHealthy -------------------------------------------------------------

func TestWaitHealthyReturnsAsSoonAsItIsAndRetriesUntilThen(t *testing.T) {
	waitInterval, waitRetries = time.Millisecond, 5
	t.Cleanup(func() { waitInterval, waitRetries = 2*time.Second, 15 })

	attempts := 0
	r, _ := newRuntime(t, func(bin string, args []string) (string, error) {
		attempts++
		if attempts < 3 {
			return `[{"Names":["api-web"],"State":"running","Status":"Up 1 second (starting)","Labels":{"io.podcd.app":"api"}}]`, nil
		}
		return `[{"Names":["api-web"],"State":"running","Status":"Up 3 seconds (healthy)","Labels":{"io.podcd.app":"api"}}]`, nil
	})
	h := r.WaitHealthy(context.Background(), model.Application{Name: "api"})
	if !h.OK() || attempts != 3 {
		t.Fatalf("should succeed on the third look: ok=%v attempts=%d %s", h.OK(), attempts, h.Message)
	}
}

func TestWaitHealthyGivesUpAndReportsTheLastVerdict(t *testing.T) {
	waitInterval, waitRetries = time.Millisecond, 3
	t.Cleanup(func() { waitInterval, waitRetries = 2*time.Second, 15 })

	r, _ := newRuntime(t, func(bin string, args []string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "inspect") {
			return "[]", nil
		}
		return `[{"Names":["api-web"],"State":"exited","Status":"Exited (3) 1 second ago","Labels":{"io.podcd.app":"api"}}]`, nil
	})
	h := r.WaitHealthy(context.Background(), model.Application{Name: "api"})
	if h.OK() || !strings.Contains(h.Message, "api-web is exited") {
		t.Fatalf("got %+v", h)
	}
}

func TestWaitHealthyStopsWhenTheContextDoes(t *testing.T) {
	waitInterval, waitRetries = time.Hour, 15
	t.Cleanup(func() { waitInterval, waitRetries = 2*time.Second, 15 })

	r, _ := newRuntime(t, func(bin string, args []string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "inspect") {
			return "[]", nil
		}
		return `[{"Names":["api-web"],"State":"created","Labels":{"io.podcd.app":"api"}}]`, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	h := r.WaitHealthy(ctx, model.Application{Name: "api"})
	if h.Status != model.HealthUnknown || !strings.Contains(h.Message, "gave up waiting") {
		t.Fatalf("a cancelled wait is unknown, not a verdict: %+v", h)
	}
}

// --- small helpers -----------------------------------------------------------

func TestYamlPathOfReadsTheKubeUnit(t *testing.T) {
	if got := yamlPathOf([]byte("# header\n[Unit]\nDescription=x\n\n[Kube]\nYaml=/home/u/.local/state/podcd/kube/api.yaml\n")); got != "/home/u/.local/state/podcd/kube/api.yaml" {
		t.Fatalf("got %q", got)
	}
	if got := yamlPathOf([]byte("[Container]\nImage=x\n")); got != "" {
		t.Fatalf("no Yaml= means no manifest, got %q", got)
	}
}

func TestAvailableNamesWhatIsMissing(t *testing.T) {
	r := New(Options{PodmanBin: "/nonexistent/podman"})
	ok, why := r.Available(context.Background())
	if ok || !strings.Contains(why, "podman is not installed") {
		t.Fatalf("got ok=%v why=%q", ok, why)
	}
}

func TestLogsTailsTheUnit(t *testing.T) {
	r, f := newRuntime(t, func(string, []string) (string, error) { return "line\n", nil })
	if out, err := r.Logs(context.Background(), "api", 0); err != nil || out != "line\n" {
		t.Fatalf("got %q %v", out, err)
	}
	if !f.has("journalctl", "--user -u podcd-api.service -n 50") {
		t.Fatalf("zero lines means the default of 50: %+v", f.calls)
	}
}

func keys(m map[string][]containerInfo) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func names(cs []containerInfo) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.name)
	}
	return out
}

func callsOf(f *fake, bin, argSub string) string {
	var out []string
	for _, c := range f.calls {
		if c.bin == bin && strings.Contains(c.args, argSub) {
			out = append(out, c.args)
		}
	}
	return strings.Join(out, "\n")
}
