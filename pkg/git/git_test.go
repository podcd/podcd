package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// newUpstream creates a real local repository to clone from. A file:// remote
// exercises the same code path as a network remote, with no network and no SSH.
func newUpstream(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	run(t, dir, "init", "--quiet", "--initial-branch=main")
	run(t, dir, "config", "user.email", "test@example.com")
	run(t, dir, "config", "user.name", "podcd test")
	writeFile(t, filepath.Join(dir, "app.yaml"), "one\n")
	run(t, dir, "add", ".")
	run(t, dir, "commit", "--quiet", "-m", "first")
	return dir
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCloneFetchAndPin(t *testing.T) {
	ctx := context.Background()
	upstream := newUpstream(t)
	repo := New("infra", upstream, "main", "", t.TempDir())

	sha, err := repo.Sync(ctx)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if len(sha) != 40 {
		t.Fatalf("want a full commit sha, got %q", sha)
	}
	if _, err := os.Stat(filepath.Join(repo.Dir, "app.yaml")); err != nil {
		t.Fatalf("working tree is missing the file: %v", err)
	}

	// A second sync with no upstream change is a no-op at the same commit.
	again, err := repo.Sync(ctx)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if again != sha {
		t.Fatalf("revision moved without an upstream change: %s -> %s", sha, again)
	}

	// A new commit upstream is picked up.
	writeFile(t, filepath.Join(upstream, "app.yaml"), "two\n")
	run(t, upstream, "commit", "--quiet", "-am", "second")
	moved, err := repo.Sync(ctx)
	if err != nil {
		t.Fatalf("third sync: %v", err)
	}
	if moved == sha {
		t.Fatal("the agent did not pick up the new commit")
	}
	content, err := os.ReadFile(filepath.Join(repo.Dir, "app.yaml"))
	if err != nil || string(content) != "two\n" {
		t.Fatalf("working tree was not updated: %q %v", content, err)
	}
}

func TestPinnedToATagDoesNotMove(t *testing.T) {
	ctx := context.Background()
	upstream := newUpstream(t)
	run(t, upstream, "tag", "v1.0.0")
	repo := New("infra", upstream, "v1.0.0", "", t.TempDir())

	pinned, err := repo.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(upstream, "app.yaml"), "moved on\n")
	run(t, upstream, "commit", "--quiet", "-am", "after the tag")

	still, err := repo.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if still != pinned {
		t.Fatal("a pinned tag must not follow the branch")
	}
}

func TestPinnedToACommitSha(t *testing.T) {
	ctx := context.Background()
	upstream := newUpstream(t)
	repo := New("infra", upstream, "main", "", t.TempDir())
	first, err := repo.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(upstream, "app.yaml"), "later\n")
	run(t, upstream, "commit", "--quiet", "-am", "second")

	pinned := New("infra", upstream, first, "", t.TempDir())
	got, err := pinned.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != first {
		t.Fatalf("pinned sha resolved to %s, want %s", got, first)
	}
}

func TestUnreachableRemoteFallsBackToTheLocalCommit(t *testing.T) {
	ctx := context.Background()
	upstream := newUpstream(t)
	base := t.TempDir()
	repo := New("infra", upstream, "main", "", base)

	sha, err := repo.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// The remote disappears, exactly like a network outage or a dead Git host.
	if err := os.RemoveAll(upstream); err != nil {
		t.Fatal(err)
	}

	got, err := repo.Sync(ctx)
	if !errors.Is(err, ErrOffline) {
		t.Fatalf("an unreachable remote must report ErrOffline, got: %v", err)
	}
	if got != sha {
		t.Fatalf("the agent must keep reconciling from the commit it has, got %q", got)
	}
}

func TestUnknownRevisionIsAnError(t *testing.T) {
	ctx := context.Background()
	repo := New("infra", newUpstream(t), "no-such-branch", "", t.TempDir())
	if _, err := repo.Sync(ctx); err == nil {
		t.Fatal("a missing revision must fail rather than reconcile something arbitrary")
	}
}

