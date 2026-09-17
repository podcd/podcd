package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A fake host: podman, systemctl and journalctl are shell scripts on PATH that
// answer from files in a fixture directory and log what they were asked. The
// rest - config, unit files, manifests, state, a local Git repository - is real
// and lives under a fresh HOME. This runs on any machine with sh and git, so
// the commands are exercised end to end without a container runtime.
type fakeHost struct {
	t    *testing.T
	home string
	fix  string // fixtures: <bin>.<subcommand> (or <bin>) is the stdout, <bin>.<subcommand>.fail the stderr and a non-zero exit
	log  string
	repo string
}

const fakeBin = `#!/bin/sh
name=$(basename "$0")
printf '%s %s\n' "$name" "$*" >> "$PODCD_FAKE_LOG"
sub=""
for a in "$@"; do case "$a" in -*) ;; *) sub=$a; break;; esac; done
f="$PODCD_FAKE_DIR/$name.$sub"
[ -f "$f" ] || [ -f "$f.fail" ] || [ -f "$f.next" ] || f="$PODCD_FAKE_DIR/$name"
if [ -f "$f.fail" ]; then cat "$f.fail" >&2; exit 1; fi
if [ -f "$f" ]; then cat "$f"; fi
# A queued reply becomes the current one once this one has been given.
if [ -f "$f.next" ]; then mv "$f.next" "$f"; fi
exit 0
`

// newFakeHost sets HOME, PATH and the agent config up and commits docs to a
// local repository the config points at. Host identity is vm-1.
func newFakeHost(t *testing.T, docs string) *fakeHost {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	home := agentConfig(t)
	h := &fakeHost{t: t, home: home, fix: filepath.Join(home, "fixtures"), log: filepath.Join(home, "calls.log"), repo: filepath.Join(home, "repo")}

	bin := filepath.Join(home, "bin")
	for _, d := range []string{bin, h.fix, h.repo} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"podman", "systemctl", "journalctl"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(fakeBin), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PODCD_FAKE_DIR", h.fix)
	t.Setenv("PODCD_FAKE_LOG", h.log)
	h.reply("systemctl.is-system-running", "running\n")

	h.commit(docs)
	for _, args := range [][]string{
		{"config", "set", "repo-url", h.repo},
		{"config", "set", "host", "vm-1"},
	} {
		if out, code := run(t, args...); code != 0 {
			t.Fatalf("%v: %s", args, out)
		}
	}
	return h
}

// reply sets what a fake binary prints for a subcommand, e.g. "podman.ps".
func (h *fakeHost) reply(name, stdout string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.fix, name), []byte(stdout), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// then queues the reply a subcommand gives after its current one has been
// used once: the host changes between two looks.
func (h *fakeHost) then(name, stdout string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.fix, name+".next"), []byte(stdout), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// fail makes a subcommand exit non-zero with this on stderr.
func (h *fakeHost) fail(name, stderr string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.fix, name+".fail"), []byte(stderr), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// calls returns every fake invocation so far, one per line, e.g.
// "systemctl --user restart podcd-api.service".
func (h *fakeHost) calls() string {
	out, _ := os.ReadFile(h.log)
	return string(out)
}

func (h *fakeHost) resetCalls() { _ = os.Remove(h.log) }

func (h *fakeHost) commit(docs string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.repo, "config.yaml"), []byte(docs), 0o644); err != nil {
		h.t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.repo, ".git")); os.IsNotExist(err) {
		h.git("init", "--quiet", "--initial-branch=main")
		h.git("config", "user.email", "t@e.x")
		h.git("config", "user.name", "t")
	}
	h.git("add", "--all")
	h.git("commit", "--quiet", "--allow-empty", "-m", "update")
}

func (h *fakeHost) git(args ...string) {
	h.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = h.repo
	if out, err := cmd.CombinedOutput(); err != nil {
		h.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// unitDir is where the agent config on this host puts Quadlet units.
func (h *fakeHost) unitDir() string {
	return filepath.Join(h.home, ".config", "containers", "systemd")
}

// psRunning is podman ps with one healthy container per app, named <app>-app.
func psRunning(apps ...string) string {
	var rows []string
	for _, app := range apps {
		rows = append(rows, `{"Id":"73b46a5f1513dddd","Names":["`+app+`-app"],"Image":"docker.io/library/busybox:1.36","State":"running","Status":"Up 16 minutes (healthy)","ExitCode":0,"Restarts":0,"IsInfra":false,"Labels":{"io.podcd.app":"`+app+`","io.podcd.managed":"true"}}`)
	}
	return "[" + strings.Join(rows, ",") + "]"
}

// hostWith is a repository declaring these Pods and a Host vm-1 running them all.
func hostWith(apps ...string) string {
	images := map[string]string{
		"api": "docker.io/library/busybox@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"web": "docker.io/library/nginx@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	var b strings.Builder
	for i, app := range apps {
		b.WriteString("apiVersion: v1\nkind: Pod\nmetadata:\n  name: " + app + "\nspec:\n  containers:\n    - name: app\n      image: " + images[app] + "\n")
		b.WriteString("      ports:\n        - containerPort: 80\n          hostPort: " + string(rune('8'+i)) + "080\n---\n")
	}
	b.WriteString("apiVersion: gitops.podcd.io/v1\nkind: Host\nmetadata: {name: vm-1}\nspec: {applications: [" + strings.Join(apps, ", ") + "]}\n")
	return b.String()
}
