package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateCompilesTheHostFromGit(t *testing.T) {
	newFakeHost(t, hostWith("api", "web"))
	out, code := run(t, "validate")
	if code != 0 {
		t.Fatal(out)
	}
	for _, want := range []string{"host vm-1", "api", "web", "8080→80", "2 application(s); configuration is valid"} {
		if !strings.Contains(out, want) {
			t.Errorf("validate output is missing %q:\n%s", want, out)
		}
	}
	out, code = run(t, "validate", "-o", "json")
	if code != 0 {
		t.Fatal(out)
	}
	var desired struct {
		Host         string `json:"host"`
		Applications []struct {
			Name string `json:"name"`
		} `json:"applications"`
	}
	if err := json.Unmarshal([]byte(out), &desired); err != nil || desired.Host != "vm-1" || len(desired.Applications) != 2 {
		t.Fatalf("json output should be the desired state: %v\n%s", err, out)
	}
}

func TestValidateRejectsABrokenRepository(t *testing.T) {
	newFakeHost(t, "apiVersion: v1\nkind: Pod\nmetadata: {name: api}\nspec: {containers: [{name: a, image: busybox}]}\n---\napiVersion: gitops.podcd.io/v1\nkind: Host\nmetadata: {name: vm-1}\nspec: {applications: [nope]}\n")
	out, code := run(t, "validate")
	if code == 0 || !strings.Contains(out, "nope") {
		t.Fatalf("a host naming an unknown application must fail validation with the name: %d %s", code, out)
	}
}

func TestPlanOnAFreshHostCreatesEverything(t *testing.T) {
	h := newFakeHost(t, hostWith("api", "web"))
	h.reply("podman.ps", "[]")
	out, code := run(t, "plan")
	if code != 0 {
		t.Fatal(out)
	}
	for _, want := range []string{"plan for vm-1", "+ create api", "+ create web", "2 change(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan is missing %q:\n%s", want, out)
		}
	}
	if entries, _ := os.ReadDir(h.unitDir()); len(entries) != 0 {
		t.Fatal("plan must not write anything")
	}
	if strings.Contains(h.calls(), "restart") || strings.Contains(h.calls(), "daemon-reload") {
		t.Fatalf("plan must not touch systemd:\n%s", h.calls())
	}
}

func TestReconcileAppliesThenReportsHealthThenIsIdle(t *testing.T) {
	h := newFakeHost(t, hostWith("api"))
	h.reply("podman.ps", "[]")
	h.then("podman.ps", psRunning("api"))
	h.reply("systemctl.show", "Id=podcd-api.service\nActiveState=active\nSubState=running\nLoadState=loaded\n")

	out, code := run(t, "reconcile")
	if code != 0 {
		t.Fatal(out)
	}
	if !strings.Contains(out, "applied 1 change(s)") || !strings.Contains(out, "+ create api") {
		t.Fatalf("got:\n%s", out)
	}
	if !strings.Contains(out, "api  healthy") {
		t.Fatalf("health should be reported after applying:\n%s", out)
	}
	unit := filepath.Join(h.unitDir(), "podcd-api.kube")
	if _, err := os.Stat(unit); err != nil {
		t.Fatalf("the Quadlet unit should exist: %v", err)
	}
	manifest := filepath.Join(h.home, ".local", "state", "podcd", "kube", "api.yaml")
	if info, err := os.Stat(manifest); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the manifest should be written 0600 under the state dir: %v %v", info, err)
	}
	calls := h.calls()
	if i, j := strings.Index(calls, "daemon-reload"), strings.Index(calls, "restart podcd-api.service"); i < 0 || j < 0 || i > j {
		t.Fatalf("daemon-reload must precede the restart:\n%s", calls)
	}

	// Second run: nothing changed in Git, unit on disk matches, container healthy.
	h.resetCalls()
	out, code = run(t, "reconcile")
	if code != 0 || !strings.Contains(out, "nothing to do (1 application(s)") {
		t.Fatalf("a converged host should be idle: %d\n%s", code, out)
	}
	if strings.Contains(h.calls(), "restart") {
		t.Fatalf("an idle reconcile must not restart anything:\n%s", h.calls())
	}

	// status now knows about it, offline.
	out, code = run(t, "status")
	if code != 0 {
		t.Fatal(out)
	}
	for _, want := range []string{"host     vm-1", "(agent config host field)", "APP  UNIT    CONTAINERS  RESTARTS", "api  active  1/1         0", "healthy", "success  20"} {
		if !strings.Contains(out, want) {
			t.Errorf("status is missing %q:\n%s", want, out)
		}
	}
	out, code = run(t, "status", "-o", "json")
	if code != 0 {
		t.Fatal(out)
	}
	var st struct {
		State struct {
			Applications map[string]struct {
				Health string `json:"health"`
			} `json:"applications"`
		} `json:"state"`
		Actual struct {
			Apps map[string]struct {
				UnitState string `json:"unitState"`
			} `json:"apps"`
		} `json:"actual"`
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if st.State.Applications["api"].Health != "healthy" || st.Actual.Apps["api"].UnitState != "active" {
		t.Fatalf("got %s", out)
	}
}

