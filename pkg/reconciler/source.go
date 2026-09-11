package reconciler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/podcd/podcd/pkg/config"
	"github.com/podcd/podcd/pkg/git"
	"github.com/podcd/podcd/pkg/model"
	"github.com/podcd/podcd/pkg/secrets"
)

// Source turns "some Git repositories" into "what should run on this host".
//
// It is the compiler half of the agent: pull, parse, validate, resolve. It
// never touches the container runtime.
type Source struct {
	Repos   []*git.Repository
	Host    string
	Secrets *secrets.Resolver
	Log     *slog.Logger
}

// LoadResult is a compiled desired state plus what went wrong on the way.
type LoadResult struct {
	Desired model.DesiredState
	// Offline lists repositories that could not be refreshed. Their last known
	// commit was used instead. Reconciling from a slightly old commit beats
	// taking applications down because a network hiccuped.
	Offline []string
}

// LoadDesiredState fetches every repository, compiles the documents and
// resolves them for this host.
func (s *Source) LoadDesiredState(ctx context.Context) (LoadResult, error) {
	var result LoadResult
	index := config.NewIndex()
	revisions := map[string]string{}

	for _, repo := range s.Repos {
		sha, err := repo.Sync(ctx)
		switch {
		case err == nil:
		case errors.Is(err, git.ErrOffline) && sha != "":
			result.Offline = append(result.Offline, repo.Name)
			s.logWarn("git remote unreachable, using the commit already on disk",
				"repo", repo.Name, "revision", sha, "error", err)
		default:
			return result, fmt.Errorf("repository %s: %w", repo.Name, err)
		}
		revisions[repo.Name] = sha

		if err := index.LoadTree(repo.Name, repo.TreePath()); err != nil {
			return result, err
		}
	}

	desired, err := index.Resolve(ctx, config.ResolveOptions{
		Host:      s.Host,
		Secrets:   s.Secrets,
		Revisions: revisions,
	})
	if err != nil {
		return result, err
	}
	result.Desired = desired
	return result, nil
}

func (s *Source) logWarn(msg string, args ...any) {
	if s.Log != nil {
		s.Log.Warn(msg, args...)
	}
}

// ReposFromConfig builds the repository list from the agent configuration.
func ReposFromConfig(cfg config.AgentConfig) []*git.Repository {
	repos := make([]*git.Repository, 0, len(cfg.Repositories))
	for _, r := range cfg.Repositories {
		repo := git.New(r.Name, r.URL, r.Revision, r.Path, cfg.ReposDir())
		repo.Insecure = r.Insecure
		repos = append(repos, repo)
	}
	return repos
}
