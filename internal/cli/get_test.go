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
	out, _ = run(t, "get", "pods", "--repo", dir)
	if !strings.Contains(out, "CONTAINERS") || !strings.Contains(out, "8080→80") {
		t.Errorf("pods table:\n%s", out)
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

func TestGetDefaultsToTheAgentConfigRepository(t *testing.T) {
	cfg := agentConfigFor(t, repoWithEverything(t), "vm-1")
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
	state := t.TempDir()
	cfg := filepath.Join(state, "agent.yaml")
	if err := os.WriteFile(cfg, []byte("host: vm-1\nstateDir: "+state+"\nenvFile: "+filepath.Join(state, "agent.env")+"\nrepository:\n  name: gitops\n  url: "+dir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := run(t, "--config", cfg, "get", "hosts", "--repo", "gitops")
	if code != 0 || !strings.Contains(out, "gitops/hosts.yaml:") {
		t.Fatalf("--repo NAME should fetch the configured repository:\n%s", out)
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
	if out, code := run(t, "--config", cfg, "get", "--repo", "nothing"); code == 0 || !strings.Contains(out, "(gitops)") {
		t.Fatalf("an unknown --repo should name the configured repository:\n%s", out)
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

// repoWithTemplate is repoWithEverything plus api.yaml.tpl, a templated Pod, and vm-api, which runs it.
func repoWithTemplate(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // keep the developer's own agent config out
	t.Setenv("XDG_CONFIG_HOME", "")
	dir := repoWithEverything(t)
	tpl := "apiVersion: v1\nkind: Pod\nmetadata:\n  name: '{{ default \"api\" .Values.name }}'\nspec:\n  containers:\n    - name: api\n      image: ghcr.io/you/api" + testDigest + "\n"
	for file, doc := range map[string]string{
		"api.yaml.tpl": tpl,
		"vm-api.yaml":  "apiVersion: gitops.podcd.io/v1\nkind: Host\nmetadata: {name: vm-api}\nspec: {applications: [api]}\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// agentConfigFor commits dir and writes an agent.yaml that reconciles it as host.
func agentConfigFor(t *testing.T, dir, host string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Setenv("PODCD_HOST", "") // it beats the config's host:
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"-c", "user.email=t@e.x", "-c", "user.name=t", "commit", "-qm", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	state := t.TempDir()
	cfg := filepath.Join(state, "agent.yaml")
	if err := os.WriteFile(cfg, []byte("host: "+host+"\nstateDir: "+state+"\nenvFile: "+filepath.Join(state, "agent.env")+"\nrepository:\n  name: gitops\n  url: "+dir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// A Pod in a template is still a Pod: get renders it for the host.
func TestGetRendersTemplatesForTheHost(t *testing.T) {
	out, code := run(t, "get", "pods", "--repo", repoWithTemplate(t), "--host", "vm-api")
	if code != 0 || !strings.Contains(out, "api.yaml.tpl:1") {
		t.Fatalf("%d\n%s", code, out)
	}
}

func TestGetWithoutAHostNamesTheSkippedTemplates(t *testing.T) {
	out, code := run(t, "get", "pods", "--repo", repoWithTemplate(t))
	if code != 0 || !strings.Contains(out, "warning: 1 template(s) not listed: pass --host") || strings.Contains(out, "api.yaml.tpl") {
		t.Fatalf("%d\n%s", code, out)
	}
}

func TestGetOfATemplatedNameWithoutAHostPointsAtHost(t *testing.T) {
	out, code := run(t, "get", "pod", "api", "--repo", repoWithTemplate(t))
	if code == 0 || !strings.Contains(out, "1 template(s) were not rendered, pass --host") {
		t.Fatalf("%d\n%s", code, out)
	}
}

// As in lint: a --host the repository does not define is a mistake.
func TestGetWithAnUnknownHostIsAnError(t *testing.T) {
	out, code := run(t, "get", "pods", "--repo", repoWithTemplate(t), "--host", "nosuchhost")
	if code == 0 || !strings.Contains(out, `no Host document matches "nosuchhost"`) {
		t.Fatalf("%d\n%s", code, out)
	}
}

// Whatever the kind: hosts come from no template, but --host still names one.
func TestGetHostsWithAnUnknownHostIsAnError(t *testing.T) {
	out, code := run(t, "get", "hosts", "--repo", repoWithTemplate(t), "--host", "nosuchhost")
	if code == 0 || !strings.Contains(out, `no Host document matches "nosuchhost"`) {
		t.Fatalf("%d\n%s", code, out)
	}
}

// Hosts come from no template, so a broken one must not hide them.
func TestGetHostsIgnoresABrokenTemplate(t *testing.T) {
	dir := repoWithTemplate(t)
	if err := os.WriteFile(filepath.Join(dir, "broken.yaml.tpl"), []byte("{{ .Values.missing.key }}"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code := run(t, "get", "hosts", "--repo", dir, "--host", "vm-2")
	if code != 0 || !strings.Contains(out, "vm-2") {
		t.Fatalf("%d\n%s", code, out)
	}
}

func TestGetWithAnUnknownHostOnTheAgentsRepositoryIsAnError(t *testing.T) {
	dir := repoWithTemplate(t)
	out, code := run(t, "--config", agentConfigFor(t, dir, "vm-2"), "--host", "nosuchhost", "get", "pods")
	if code == 0 || !strings.Contains(out, `no Host document matches "nosuchhost"`) {
		t.Fatalf("%d\n%s", code, out)
	}
}

// repoWithSecrets is repoWithTemplate plus a SecretStore, a templated ExternalSecret with
// four keys, and worker on vm-es, which reads its Secret.
func repoWithSecrets(t *testing.T) string {
	t.Helper()
	dir := repoWithTemplate(t)
	store := "apiVersion: external-secrets.io/v1beta1\nkind: SecretStore\nmetadata: {name: local}\nspec:\n  provider:\n    env: {}\n"
	es := "apiVersion: external-secrets.io/v1beta1\nkind: ExternalSecret\nmetadata: {name: api-creds}\nspec:\n  secretStoreRef: {name: local}\n  target: {name: '{{ default \"api-secret\" .Values.target }}'}\n  data:\n"
	for _, k := range []string{"a", "b", "c", "d"} {
		es += "    - {secretKey: " + k + ", remoteRef: {key: API_" + k + "}}\n"
	}
	worker := "apiVersion: v1\nkind: Pod\nmetadata: {name: worker}\nspec:\n  containers:\n    - name: worker\n      image: ghcr.io/you/worker" + testDigest + "\n      envFrom: [{secretRef: {name: api-secret}}]\n" +
		"---\napiVersion: gitops.podcd.io/v1\nkind: Host\nmetadata: {name: vm-es}\nspec: {applications: [worker]}\n"
	for file, doc := range map[string]string{"stores.yaml": store, "creds.yaml.tpl": es, "worker.yaml": worker} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// Every kind counts: nginx, side, worker, web, prod, vm-1, vm-2, vm-api, vm-es, and the store.
func TestLintCountsEveryKind(t *testing.T) {
	out, code := run(t, "lint", repoWithSecrets(t))
	if code != 0 || !strings.Contains(out, "ok: 10 document(s) and 2 template(s) loaded") {
		t.Fatalf("%d\n%s", code, out)
	}
}

func TestGetListsSecretStores(t *testing.T) {
	out, code := run(t, "get", "secretstores", "--repo", repoWithSecrets(t))
	if code != 0 || !strings.Contains(out, "NAME   PROVIDER  SOURCE") || !strings.Contains(out, "local  env") {
		t.Fatalf("%d\n%s", code, out)
	}
}

// Rendered like any template; key names shown, the fourth only counted.
func TestGetListsRenderedExternalSecrets(t *testing.T) {
	out, code := run(t, "get", "externalsecrets", "--repo", repoWithSecrets(t), "--host", "vm-es")
	if code != 0 || !strings.Contains(out, "api-creds  local  api-secret  a,b,c +1  creds.yaml.tpl:1") {
		t.Fatalf("%d\n%s", code, out)
	}
}

// repoForUse has two hosts: h1 runs api, which reads a Secret an ExternalSecret
// provisions from store "local"; h2 runs nothing. "other" and "unused" serve no one.
func repoForUse(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	dir := t.TempDir()
	docs := `apiVersion: v1
kind: Pod
metadata: {name: api}
spec:
  containers:
    - name: api
      image: ghcr.io/you/api` + testDigest + `
      env:
        - name: TOKEN
          valueFrom: {secretKeyRef: {name: api-secret, key: token}}
---
apiVersion: external-secrets.io/v1beta1
kind: SecretStore
metadata: {name: local}
spec: {provider: {env: {}}}
---
apiVersion: external-secrets.io/v1beta1
kind: SecretStore
metadata: {name: unused}
spec: {provider: {env: {}}}
---
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata: {name: api-creds}
spec:
  secretStoreRef: {name: local}
  target: {name: api-secret}
  data:
    - {secretKey: token, remoteRef: {key: API_TOKEN}}
---
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata: {name: other}
spec:
  secretStoreRef: {name: unused}
  data:
    - {secretKey: x, remoteRef: {key: X}}
---
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: h1}
spec: {applications: [api]}
---
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: h2}
spec: {}
`
	if err := os.WriteFile(filepath.Join(dir, "all.yaml"), []byte(docs), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// With a host, get follows what it runs: its Pods, their Secrets' ExternalSecrets and stores.
func TestGetWithAHostListsOnlyWhatItUses(t *testing.T) {
	out, code := run(t, "get", "--repo", repoForUse(t), "--host", "h1")
	if code != 0 {
		t.Fatal(out)
	}
	for _, want := range []string{"Host            h1", "Pod             api", "SecretStore     local", "ExternalSecret  api-creds"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"h2", "unused", "other"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("%q is not used by h1:\n%s", unwanted, out)
		}
	}
}

// get hosts is how to find the others, so it is never narrowed to one.
func TestGetHostsListsEveryHost(t *testing.T) {
	out, code := run(t, "get", "hosts", "--repo", repoForUse(t), "--host", "h1")
	if code != 0 || !strings.Contains(out, "h1") || !strings.Contains(out, "h2") {
		t.Fatalf("%d\n%s", code, out)
	}
}

func TestGetAllListsEveryHostsDocuments(t *testing.T) {
	out, code := run(t, "get", "-a", "--repo", repoForUse(t), "--host", "h1")
	if code != 0 || !strings.Contains(out, "h2") || !strings.Contains(out, "unused") || !strings.Contains(out, "other") {
		t.Fatalf("%d\n%s", code, out)
	}
}

// A host that does not compile cannot say what it uses: warn and list everything.
func TestGetOfAHostThatDoesNotCompileListsEverything(t *testing.T) {
	dir := repoForUse(t)
	h3 := "apiVersion: gitops.podcd.io/v1\nkind: Host\nmetadata: {name: h3}\nspec: {applications: [missing]}\n"
	if err := os.WriteFile(filepath.Join(dir, "h3.yaml"), []byte(h3), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code := run(t, "get", "secretstores", "--repo", dir, "--host", "h3")
	if code != 0 || !strings.Contains(out, "warning: cannot tell what h3 uses") || !strings.Contains(out, "unused") {
		t.Fatalf("%d\n%s", code, out)
	}
}

func TestGetOfANameTheHostDoesNotUse(t *testing.T) {
	out, code := run(t, "get", "secretstore", "unused", "--repo", repoForUse(t), "--host", "h1")
	if code == 0 || !strings.Contains(out, `no secretstores named "unused" that h1 uses`) {
		t.Fatalf("%d\n%s", code, out)
	}
}

func TestGetRendersTheAgentsRepositoryForItsIdentity(t *testing.T) {
	dir := repoWithTemplate(t)
	out, code := run(t, "--config", agentConfigFor(t, dir, "vm-api"), "get", "pods")
	if code != 0 || !strings.Contains(out, "gitops/api.yaml.tpl:1") {
		t.Fatalf("%d\n%s", code, out)
	}
}

// A workstation's hostname is no Host, and hosts come from no template: get lists them quietly.
func TestGetHostsWithAnIdentityTheRepositoryLacks(t *testing.T) {
	dir := repoWithTemplate(t)
	out, code := run(t, "--config", agentConfigFor(t, dir, "laptop"), "get", "hosts")
	if code != 0 || !strings.Contains(out, "vm-2") || strings.Contains(out, "warning") {
		t.Fatalf("%d\n%s", code, out)
	}
}

func TestGetPodsWithAnIdentityTheRepositoryLacksWarns(t *testing.T) {
	dir := repoWithTemplate(t)
	out, code := run(t, "--config", agentConfigFor(t, dir, "laptop"), "get", "pods")
	if code != 0 || !strings.Contains(out, `warning: 1 template(s) not listed: no Host document matches "laptop"`) {
		t.Fatalf("%d\n%s", code, out)
	}
}

func TestGetRepoDirIgnoresTheAgentIdentity(t *testing.T) {
	dir := repoWithTemplate(t)
	out, code := run(t, "--config", agentConfigFor(t, dir, "vm-2"), "get", "pods", "--repo", dir)
	if code != 0 || !strings.Contains(out, "warning: 1 template(s) not listed: pass --host") {
		t.Fatalf("only --host renders another repository:\n%s", out)
	}
}

// lol why not, let's test that the README's create commands actually run and produce valid documents.
func TestInitReadmeCreateCommandsRun(t *testing.T) {
	dir := t.TempDir()
	if out, code := run(t, "init", dir); code != 0 {
		t.Fatal(out)
	}
	readme, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(readme), "\n") {
		line, _, _ = strings.Cut(strings.TrimSpace(line), " >>")
		if !strings.HasPrefix(line, "podcd create ") {
			continue
		}
		n++
		if out, code := run(t, strings.Fields(line)[1:]...); code != 0 {
			t.Errorf("README: %s\n%s", line, out)
		}
	}
	if n == 0 {
		t.Fatal("the README shows no create commands")
	}
}

func TestCreateRejectsAnUnknownKind(t *testing.T) {
	if out, code := run(t, "create", "application", "api", "--image", "x:1"); code == 0 || !strings.Contains(out, `unknown kind "application"`) {
		t.Fatalf("%d\n%s", code, out)
	}
}

// A subcommand's own flag mistake must not read as an unknown kind.
func TestCreateSubcommandKeepsItsFlagError(t *testing.T) {
	out, code := run(t, "create", "pod", "api", "--bogus")
	if code == 0 || !strings.Contains(out, "unknown flag: --bogus") || strings.Contains(out, "unknown kind") {
		t.Fatalf("%d\n%s", code, out)
	}
}

func TestConfigHasAShortFlag(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "agent.yaml")
	if out, _ := run(t, "-c", missing, "status"); !strings.Contains(out, missing) {
		t.Fatalf("-c must name the config like --config:\n%s", out)
	}
}