func TestReconcileDryRunChangesNothing(t *testing.T) {
	h := newFakeHost(t, hostWith("api"))
	h.reply("podman.ps", "[]")
	out, code := run(t, "reconcile", "--dry-run")
	if code != 0 || !strings.Contains(out, "+ create api") {
		t.Fatalf("%d %s", code, out)
	}
	if entries, _ := os.ReadDir(h.unitDir()); len(entries) != 0 {
		t.Fatal("--dry-run wrote a unit")
	}
}

func TestReconcileFailsWhenSystemdCannotStartTheUnit(t *testing.T) {
	h := newFakeHost(t, hostWith("api"))
	h.reply("podman.ps", "[]")
	h.fail("systemctl.restart", "Job for podcd-api.service failed because the control process exited with error code.\n")
	h.reply("journalctl", "podcd-api.service: Main process exited, code=exited, status=125/n/a\n")

	out, code := run(t, "reconcile")
	if code == 0 {
		t.Fatalf("a unit that will not start must fail the reconcile:\n%s", out)
	}
	if !strings.Contains(out, "starting podcd-api.service") || !strings.Contains(out, "status=125") {
		t.Fatalf("the failure should say which unit and quote the journal:\n%s", out)
	}
	out, _ = run(t, "status")
	if !strings.Contains(out, "failure") || !strings.Contains(out, "1 consecutive failure(s)") {
		t.Fatalf("status should record the failed attempt:\n%s", out)
	}
}

