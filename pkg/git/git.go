// Package git keeps a local checkout of a repository pinned to an exact commit.
//
// It shells out to the git binary on purpose: every authentication mechanism a
// host already has - ssh keys, credential helpers, https tokens, local file://
// remotes - works without this package knowing about any of them. Everything is
// outbound; nothing here ever listens.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ErrOffline reports that the remote could not be reached. The caller may
// decide to carry on with the checkout it already has.
var ErrOffline = errors.New("git remote unreachable")

var shaRE = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// Repository is a local checkout of one remote repository.
type Repository struct {
	Name     string
	URL      string
	Revision string // branch, tag or commit
	Subdir   string // optional subdirectory containing the YAML
	Dir      string // local checkout path
	Insecure bool

	// Timeout bounds any single git invocation.
	Timeout time.Duration
}

// New returns a repository that will be checked out under baseDir.
func New(name, url, revision, subdir, baseDir string) *Repository {
	if revision == "" {
		revision = "main"
	}
	return &Repository{
		Name:     name,
		URL:      url,
		Revision: revision,
		Subdir:   subdir,
		Dir:      filepath.Join(baseDir, name),
		Timeout:  5 * time.Minute,
	}
}

// TreePath is the directory holding the configuration documents.
func (r *Repository) TreePath() string {
	if r.Subdir == "" {
		return r.Dir
	}
	return filepath.Join(r.Dir, r.Subdir)
}

// Exists reports whether a usable local checkout is already present.
func (r *Repository) Exists() bool {
	_, err := os.Stat(filepath.Join(r.Dir, ".git"))
	return err == nil
}

// Fetch makes sure the local checkout exists and knows about the remote's
// current refs. A network failure is wrapped in ErrOffline so the caller can
// fall back to the commit it already has.
func (r *Repository) Fetch(ctx context.Context) error {
	if !r.Exists() {
		return r.clone(ctx)
	}
	remote, err := r.run(ctx, "config", "--get", "remote.origin.url")
	if err == nil && strings.TrimSpace(remote) != r.URL {
		// The repository was re-pointed. The checkout is a cache, so throw it away.
		if err := os.RemoveAll(r.Dir); err != nil {
			return fmt.Errorf("repository %s: removing stale checkout: %w", r.Name, err)
		}
		return r.clone(ctx)
	}
	if _, err := r.run(ctx, "fetch", "--prune", "--tags", "--force", "origin"); err != nil {
		return fmt.Errorf("%w: repository %s: %v", ErrOffline, r.Name, err)
	}
	return nil
}

func (r *Repository) clone(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(r.Dir), 0o755); err != nil {
		return err
	}
	if err := os.RemoveAll(r.Dir); err != nil {
		return err
	}
	args := []string{"clone", "--quiet", r.URL, r.Dir}
	if _, err := r.runIn(ctx, filepath.Dir(r.Dir), args...); err != nil {
		return fmt.Errorf("%w: cloning %s: %v", ErrOffline, r.Name, err)
	}
	return nil
}

// Resolve turns the configured revision into an exact commit sha. It reads only
// the local checkout, so it works after a failed fetch.
func (r *Repository) Resolve(ctx context.Context) (string, error) {
	candidates := []string{
		"refs/remotes/origin/" + r.Revision,
		"refs/tags/" + r.Revision,
	}
	if shaRE.MatchString(r.Revision) {
		candidates = append([]string{r.Revision}, candidates...)
	}
	candidates = append(candidates, r.Revision)
	for _, c := range candidates {
		out, err := r.run(ctx, "rev-parse", "--verify", "--quiet", c+"^{commit}")
		if err == nil {
			if sha := strings.TrimSpace(out); sha != "" {
				return sha, nil
			}
		}
	}
	return "", fmt.Errorf("repository %s: revision %q not found (no such branch, tag or commit)", r.Name, r.Revision)
}

// Head returns the commit currently checked out.
func (r *Repository) Head(ctx context.Context) (string, error) {
	out, err := r.run(ctx, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Checkout puts the working tree at an exact commit and removes anything that
// is not in that commit, so a partially applied previous run cannot leak in.
func (r *Repository) Checkout(ctx context.Context, sha string) error {
	if _, err := r.run(ctx, "-c", "advice.detachedHead=false", "checkout", "--force", "--detach", sha); err != nil {
		return fmt.Errorf("repository %s: checking out %s: %w", r.Name, sha, err)
	}
	if _, err := r.run(ctx, "clean", "-ffdx"); err != nil {
		return fmt.Errorf("repository %s: cleaning worktree: %w", r.Name, err)
	}
	return nil
}

// Sync fetches, resolves the pinned revision and checks it out. It returns the
// commit now in the working tree.
//
// If the remote is unreachable but a checkout already exists, Sync reports the
// commit it has along with ErrOffline: a broken network must not take running
// applications down, and it must not silently look like success either.
func (r *Repository) Sync(ctx context.Context) (string, error) {
	fetchErr := r.Fetch(ctx)
	if fetchErr != nil {
		if !errors.Is(fetchErr, ErrOffline) || !r.Exists() {
			return "", fetchErr
		}
		head, err := r.Head(ctx)
		if err != nil {
			return "", fetchErr
		}
		return head, fetchErr
	}
	sha, err := r.Resolve(ctx)
	if err != nil {
		return "", err
	}
	head, err := r.Head(ctx)
	if err != nil || head != sha {
		if err := r.Checkout(ctx, sha); err != nil {
			return "", err
		}
	}
	return sha, nil
}

// ShortLog returns a one-line description of a commit, for logs a human reads.
func (r *Repository) ShortLog(ctx context.Context, sha string) string {
	out, err := r.run(ctx, "show", "--no-patch", "--format=%h %s", sha)
	if err != nil {
		return sha
	}
	return strings.TrimSpace(out)
}

func (r *Repository) run(ctx context.Context, args ...string) (string, error) {
	return r.runIn(ctx, r.Dir, args...)
}

func (r *Repository) runIn(ctx context.Context, dir string, args ...string) (string, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = r.env()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.String(), fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}

// env builds the environment for git: never interactive, never prompting, so a
// missing credential fails the reconcile instead of hanging the agent forever.
func (r *Repository) env() []string {
	env := append([]string{}, os.Environ()...)
	env = append(env,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"GCM_INTERACTIVE=never",
		"LC_ALL=C",
	)
	ssh := "ssh -o BatchMode=yes -o ConnectTimeout=15"
	if r.Insecure {
		ssh += " -o StrictHostKeyChecking=no"
		env = append(env, "GIT_SSL_NO_VERIFY=true")
	}
	env = append(env, "GIT_SSH_COMMAND="+ssh)
	return env
}
