package docker

// These tests drive the runtime with recorded docker output instead of a
// daemon, through the exec seam, the same way the podman tests do.

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
)

type call struct {
	bin  string
	args string
}

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

func (f *fake) has(argSub string) bool {
	for _, c := range f.calls {
		if strings.Contains(c.args, argSub) {
			return true
		}
	}
	return false
}

func newRuntime(t *testing.T, respond func(bin string, args []string) (string, error)) (*Runtime, *fake) {
	t.Helper()
	f := &fake{respond: respond}
	r := New(Options{UnitDir: filepath.Join(t.TempDir(), "docker"), KubeDir: filepath.Join(t.TempDir(), "kube"), PauseImage: "pause:1"})
	if err := os.MkdirAll(r.unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	r.exec = f.run
	return r, f
}

// psJSON is `docker ps --all --no-trunc --filter label=io.podcd.managed=true
// --format {{json .}}`: one object per line, labels as one string, no exit
// code or restart count of its own.
const psJSON = `{"ID":"daaef2fa2c13aaaa","Names":"web-infra","Image":"pause:1","State":"running","Status":"Up 22 seconds","Labels":"io.podcd.app=web,io.podcd.infra=true,io.podcd.managed=true"}
{"ID":"88f54f1c0dc1bbbb","Names":"web-nginx","Image":"docker.io/library/nginx:alpine","State":"running","Status":"Up 22 seconds (health: starting)","Labels":"io.podcd.app=web,io.podcd.managed=true"}
{"ID":"05be03bb1c1bcccc","Names":"web-seed","Image":"docker.io/library/busybox:1.36","State":"exited","Status":"Exited (0) 25 seconds ago","Labels":"io.podcd.app=web,io.podcd.managed=true"}
{"ID":"73b46a5f1513dddd","Names":"job-run","Image":"docker.io/library/busybox:1.36","State":"exited","Status":"Exited (1) 16 minutes ago","Labels":"io.podcd.app=job,io.podcd.managed=true"}
{"ID":"eeeeeeeeeeeeeeee","Names":"stray","Image":"x","State":"running","Status":"Up","Labels":"io.podcd.managed=true"}
`

func TestListContainersReadsDockerPsLines(t *testing.T) {
	r, f := newRuntime(t, func(string, []string) (string, error) { return psJSON, nil })
	got, err := r.listContainers(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !f.has("ps --all --no-trunc --filter label=io.podcd.managed=true") || f.has("label=io.podcd.app=") {
		t.Fatalf("an unscoped listing filters on the managed label only: %+v", f.calls)
	}
	if _, ok := got[""]; ok || len(got) != 2 {
		t.Fatalf("want web and job only, got %v", got)
	}
	web := got["web"]
	if len(web) != 3 || web[0].name != "web-infra" || web[1].name != "web-nginx" || web[2].name != "web-seed" {
		t.Fatalf("containers sorted by name: %+v", web)
	}
	if !web[0].infra || web[1].infra {
		t.Fatalf("infra is flagged by label: %+v", web)
	}
	if web[1].id != "88f54f1c0dc1" || web[1].health != "starting" || web[1].state != "running" {
		t.Fatalf("nginx misread: %+v", web[1])
	}
	if web[2].exitCode != 0 || got["job"][0].exitCode != 1 {
		t.Fatalf("exit codes come from the status column: %+v %+v", web[2], got["job"][0])
	}
}

func TestInspectReadsRecordsAndContainersTogether(t *testing.T) {
	r, f := newRuntime(t, nil)
	writeRecord(t, r, app("web"), true)
	writeRecord(t, r, app("db"), false) // manifest edited on disk, no containers
	if err := os.WriteFile(filepath.Join(r.unitDir, "podcd-foreign.kube"), []byte("[Kube]\nYaml=/x.yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.respond = func(_ string, args []string) (string, error) {
		switch args[0] {
		case "ps":
			return psJSON, nil
		case "network":
			return `{"Name":"bridge","Labels":""}` + "\n" + `{"Name":"backend","Labels":"io.podcd.managed=true,io.podcd.network=backend"}` + "\n", nil
		}
		return "", nil
	}
	st, err := r.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(st.Names(), ","); got != "db,foreign,job,web" {
		t.Fatalf("apps: %s", got)
	}
	web := st.Apps["web"]
	if !web.Managed || web.UnitState != model.UnitActive || web.ContainerImage != "docker.io/library/nginx:alpine" || len(web.Containers) != 2 {
		t.Fatalf("web: %+v", web)
	}
	if strings.HasPrefix(web.UnitFileHash, "manifest-drift:") {
		t.Fatal("web's manifest matches its record")
	}
	db := st.Apps["db"]
	if db.UnitState != model.UnitInactive || !strings.HasPrefix(db.UnitFileHash, "manifest-drift:") {
		t.Fatalf("db has a record, no containers, and an edited manifest: %+v", db)
	}
	if st.Apps["foreign"].Managed {
		t.Fatal("a record without podcd's header is not ours")
	}
	job := st.Apps["job"]
	if !job.Managed || job.UnitFile != "" || job.UnitState != model.UnitInactive || job.SubState != "exited" {
		t.Fatalf("job has containers but no record: %+v", job)
	}
	if n := st.Networks["backend"]; !n.Managed || !n.Exists || n.UnitFile != "" {
		t.Fatalf("a labelled network without a record is still ours: %+v", n)
	}
	if _, ok := st.Networks["bridge"]; ok {
		t.Fatal("docker's own network is not ours")
	}
}

func TestApplyWritesTheProjectAndBringsItUp(t *testing.T) {
	r, f := newRuntime(t, nil)
	a := app("web")
	if err := r.Apply(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(r.unitDir, "podcd-web.kube")
	if _, err := os.Stat(unit); err != nil {
		t.Fatalf("the record is written: %v", err)
	}
	compose := r.composePath("web")
	info, err := os.Stat(compose)
	if err != nil {
		t.Fatalf("the compose file is written: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("the compose file can hold secrets: mode %o", info.Mode().Perm())
	}
	data, _ := os.ReadFile(compose)
	if !strings.Contains(string(data), "container_name: web-nginx") {
		t.Fatalf("unexpected compose file:\n%s", data)
	}
	if len(f.calls) != 1 || f.calls[0].args != "compose --project-name podcd-web --file "+compose+" up --detach --force-recreate --remove-orphans --quiet-pull" {
		t.Fatalf("calls: %+v", f.calls)
	}
}

func TestApplyFailureCarriesTheLogs(t *testing.T) {
	r, _ := newRuntime(t, func(_ string, args []string) (string, error) {
		if args[len(args)-1] == "15" {
			return "web-seed  | boom\n", nil
		}
		return "", errors.New("dependency failed to start")
	})
	err := r.Apply(context.Background(), app("web"))
	if err == nil || !strings.Contains(err.Error(), "dependency failed") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want the compose error and the log tail, got %v", err)
	}
}

func TestRestartReplaysFromTheManifestOnDisk(t *testing.T) {
	r, f := newRuntime(t, nil)
	writeRecord(t, r, app("web"), true)
	if err := r.Restart(context.Background(), "web"); err != nil {
		t.Fatal(err)
	}
	if !f.has("up --detach --force-recreate") {
		t.Fatalf("restart replays the project: %+v", f.calls)
	}
	if _, err := os.Stat(r.composePath("web")); err != nil {
		t.Fatal("restart rebuilds the compose file")
	}
	if err := r.Restart(context.Background(), "nothing"); err == nil {
		t.Fatal("an application with no manifest cannot be restarted")
	}
}

func TestRemoveTakesDownAndForgets(t *testing.T) {
	r, f := newRuntime(t, func(_ string, args []string) (string, error) {
		if args[0] == "ps" {
			return `{"ID":"abc","Names":"web-nginx","State":"exited","Status":"Exited (0) 1s ago","Labels":"io.podcd.app=web,io.podcd.managed=true"}` + "\n", nil
		}
		return "", nil
	})
	writeRecord(t, r, app("web"), true)
	if err := r.Apply(context.Background(), app("web")); err != nil {
		t.Fatal(err)
	}
	if err := r.Remove(context.Background(), "web"); err != nil {
		t.Fatal(err)
	}
	if !f.has("down --remove-orphans") || f.has("--volumes") {
		t.Fatalf("down without volumes: %+v", f.calls)
	}
	if !f.has("rm --force abc") {
		t.Fatalf("leftovers are removed by label: %+v", f.calls)
	}
	for _, p := range []string{filepath.Join(r.unitDir, "podcd-web.kube"), r.rend.ManifestPath("web"), r.projectDir("web")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s should be gone", p)
		}
	}
}

func TestHealthVerdicts(t *testing.T) {
	now := time.Now()
	c := func(name, state string, exit int, health string) containerInfo {
		return containerInfo{name: name, state: state, exitCode: exit, health: health}
	}
	for _, tc := range []struct {
		name       string
		containers []containerInfo
		init       []string
		status     model.HealthStatus
		message    string
	}{
		{"nothing", nil, nil, model.HealthUnhealthy, "no containers"},
		{"running", []containerInfo{c("web-nginx", "running", 0, "")}, nil, model.HealthHealthy, "all workload containers running"},
		{"init done", []containerInfo{c("web-seed", "exited", 0, ""), c("web-nginx", "running", 0, "")}, []string{"seed"}, model.HealthHealthy, "all workload"},
		{"init failed", []containerInfo{c("web-seed", "exited", 1, ""), c("web-nginx", "created", 0, "")}, []string{"seed"}, model.HealthUnhealthy, "init container failed: web-seed exited with code 1"},
		{"init running", []containerInfo{c("web-seed", "running", 0, ""), c("web-nginx", "created", 0, "")}, []string{"seed"}, model.HealthUnknown, "still running: web-seed"},
		{"exited", []containerInfo{c("web-nginx", "exited", 1, "")}, nil, model.HealthUnhealthy, "web-nginx is exited"},
		{"unhealthy", []containerInfo{c("web-nginx", "running", 0, "unhealthy")}, nil, model.HealthUnhealthy, "healthcheck failing: web-nginx"},
		{"starting", []containerInfo{c("web-nginx", "running", 0, "starting")}, nil, model.HealthUnknown, "not passed yet: web-nginx"},
	} {
		h := healthForContainers("web", tc.containers, tc.init, now)
		if h.Status != tc.status || !strings.Contains(h.Message, tc.message) {
			t.Errorf("%s: want %s %q, got %s %q", tc.name, tc.status, tc.message, h.Status, h.Message)
		}
	}
}

func TestHealthExplainsWithInspect(t *testing.T) {
	r, f := newRuntime(t, func(_ string, args []string) (string, error) {
		switch args[0] {
		case "ps":
			return `{"ID":"abc","Names":"web-nginx","State":"running","Status":"Up 3s (unhealthy)","Labels":"io.podcd.app=web,io.podcd.managed=true"}` + "\n", nil
		case "inspect":
			return `{"Name":"/web-nginx","RestartCount":4,"State":{"Error":"","OOMKilled":false,"Health":{"FailingStreak":3,"Log":[{"ExitCode":1,"Output":"curl: (7) Failed to connect\n"}]}}}` + "\n", nil
		}
		return "", nil
	})
	h, err := r.Health(context.Background(), app("web"))
	if err != nil {
		t.Fatal(err)
	}
	if h.Status != model.HealthUnhealthy || !strings.Contains(h.Message, "web-nginx: restarted 4 times, 3 consecutive check failures, last check: curl: (7) Failed to connect") {
		t.Fatalf("verdict: %+v", h)
	}
	if !f.has("inspect --format {{json .}} web-nginx") {
		t.Fatalf("calls: %+v", f.calls)
	}
}

func TestApplyNetworkCreatesAdoptsAndRecreates(t *testing.T) {
	var have bool
	r, f := newRuntime(t, func(_ string, args []string) (string, error) {
		switch {
		case args[0] == "network" && args[1] == "ls":
			if have {
				return `{"Name":"backend","Labels":"io.podcd.managed=true,io.podcd.network=backend"}` + "\n", nil
			}
			return "", nil
		case args[0] == "ps":
			return "c1\nc2\n", nil
		}
		return "", nil
	})
	net := model.Network{Name: "backend", Subnet: "10.9.0.0/24", Options: map[string]string{"mtu": "1400"}}

	// No record, no network: created.
	if err := r.ApplyNetwork(context.Background(), net); err != nil {
		t.Fatal(err)
	}
	if !f.has("network create --label io.podcd.managed=true --label io.podcd.network=backend --subnet 10.9.0.0/24 --opt mtu=1400 backend") {
		t.Fatalf("create: %+v", f.calls)
	}

	// Record present, network present, definition changed: recreated after
	// stopping what is on it.
	have = true
	f.calls = nil
	net.Subnet = "10.10.0.0/24"
	if err := r.ApplyNetwork(context.Background(), net); err != nil {
		t.Fatal(err)
	}
	args := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		args = append(args, c.args)
	}
	joined := strings.Join(args, "\n")
	if !strings.Contains(joined, "ps --quiet --filter network=backend") || !strings.Contains(joined, "stop c1 c2") ||
		strings.Index(joined, "network rm backend") > strings.Index(joined, "network create") {
		t.Fatalf("recreate order: %s", joined)
	}

	// No record but the network exists: adopted, not touched.
	r2, f2 := newRuntime(t, func(_ string, args []string) (string, error) {
		if args[0] == "network" && args[1] == "ls" {
			return `{"Name":"backend","Labels":""}` + "\n", nil
		}
		return "", nil
	})
	if err := r2.ApplyNetwork(context.Background(), net); err != nil {
		t.Fatal(err)
	}
	if f2.has("network create") || f2.has("network rm") {
		t.Fatalf("an existing network is adopted: %+v", f2.calls)
	}

	if err := r.ApplyNetwork(context.Background(), model.Network{Name: "x", DisableDNS: true}); err == nil {
		t.Fatal("podman-only DNS options are refused")
	}
}

func TestRemoveNetworkTolerantOfMissing(t *testing.T) {
	r, _ := newRuntime(t, func(string, []string) (string, error) {
		return "", errors.New("Error response from daemon: No such network: backend")
	})
	writeNetworkRecord(t, r, "backend")
	if err := r.RemoveNetwork(context.Background(), "backend"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.unitDir, "podcd-backend.network")); !os.IsNotExist(err) {
		t.Fatal("the record should be gone")
	}
	r2, _ := newRuntime(t, func(string, []string) (string, error) {
		return "", errors.New("network backend has active endpoints")
	})
	if err := r2.RemoveNetwork(context.Background(), "backend"); err == nil {
		t.Fatal("a network in use is not forced")
	}
}

func TestLogsUsesCompose(t *testing.T) {
	r, f := newRuntime(t, func(string, []string) (string, error) { return "web-nginx | hi\n", nil })
	out, err := r.Logs(context.Background(), "web", 0)
	if err != nil || out != "web-nginx | hi\n" {
		t.Fatalf("%q %v", out, err)
	}
	if !f.has("compose --project-name podcd-web logs --no-color --timestamps --tail 50") {
		t.Fatalf("calls: %+v", f.calls)
	}
}

// --- helpers -----------------------------------------------------------------

func app(name string) model.Application {
	a := model.Application{Name: name, Image: "docker.io/library/nginx:alpine", RestartPolicy: "always"}
	a.SetManifest([]byte("apiVersion: v1\nkind: Pod\nmetadata:\n  name: " + name + "\n  labels:\n    io.podcd.app: " + name +
		"\n    io.podcd.managed: \"true\"\nspec:\n  containers:\n  - name: nginx\n    image: docker.io/library/nginx:alpine\n"))
	return a
}

func writeRecord(t *testing.T, r *Runtime, a model.Application, manifestOK bool) {
	t.Helper()
	u, err := r.rend.Render(a)
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
}

func writeNetworkRecord(t *testing.T, r *Runtime, name string) {
	t.Helper()
	u, err := r.rend.RenderNetwork(model.Network{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(u.Path, u.Content, 0o644); err != nil {
		t.Fatal(err)
	}
}