func TestReconcileRemovesWhatGitDroppedUnlessNoPrune(t *testing.T) {
	h := newFakeHost(t, hostWith("api", "web"))
	h.reply("podman.ps", psRunning("api", "web"))
	h.reply("systemctl.show", "Id=podcd-api.service\nActiveState=active\nLoadState=loaded\n\nId=podcd-web.service\nActiveState=active\nLoadState=loaded\n")
	if out, code := run(t, "reconcile"); code != 0 {
		t.Fatal(out)
	}

	h.commit(hostWith("api")) // web leaves Git
	h.resetCalls()
	out, code := run(t, "reconcile", "--no-prune")
	if code != 0 || strings.Contains(out, "web") {
		t.Fatalf("--no-prune must keep web: %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(h.unitDir(), "podcd-web.kube")); err != nil {
		t.Fatal("--no-prune removed web's unit")
	}

	out, code = run(t, "reconcile")
	if code != 0 || !strings.Contains(out, "- delete web - no longer declared in Git") {
		t.Fatalf("web is no longer in Git and must be removed: %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(h.unitDir(), "podcd-web.kube")); !os.IsNotExist(err) {
		t.Fatal("web's unit is still on disk")
	}
	if !strings.Contains(h.calls(), "systemctl --user stop podcd-web.service") || !strings.Contains(h.calls(), "podman pod rm --force --time 10 web") {
		t.Fatalf("removing web should stop its unit and take the pod down:\n%s", h.calls())
	}
}

func TestReconcileRestartsAContainerThatDied(t *testing.T) {
	h := newFakeHost(t, hostWith("api"))
	h.reply("podman.ps", psRunning("api"))
	h.reply("systemctl.show", "Id=podcd-api.service\nActiveState=active\nLoadState=loaded\n")
	if out, code := run(t, "reconcile"); code != 0 {
		t.Fatal(out)
	}

	// Someone killed the container; the unit is still active.
	h.reply("podman.ps", `[{"Names":["api-app"],"State":"exited","Status":"Exited (137) 1 minute ago","ExitCode":137,"Labels":{"io.podcd.app":"api","io.podcd.managed":"true"}}]`)
	h.reply("podman.inspect", `[{"Name":"api-app","State":{"Status":"exited","ExitCode":137,"OOMKilled":false,"Health":{}}}]`)
	out, code := run(t, "plan")
	if code != 0 || !strings.Contains(out, "restart api") || !strings.Contains(out, "api-app is exited") {
		t.Fatalf("plan should want a restart: %d\n%s", code, out)
	}
	h.then("podman.ps", psRunning("api")) // back once restarted
	h.resetCalls()
	out, code = run(t, "reconcile")
	if code != 0 || !strings.Contains(out, "restart api") || !strings.Contains(out, "api  healthy") {
		t.Fatalf("%d\n%s", code, out)
	}
	if !strings.Contains(h.calls(), "systemctl --user restart podcd-api.service") {
		t.Fatalf("the unit should have been restarted:\n%s", h.calls())
	}
}

func TestHealthListsEveryApplicationAndFailsWhenOneIsBad(t *testing.T) {
	h := newFakeHost(t, hostWith("api", "web"))
	h.reply("podman.ps", psRunning("api"))
	out, code := run(t, "health")
	// web has no containers at all: unhealthy.
	if code == 0 || !strings.Contains(out, "api  healthy") || !strings.Contains(out, "web  unhealthy") || !strings.Contains(out, "1 application(s) are not healthy") {
		t.Fatalf("%d\n%s", code, out)
	}

	out, code = run(t, "health", "-o", "json")
	if code != 0 {
		t.Fatalf("json output reports; it does not judge: %d\n%s", code, out)
	}
	var results []struct {
		App    string `json:"app"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &results); err != nil || len(results) != 2 {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestLogsDefaultToTheAgentAndTakeAnApplication(t *testing.T) {
	h := newFakeHost(t, hostWith("api"))
	h.reply("journalctl", "some line\n")

	out, code := run(t, "logs")
	if code != 0 || out != "some line\n" {
		t.Fatalf("%d %q", code, out)
	}
	if !strings.Contains(h.calls(), "journalctl --user -u podcd-agent.service -n 50") {
		t.Fatalf("bare logs is the agent's unit:\n%s", h.calls())
	}
	h.resetCalls()
	if _, code := run(t, "logs", "api", "--tail", "7"); code != 0 {
		t.Fatal(code)
	}
	if !strings.Contains(h.calls(), "journalctl --user -u podcd-api.service -n 7") {
		t.Fatalf("logs api should read the application's unit with the given tail:\n%s", h.calls())
	}
}

func TestRemoveByNameAndPruneAgainstGit(t *testing.T) {
	h := newFakeHost(t, hostWith("api", "web"))
	h.reply("podman.ps", psRunning("api", "web"))
	h.reply("systemctl.show", "Id=podcd-api.service\nActiveState=active\nLoadState=loaded\n\nId=podcd-web.service\nActiveState=active\nLoadState=loaded\n")
	if out, code := run(t, "reconcile"); code != 0 {
		t.Fatal(out)
	}

	// prune with everything still in Git: nothing.
	out, code := run(t, "prune", "-y")
	if code != 0 || !strings.Contains(out, "nothing to prune") {
		t.Fatalf("%d %s", code, out)
	}

	// remove by name, without -y and without a terminal: aborted.
	out, code = run(t, "remove", "web")
	if code != 0 || !strings.Contains(out, "aborted") {
		t.Fatalf("%d %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(h.unitDir(), "podcd-web.kube")); err != nil {
		t.Fatal("an aborted remove took web's unit")
	}

	h.resetCalls()
	out, code = run(t, "remove", "web", "-y")
	if code != 0 || !strings.Contains(out, "removed: web") {
		t.Fatalf("%d %s", code, out)
	}
	if !strings.Contains(h.calls(), "stop podcd-web.service") || strings.Contains(h.calls(), "stop podcd-api.service") {
		t.Fatalf("only web should have been stopped:\n%s", h.calls())
	}

	// Git drops api too; prune finds it.
	h.commit("apiVersion: gitops.podcd.io/v1\nkind: Host\nmetadata: {name: vm-1}\nspec: {applications: []}\n")
	out, code = run(t, "prune", "-y")
	if code != 0 || !strings.Contains(out, "removed: api") {
		t.Fatalf("%d %s", code, out)
	}
	if entries, _ := os.ReadDir(h.unitDir()); len(entries) != 0 {
		t.Fatalf("the unit directory should be empty, got %d entries", len(entries))
	}
}

func TestRemoveAllTakesEveryManagedUnitAndNothingElse(t *testing.T) {
	h := newFakeHost(t, hostWith("api", "web"))
	h.reply("podman.ps", psRunning("api", "web"))
	if out, code := run(t, "reconcile"); code != 0 {
		t.Fatal(out)
	}
	theirs := filepath.Join(h.unitDir(), "podcd-theirs.kube")
	if err := os.WriteFile(theirs, []byte("[Kube]\nYaml=/x.yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := run(t, "remove", "--all", "-y")
	if code != 0 || !strings.Contains(out, "removed: api, web") {
		t.Fatalf("%d %s", code, out)
	}
	if _, err := os.Stat(theirs); err != nil {
		t.Fatal("remove --all took a unit podcd does not manage")
	}
}

func TestCommandsFailCleanlyWithoutAConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PODCD_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	for _, args := range [][]string{{"status"}, {"plan"}, {"reconcile"}, {"health"}, {"logs"}, {"validate"}, {"prune", "-y"}, {"remove", "--all", "-y"}} {
		out, code := run(t, args...)
		if code == 0 || !strings.Contains(out, "config") {
			t.Errorf("%v without a config: %d %s", args, code, out)
		}
	}
}

func TestTeardownRemovesEveryApplicationThenTheAgent(t *testing.T) {
	h := newFakeHost(t, hostWith("api", "web"))
	h.reply("podman.ps", psRunning("api", "web"))
	if out, code := run(t, "reconcile"); code != 0 {
		t.Fatal(out)
	}
	service := filepath.Join(h.home, ".config", "systemd", "user", "podcd-agent.service")
	if out, code := run(t, "install"); code != 0 {
		t.Fatal(out)
	}

	out, code := run(t, "teardown")
	if code != 0 || !strings.Contains(out, "stop and remove 2 application(s): api, web") || !strings.Contains(out, "aborted") {
		t.Fatalf("teardown should describe what it would do and stop at the prompt: %d\n%s", code, out)
	}
	if _, err := os.Stat(service); err != nil {
		t.Fatal("an aborted teardown removed the agent's unit")
	}

	h.resetCalls()
	out, code = run(t, "teardown", "-y")
	if code != 0 || !strings.Contains(out, "removed: api, web") || !strings.Contains(out, "removed "+service) {
		t.Fatalf("%d\n%s", code, out)
	}
	if entries, _ := os.ReadDir(h.unitDir()); len(entries) != 0 {
		t.Fatalf("application units should be gone, got %d", len(entries))
	}
	if _, err := os.Stat(service); !os.IsNotExist(err) {
		t.Fatal("the agent's unit should be gone")
	}
	calls := h.calls()
	for _, want := range []string{"stop podcd-api.service", "stop podcd-web.service", "podman pod rm --force --time 10 api", "podman pod rm --force --time 10 web", "stop podcd-agent.service", "disable podcd-agent.service"} {
		if !strings.Contains(calls, want) {
			t.Errorf("teardown should have run %q:\n%s", want, calls)
		}
	}
	// The config and state are still there for a human to look at.
	if _, err := os.Stat(filepath.Join(h.home, ".config", "podcd", "agent.yaml")); err != nil {
		t.Fatal("teardown without --purge-config removed the config")
	}
	if _, err := os.Stat(filepath.Join(h.home, ".local", "state", "podcd", "state.json")); err != nil {
		t.Fatal("teardown without --purge-state removed the state")
	}
}

func TestConfigPathAndView(t *testing.T) {
	h := newFakeHost(t, hostWith("api"))
	want := filepath.Join(h.home, ".config", "podcd", "agent.yaml")
	out, code := run(t, "config", "path")
	if code != 0 || strings.TrimSpace(out) != want {
		t.Fatalf("%d %q", code, out)
	}
	out, code = run(t, "config", "view")
	if code != 0 || !strings.Contains(out, "host: vm-1") || !strings.Contains(out, "url: "+h.repo) {
		t.Fatalf("%d\n%s", code, out)
	}
	if out, code := run(t, "config", "view", "/nonexistent/agent.yaml"); code == 0 {
		t.Fatalf("a missing file must fail: %s", out)
	}

	// Without any config, path says where one would go and view fails.
	t.Setenv("HOME", t.TempDir())
	out, code = run(t, "config", "path")
	if code != 0 || !strings.HasSuffix(strings.TrimSpace(out), filepath.Join(".config", "podcd", "agent.yaml")) {
		t.Fatalf("%d %q", code, out)
	}
	if _, code := run(t, "config", "view"); code == 0 {
		t.Fatal("view without a config must fail")
	}
}

func TestInitScaffoldsARepositoryThatValidates(t *testing.T) {
	h := newFakeHost(t, hostWith("api"))
	dir := filepath.Join(h.home, "new-repo")
	out, code := run(t, "init", dir, "--host", "vm-1")
	if code != 0 {
		t.Fatal(out)
	}
	for _, f := range []string{"apps.yaml", "hosts.yaml", "README.md"} {
		if !strings.Contains(out, filepath.Join(dir, f)) {
			t.Errorf("init should report writing %s:\n%s", f, out)
		}
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s was not written", f)
		}
	}
	if !strings.Contains(out, "not a git repository yet") {
		t.Errorf("init should remind the user to commit:\n%s", out)
	}
	if out, code := run(t, "init", dir, "--host", "vm-1"); code == 0 {
		t.Fatalf("init must not overwrite without --force: %s", out)
	}
	if out, code := run(t, "init", dir, "--host", "vm-1", "--force"); code != 0 {
		t.Fatal(out)
	}
	if out, code := run(t, "lint", dir); code != 0 {
		t.Fatalf("the scaffold should lint clean: %s", out)
	}
}
