package reconciler

import (
	"context"
	"fmt"
	"sort"

	"github.com/podcd/podcd/pkg/model"
)

// PruneResult is what one Prune call did, application by application
type PruneResult struct {
	// Removed are the applications actually stopped and removed.
	Removed []string
	// Skipped are present on the host but not owned by podcd. Prune never
	// touches these - the same refusal the planner gives a reconcile that
	// finds a unit it does not manage.
	Skipped []string
	// NotFound were asked for by name but are not on this host at all.
	NotFound []string
	// Failed is name -> the error removing it hit. Prune does not stop at
	// the first failure - a cleanup command that gives up after one bad
	// application is worse than one that finishes the rest and reports what
	// did not work.
	Failed map[string]error
}

// Empty reports whether the result changed nothing.
func (r PruneResult) Empty() bool { return len(r.Removed) == 0 }

// OK reports whether every requested removal succeeded (Skipped and NotFound
// do not count as failures - they were never going to be removed).
func (r PruneResult) OK() bool { return len(r.Failed) == 0 }

// Candidates inspects the host and returns the managed application names
func (e *Engine) Candidates(ctx context.Context, names []string, all bool) ([]string, model.ActualState, error) {
	actual, err := e.rt.Inspect(ctx)
	if err != nil {
		return nil, actual, err
	}
	if all {
		var out []string
		for _, n := range actual.Names() {
			if actual.Apps[n].Managed {
				out = append(out, n)
			}
		}
		return out, actual, nil
	}
	return names, actual, nil
}

// Prune removes applications directly, without consulting Git
// For a host whose repository is unreachable or simply the wrong tool for "get rid of this now".
// Pass all to remove every application podcd manages; otherwise Prune acts only on the names given.
//
// It holds the same lock Reconcile does, so it cannot race the agent's own loop.
// Never touches a unit that is not managed by podcd.
func (e *Engine) Prune(ctx context.Context, names []string, all bool) (PruneResult, error) {
	var res PruneResult
	res.Failed = map[string]error{}

	lock, err := Acquire(e.cfg.LockPath())
	if err != nil {
		return res, err
	}
	defer lock.Release()

	targets, actual, err := e.Candidates(ctx, names, all)
	if err != nil {
		return res, err
	}
	if len(targets) == 0 {
		return res, nil
	}

	st, err := e.store.Load()
	if err != nil {
		e.log.Warn("local state could not be read", "error", err)
	}

	seen := map[string]bool{}
	for _, name := range targets {
		if seen[name] {
			continue
		}
		seen[name] = true

		app, exists := actual.Apps[name]
		switch {
		case !exists:
			res.NotFound = append(res.NotFound, name)
		case !app.Managed:
			res.Skipped = append(res.Skipped, name)
		default:
			e.log.Warn("pruning application", "app", name)
			if err := e.rt.Remove(ctx, name); err != nil {
				res.Failed[name] = err
				e.log.Error("could not prune application", "app", name, "error", err)
				continue
			}
			st.Forget(name)
			res.Removed = append(res.Removed, name)
		}
	}

	sort.Strings(res.Removed)
	sort.Strings(res.Skipped)
	sort.Strings(res.NotFound)

	if len(res.Removed) > 0 {
		e.save(&st)
	}
	if len(res.Failed) > 0 {
		return res, fmt.Errorf("pruning failed for %d application(s)", len(res.Failed))
	}
	return res, nil
}
