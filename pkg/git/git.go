// Package git keeps a local checkout of a repository pinned to an exact commit.
//
// It shells out to the git binary on purpose.
// That way every auth method the host already has works: ssh keys, credential helpers, https tokens, local file:// remotes.
package git

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/podcd/podcd/internal/subprocess"
)

// ErrOffline means the remote could not be reached.
// The caller may carry on with the checkout it already has.
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

	// Auth holds the read credentials for a private repository.
	Auth Auth

	// Timeout bounds any single git invocation.
	Timeout time.Duration
}

// Auth is a read credential for one repository.
type Auth struct {
	Username string
	Token    string // the resolved value; set before every Sync

	SSHKeyPath        string
	SSHKnownHostsPath string
}

// DefaultUsername picks the username to pair with a token when none is set.
// GitHub takes any username for a token, its docs use x-access-token.
// GitLab and most others take oauth2 for personal and project access tokens.
// GitLab deploy tokens have their own username, set Auth.Username for those.
func DefaultUsername(url string) string {
	if strings.Contains(url, "github.com") {
		return "x-access-token"
	}
	return "oauth2"
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

// Fetch makes sure the local checkout exists and has the remote's current refs.
// A network failure is wrapped in ErrOffline so the caller can fall back to the commit it already has.
func (r *Repository) Fetch(ctx context.Context) error {
	if !r.Exists() {
		return r.clone(ctx)
	}
	remote, err := r.run(ctx, "config", "--get", "remote.origin.url")
	if err == nil && strings.TrimSpace(remote) != r.URL {
		// Remote URL changed. The checkout is just a cache, throw it away.
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

// Resolve turns the configured revision into an exact commit sha.
// It only reads the local checkout, so it still works after a failed fetch.
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

// Checkout puts the working tree at an exact commit.
// It also removes anything not in that commit, so a half-finished previous run cannot leak in.
func (r *Repository) Checkout(ctx context.Context, sha string) error {
	if _, err := r.run(ctx, "-c", "advice.detachedHead=false", "checkout", "--force", "--detach", sha); err != nil {
		return fmt.Errorf("repository %s: checking out %s: %w", r.Name, sha, err)
	}
	if _, err := r.run(ctx, "clean", "-ffdx"); err != nil {
		return fmt.Errorf("repository %s: cleaning worktree: %w", r.Name, err)
	}
	return nil
}

// Sync fetches, resolves the pinned revision, and checks it out.
// It returns the commit now in the working tree.
//
// If the remote is unreachable but a checkout already exists, Sync returns the commit it has plus ErrOffline.
// A broken network must not take running applications down, but it must not look like silent success either.
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
	return subprocess.Run(ctx, subprocess.Command{
		Bin: "git", Args: args, Dir: dir, Env: r.env(), Timeout: cmp.Or(r.Timeout, 5*time.Minute),
	})
}

// env builds the environment for git, never interactive, never prompting.
// A missing credential fails the reconcile instead of hanging the agent forever.
func (r *Repository) env() []string {
	base := os.Environ()
	env := make([]string, 0, len(base)+8)

	for _, kv := range base {
		key, _, _ := strings.Cut(kv, "=")

		// Git authentication must never become interactive through an
		// inherited askpass helper or credential UI.
		switch key {
		case "GIT_ASKPASS",
			"GIT_TERMINAL_PROMPT",
			"GCM_INTERACTIVE",
			"SSH_ASKPASS":
			continue
		}

		env = append(env, kv)
	}

	env = append(env,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"GCM_INTERACTIVE=never",
		"LC_ALL=C",
	)

	ssh := "ssh -o BatchMode=yes -o ConnectTimeout=15"
	if r.Auth.SSHKeyPath != "" {
		ssh += " -o IdentitiesOnly=yes -i " + shellQuote(r.Auth.SSHKeyPath)
	}
	if r.Auth.SSHKnownHostsPath != "" {
		ssh += " -o UserKnownHostsFile=" + shellQuote(r.Auth.SSHKnownHostsPath)
	}
	if r.Insecure {
		ssh += " -o StrictHostKeyChecking=no"
		env = append(env, "GIT_SSL_NO_VERIFY=true")
	}
	env = append(env, "GIT_SSH_COMMAND="+ssh)

	return r.authConfig(env)
}

// authConfig adds the token as git config in the environment.
// It is an Authorization header scoped to this repository's URL, so it is never sent to another host git gets redirected to.
//
// The environment may already carry GIT_CONFIG_COUNT entries from whoever started the agent.
// Ours is appended after them, not replacing them.
func (r *Repository) authConfig(env []string) []string {
	if r.Auth.Token == "" {
		return env
	}
	username := r.Auth.Username
	if username == "" {
		username = DefaultUsername(r.URL)
	}
	basic := base64.StdEncoding.EncodeToString([]byte(username + ":" + r.Auth.Token))

	count := 0
	kept := env[:0:0]
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "GIT_CONFIG_COUNT="); ok {
			count, _ = strconv.Atoi(v)
			continue
		}
		kept = append(kept, kv)
	}
	n := strconv.Itoa(count)
	return append(kept,
		"GIT_CONFIG_COUNT="+strconv.Itoa(count+1),
		"GIT_CONFIG_KEY_"+n+"=http."+r.URL+".extraheader",
		"GIT_CONFIG_VALUE_"+n+"=Authorization: Basic "+basic,
	)
}

// shellQuote makes a path safe inside GIT_SSH_COMMAND, which git hands to a shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