func TestSubdirectoryTreePath(t *testing.T) {
	repo := New("infra", "https://example.com/x.git", "main", "clusters/prod", "/state/repos")
	want := filepath.Join("/state/repos", "infra", "clusters/prod")
	if repo.TreePath() != want {
		t.Fatalf("TreePath = %q, want %q", repo.TreePath(), want)
	}
}

func TestLocalChangesAreDiscarded(t *testing.T) {
	ctx := context.Background()
	upstream := newUpstream(t)
	repo := New("infra", upstream, "main", "", t.TempDir())
	if _, err := repo.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	// Somebody edited the checkout on the host. Git is the source of truth.
	writeFile(t, filepath.Join(repo.Dir, "app.yaml"), "hand edited\n")
	writeFile(t, filepath.Join(repo.Dir, "extra.yaml"), "not in git\n")

	writeFile(t, filepath.Join(upstream, "app.yaml"), "three\n")
	run(t, upstream, "commit", "--quiet", "-am", "third")
	if _, err := repo.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	content, _ := os.ReadFile(filepath.Join(repo.Dir, "app.yaml"))
	if string(content) != "three\n" {
		t.Fatalf("the local edit survived: %q", content)
	}
	if _, err := os.Stat(filepath.Join(repo.Dir, "extra.yaml")); !os.IsNotExist(err) {
		t.Fatal("an untracked file survived the checkout")
	}
}

func TestRevisionCanBeSwitchedOnAnExistingCheckout(t *testing.T) {
	ctx := context.Background()
	upstream := newUpstream(t)
	run(t, upstream, "tag", "v1")
	writeFile(t, filepath.Join(upstream, "app.yaml"), "two\n")
	run(t, upstream, "commit", "--quiet", "-am", "second")
	run(t, upstream, "checkout", "--quiet", "-b", "canary")
	writeFile(t, filepath.Join(upstream, "app.yaml"), "canary\n")
	run(t, upstream, "commit", "--quiet", "-am", "canary")
	run(t, upstream, "checkout", "--quiet", "main")

	base := t.TempDir()
	repo := New("infra", upstream, "main", "", base)
	if _, err := repo.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	for rev, want := range map[string]string{"v1": "one\n", "canary": "canary\n", "main": "two\n"} {
		repo = New("infra", upstream, rev, "", base)
		if _, err := repo.Sync(ctx); err != nil {
			t.Fatalf("switching to %s: %v", rev, err)
		}
		if got, _ := os.ReadFile(filepath.Join(repo.Dir, "app.yaml")); string(got) != want {
			t.Fatalf("at %s the tree holds %q, want %q", rev, got, want)
		}
	}
}

func TestSubdirectoryIsWhatTheAgentReads(t *testing.T) {
	ctx := context.Background()
	upstream := newUpstream(t)
	if err := os.MkdirAll(filepath.Join(upstream, "clusters", "prod"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(upstream, "clusters", "prod", "host.yaml"), "prod\n")
	run(t, upstream, "add", ".")
	run(t, upstream, "commit", "--quiet", "-m", "subdir")

	repo := New("infra", upstream, "main", "clusters/prod", t.TempDir())
	if _, err := repo.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(repo.TreePath(), "host.yaml")); err != nil || string(got) != "prod\n" {
		t.Fatalf("tree path should be the subdirectory of the checkout: %q %v", got, err)
	}
}

func TestSyncIsSafeToRepeatAfterAnInterruptedClone(t *testing.T) {
	ctx := context.Background()
	upstream := newUpstream(t)
	base := t.TempDir()
	// A directory that exists but is not a repository: a clone that was cut
	// off, or a stray file. Sync must recover rather than fail forever.
	if err := os.MkdirAll(filepath.Join(base, "infra"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(base, "infra", "half.txt"), "leftover\n")
	repo := New("infra", upstream, "main", "", base)
	if _, err := repo.Sync(ctx); err != nil {
		t.Fatalf("sync over a half-made directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo.Dir, "app.yaml")); err != nil {
		t.Fatal("the checkout was not made")
	}
}
