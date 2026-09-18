package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/podcd/podcd/pkg/scaffold"
)

const testDigest = "@sha256:0000000000000000000000000000000000000000000000000000000000000000"

// repoWithEverything scaffolds a repository holding one document of each
// podcd kind plus a Pod, the way a user would build it up with `create`.
func repoWithEverything(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := scaffold.Init(dir, "vm-1", false); err != nil {
		t.Fatal(err)
	}
	add := func(file string, gen func() ([]byte, error)) {
		t.Helper()
		doc, err := gen()
		if err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(filepath.Join(dir, file), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteString("---\n" + string(doc)); err != nil {
			t.Fatal(err)
		}
	}
	add("apps.yaml", func() ([]byte, error) {
		return scaffold.Pod(scaffold.PodOptions{Name: "side", Image: "ghcr.io/you/side" + testDigest, Ports: []string{"8082:80"}})
	})
	add("groups.yaml", func() ([]byte, error) { return scaffold.Group("web", []string{"nginx", "side"}) })
	add("envs.yaml", func() ([]byte, error) { return scaffold.Environment("prod", []string{"nginx"}) })
	add("hosts.yaml", func() ([]byte, error) {
		return scaffold.Host(scaffold.HostOptions{Name: "vm-2", Environment: "prod", Groups: []string{"web"}})
	})
	return dir
}

// run executes podcd with args and returns stdout.
func run(t *testing.T, args ...string) (string, int) {
	t.Helper()
	root := newRootCommand()
	var out strings.Builder
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	code := 0
	if err := root.Execute(); err != nil {
		out.WriteString("error: " + err.Error())
		code = 1
	}
	return out.String(), code
}

func TestGetListsEverythingInAKindOrder(t *testing.T) {
	dir := repoWithEverything(t)
	out, code := run(t, "get", "--repo", dir)
	if code != 0 {
		t.Fatal(out)
	}
	want := []string{"Environment  prod", "Group        web", "Host         vm-1", "Host         vm-2", "Pod          nginx", "Pod          side"}
	last := -1
	for _, w := range want {
		i := strings.Index(out, w)
		if i < 0 || i < last {
			t.Fatalf("expected %q in order in:\n%s", w, out)
		}
		last = i
	}
	if !strings.Contains(out, "hosts.yaml:") || strings.Contains(out, dir) {
		t.Errorf("sources should be relative to the one repository:\n%s", out)
	}
}

func TestGetKindTablesAndNames(t *testing.T) {
	dir := repoWithEverything(t)
	out, _ := run(t, "get", "hosts", "--repo", dir)
	if !strings.Contains(out, "NAME  ENVIRONMENT  GROUPS  APPLICATIONS") || !strings.Contains(out, "vm-2  prod         web") {
		t.Errorf("hosts table:\n%s", out)
	}
	out, _ = run(t, "get", "apps", "--repo", dir)
	if !strings.Contains(out, "CONTAINERS") || !strings.Contains(out, "8080→80") {
		t.Errorf("applications table:\n%s", out)
	}
	out, _ = run(t, "get", "pod", "side", "-o", "yaml", "--repo", dir)
	if !strings.HasPrefix(out, "kind: Pod\n") || !strings.Contains(out, "hostPort: 8082") {
		t.Errorf("get NAME -o yaml should print the document:\n%s", out)
	}
	out, _ = run(t, "get", "envs", "-o", "json", "--repo", dir)
	if !strings.Contains(out, `"kind": "Environment"`) || !strings.Contains(out, `"name": "prod"`) {
		t.Errorf("json output:\n%s", out)
	}
	if out, code := run(t, "get", "host", "nope", "--repo", dir); code == 0 || !strings.Contains(out, `no hosts named "nope"`) {
		t.Errorf("a missing name should be an error, got %d:\n%s", code, out)
	}
	if out, code := run(t, "get", "widgets", "--repo", dir); code == 0 || !strings.Contains(out, "unknown kind") {
		t.Errorf("an unknown kind should be an error, got %d:\n%s", code, out)
	}
}

func TestGetReadsAGitURL(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := repoWithEverything(t)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"-c", "user.email=t@e.x", "-c", "user.name=t", "commit", "-qm", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	out, code := run(t, "get", "groups", "--repo", "file://"+dir)
	if code != 0 || !strings.Contains(out, "web   nginx,side") {
		t.Fatalf("get over a git URL:\n%s", out)
	}
}

func TestGetDefaultsToTheAgentConfigRepositories(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := repoWithEverything(t)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"-c", "user.email=t@e.x", "-c", "user.name=t", "commit", "-qm", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	state := t.TempDir()
	cfg := filepath.Join(state, "agent.yaml")
	if err := os.WriteFile(cfg, []byte("host: vm-1\nstateDir: "+state+"\nenvFile: "+filepath.Join(state, "agent.env")+"\nrepositories:\n  - name: gitops\n    url: "+dir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := run(t, "--config", cfg, "get", "hosts")
	if code != 0 || !strings.Contains(out, "gitops/hosts.yaml:") || !strings.Contains(out, "vm-2") {
		t.Fatalf("get via agent.yaml should fetch the configured repository:\n%s", out)
	}
}

func TestGetRepoResolvesNameThenPathThenURL(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := repoWithEverything(t)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"-c", "user.email=t@e.x", "-c", "user.name=t", "commit", "-qm", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// A second configured repository that is not fetchable: naming the first
	// must not touch it, and the default must fail on it.
	state := t.TempDir()
	cfg := filepath.Join(state, "agent.yaml")
	if err := os.WriteFile(cfg, []byte("host: vm-1\nstateDir: "+state+"\nenvFile: "+filepath.Join(state, "agent.env")+"\nrepositories:\n  - name: gitops\n    url: "+dir+"\n  - name: broken\n    url: "+filepath.Join(state, "missing")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, code := run(t, "--config", cfg, "get", "hosts"); code == 0 {
		t.Fatal("without --repo every configured repository is fetched, so the broken one must fail")
	}
	out, code := run(t, "--config", cfg, "get", "hosts", "--repo", "gitops")
	if code != 0 || !strings.Contains(out, "gitops/hosts.yaml:") {
		t.Fatalf("--repo NAME should fetch only that configured repository:\n%s", out)
	}
	// A directory and a URL work with the same config, and without any config.
	for _, repo := range []string{dir, "file://" + dir} {
		if out, code := run(t, "--config", cfg, "get", "hosts", "--repo", repo); code != 0 || !strings.Contains(out, "hosts.yaml:") {
			t.Fatalf("--repo %s:\n%s", repo, out)
		}
		if out, code := run(t, "--config", filepath.Join(state, "nope.yaml"), "get", "hosts", "--repo", repo); code != 0 || !strings.Contains(out, "hosts.yaml:") {
			t.Fatalf("--repo %s without a readable config:\n%s", repo, out)
		}
	}
	// Nothing matched: the error names what it could have been.
	if out, code := run(t, "--config", cfg, "get", "--repo", "nothing"); code == 0 || !strings.Contains(out, "gitops, broken") {
		t.Fatalf("an unknown --repo should list the configured names:\n%s", out)
	}
	if out, code := run(t, "--config", filepath.Join(state, "nope.yaml"), "get", "--repo", "nothing"); code == 0 || !strings.Contains(out, "config could not be read") {
		t.Fatalf("an unknown --repo without a config should say why the name lookup was impossible:\n%s", out)
	}
}

func TestLintCommand(t *testing.T) {
	dir := repoWithEverything(t)
	out, code := run(t, "lint", dir)
	if code != 0 || !strings.Contains(out, "ok: ") || !strings.Contains(out, "2 host(s)") {
		t.Fatalf("a scaffolded repository should lint clean:\n%s", out)
	}
	// Break it: a host that names an application nobody defined.
	if err := os.WriteFile(filepath.Join(dir, "more.yaml"), []byte("apiVersion: gitops.podcd.io/v1\nkind: Host\nmetadata: {name: vm-3}\nspec: {applications: [ghost]}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code = run(t, "lint", dir)
	if code == 0 || !strings.Contains(out, "ok: ") || !strings.Contains(out, "host vm-3: ") || !strings.Contains(out, `"ghost"`) {
		t.Fatalf("lint should say what loaded fine, then fail and name the host:\n%s", out)
	}
	if out, code := run(t, "lint", "--host", "vm-1", dir); code != 0 {
		t.Fatalf("--host vm-1 should still be clean:\n%s", out)
	}
	out, _ = run(t, "lint", "-o", "json", dir)
	if !strings.Contains(out, `"host": "vm-3"`) {
		t.Fatalf("json findings:\n%s", out)
	}
}

func TestCreatePrintsAStreamReadyToAppend(t *testing.T) {
	dir := repoWithEverything(t)
	out, code := run(t, "create", "pod", "api", "--image", "ghcr.io/you/api"+testDigest, "--port", "8083:80")
	if code != 0 || !strings.HasPrefix(out, "---\nkind: Pod\n") {
		t.Fatalf("create pod should print a document stream:\n%s", out)
	}
	if got, _ := run(t, "get", "pods", "--repo", dir); strings.Contains(got, "api") {
		t.Fatal("create must not modify anything")
	}
	// Appending the output is how documents get into the repository, and the result lints.
	f, err := os.OpenFile(filepath.Join(dir, "apps.yaml"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(out)
	f.Close()
	if got, _ := run(t, "get", "pods", "--repo", dir); !strings.Contains(got, "api") {
		t.Fatalf("the pod should now be listed:\n%s", got)
	}
	if got, code := run(t, "lint", dir); code != 0 {
		t.Fatalf("repository should lint:\n%s", got)
	}
	// Bare `create` prints help rather than doing nothing, and takes no -f/--filename any more.
	if got, code := run(t, "create"); code != 0 || strings.Contains(got, "-f") || strings.Contains(got, "--filename") {
		t.Fatalf("bare create should show help, with no -f/--filename:\n%s", got)
	}
	if _, code := run(t, "create", "-f", "whatever"); code == 0 {
		t.Fatal("-f must not exist any more")
	}
	if _, code := run(t, "create", "pod", "api", "--image", "ghcr.io/you/api"+testDigest, "--to", "x"); code == 0 {
		t.Fatal("--to must not exist any more")
	}
}
