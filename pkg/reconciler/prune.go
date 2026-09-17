package reconciler

import (
	"context"
	"fmt"
	"sort"

	"github.com/podcd/podcd/pkg/model"
)

// RemoveResult is what one Remove or Prune call did, application by application.
type RemoveResult struct {
	// Removed are the applications actually stopped and removed.
	Removed []string
	// Skipped are present on the host but not owned by podcd. Neither command
	// ever touches these - the same refusal the planner gives a reconcile
	// that finds a unit it does not manage.
	Skipped []string
	// NotFound were asked for by name but are not on this host at all.
	NotFound []string
	// Failed is name -> the error removing it hit. Removal does not stop at
	// the first failure - a cleanup command that gives up after one bad
	// application is worse than one that finishes the rest and reports what
	// did not work.
	Failed map[string]error
}

// Empty reports whether the result changed nothing.
func (r RemoveResult) Empty() bool { return len(r.Removed) == 0 }

// OK reports whether every requested removal succeeded (Skipped and NotFound
// do not count as failures - they were never going to be removed).
func (r RemoveResult) OK() bool { return len(r.Failed) == 0 }

// RemoveCandidates inspects the host and returns the names Remove would act
// on: every managed application with all, otherwise the names given.
func (e *Engine) RemoveCandidates(ctx context.Context, names []string, all bool) ([]string, model.ActualState, error) {
	actual, err := e.rt.Inspect(ctx)
	if err != nil {
		return nil, actual, err
	}
	if all {
		return managedNames(actual), actual, nil
	}
	return names, actual, nil
}

// Remove stops and removes applications directly, without consulting Git.
//
// For a host whose repository is unreachable, or simply the right tool for
// "get rid of this now". Pass all to remove every application podcd manages;
// otherwise it acts only on the names given. It never touches a unit podcd
// does not manage.
func (e *Engine) Remove(ctx context.Context, names []string, all bool) (RemoveResult, error) {
	lock, err := Acquire(e.cfg.LockPath())
	if err != nil {
		return RemoveResult{Failed: map[string]error{}}, err
	}
	defer lock.Release()

	targets, actual, err := e.RemoveCandidates(ctx, names, all)
	if err != nil {
		return RemoveResult{Failed: map[string]error{}}, err
	}
	return e.removeApps(ctx, targets, actual), nil
}

// PruneCandidates consults Git and returns the managed applications this host
// runs that the repository no longer declares for it. This is the same set a
// reconcile would delete; here it can be seen, and acted on, without also
// applying every other change the reconcile would make.
func (e *Engine) PruneCandidates(ctx context.Context) ([]string, model.ActualState, error) {
	desired, _, _, _, err := e.desiredState(ctx)
	if err != nil {
		return nil, model.ActualState{}, err
	}
	actual, err := e.rt.Inspect(ctx)
	if err != nil {
		return nil, actual, err
	}
	wanted := map[string]bool{}
	for _, app := range desired.Applications {
		wanted[app.Name] = true
	}
	var orphans []string
	for _, n := range managedNames(actual) {
		if !wanted[n] {
			orphans = append(orphans, n)
		}
	}
	return orphans, actual, nil
}

// Prune removes what podcd manages on this host but Git no longer declares,
// and nothing else. An application still in Git is never touched, whatever
// state it is in; a unit podcd does not manage is never touched at all.
func (e *Engine) Prune(ctx context.Context) (RemoveResult, error) {
	lock, err := Acquire(e.cfg.LockPath())
	if err != nil {
		return RemoveResult{Failed: map[string]error{}}, err
	}
	defer lock.Release()

	targets, actual, err := e.PruneCandidates(ctx)
	if err != nil {
		return RemoveResult{Failed: map[string]error{}}, err
	}
	return e.removeApps(ctx, targets, actual), nil
}

// removeApps stops and removes each named application that podcd manages,
// under a lock the caller already holds. The result's error is a summary;
// per-application failures are in Failed.
func (e *Engine) removeApps(ctx context.Context, targets []string, actual model.ActualState) RemoveResult {
	res := RemoveResult{Failed: map[string]error{}}
	if len(targets) == 0 {
		return res
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
			e.log.Warn("removing application", "app", name)
			if err := e.rt.Remove(ctx, name); err != nil {
				res.Failed[name] = err
				e.log.Error("could not remove application", "app", name, "error", err)
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
	return res
}

// Err turns a result into the error a command should exit with.
func (r RemoveResult) Err() error {
	if len(r.Failed) > 0 {
		return fmt.Errorf("removal failed for %d application(s)", len(r.Failed))
	}
	return nil
}

func managedNames(actual model.ActualState) []string {
	var out []string
	for _, n := range actual.Names() {
		if actual.Apps[n].Managed {
			out = append(out, n)
		}
	}
	return out
}
