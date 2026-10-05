package reconciler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"

	"github.com/podcd/podcd/pkg/config"
	"github.com/podcd/podcd/pkg/git"
	"github.com/podcd/podcd/pkg/secrets"
)

// Source fetches and loads the repository; it never touches the runtime.
type Source struct {
	Repos   []*git.Repository
	Host    string
	Secrets *secrets.Resolver
	Log     *slog.Logger
	// TokenRefs holds each repository's auth.token reference by repo name.
	TokenRefs map[string]string
	// ValuesFiles are agent.yaml values files, by repo name.
	ValuesFiles map[string][]string
}

// LoadIndex fetches and loads every repository, returning the index, the
// agent.yaml values, the commits, and the repositories that were offline.
func (s *Source) LoadIndex(ctx context.Context, only ...string) (*config.Index, config.Values, map[string]string, []string, error) {
	index := config.NewIndex()
	revisions := map[string]string{}
	var offline []string
	values := config.Values{}

	for _, repo := range s.Repos {
		if len(only) > 0 && !slices.Contains(only, repo.Name) {
			continue
		}
		if err := s.resolveAuth(ctx, repo); err != nil {
			return nil, nil, nil, nil, err
		}
		sha, err := repo.Sync(ctx)
		switch {
		case err == nil:
		case errors.Is(err, git.ErrOffline) && sha != "":
			offline = append(offline, repo.Name)
			s.logWarn("git remote unreachable, using the commit already on disk",
				"repo", repo.Name, "revision", sha, "error", err)
		default:
			return nil, nil, nil, nil, fmt.Errorf("repository %s: %w", repo.Name, err)
		}
		revisions[repo.Name] = sha

		for _, vf := range s.ValuesFiles[repo.Name] {
			v, err := config.LoadValuesFile(filepath.Join(repo.TreePath(), vf))
			if err != nil {
				return nil, nil, nil, nil, fmt.Errorf("repository %s: %w", repo.Name, err)
			}
			values = config.MergeValues(values, v)
		}
		if err := index.LoadTree(repo.Name, repo.TreePath()); err != nil {
			return nil, nil, nil, nil, err
		}
	}
	return index, values, revisions, offline, nil
}

// resolveAuth resolves the token reference on every load, so rotation needs no restart.
func (s *Source) resolveAuth(ctx context.Context, repo *git.Repository) error {
	ref := s.TokenRefs[repo.Name]
	if ref == "" {
		return nil
	}
	if s.Secrets == nil {
		return fmt.Errorf("repository %s: auth.token needs a secret provider", repo.Name)
	}
	token, err := s.Secrets.Resolve(ctx, ref)
	if err != nil {
		return fmt.Errorf("repository %s: auth.token: %w", repo.Name, err)
	}
	repo.Auth.Token = token
	return nil
}

func (s *Source) logWarn(msg string, args ...any) {
	if s.Log != nil {
		s.Log.Warn(msg, args...)
	}
}

// ReposFromConfig returns the repositories, token references and values files, keyed by repo name.
func ReposFromConfig(cfg config.AgentConfig) ([]*git.Repository, map[string]string, map[string][]string) {
	r := cfg.Repository
	tokenRefs := map[string]string{}
	valuesFiles := map[string][]string{}
	if len(r.Values) > 0 {
		valuesFiles[r.Name] = r.Values
	}
	repo := git.New(r.Name, r.URL, r.Revision, r.Path, cfg.ReposDir())
	repo.Insecure = r.Insecure
	if a := r.Auth; a != nil {
		repo.Auth = git.Auth{
			Username:          a.Username,
			SSHKeyPath:        a.SSHKeyPath,
			SSHKnownHostsPath: a.SSHKnownHostsPath,
		}
		if a.Token != "" {
			tokenRefs[r.Name] = a.Token
		}
	}
	return []*git.Repository{repo}, tokenRefs, valuesFiles
}
