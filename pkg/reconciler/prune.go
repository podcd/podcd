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
	// Networks are the networks removed, after the applications.
	Networks []string
	// Skipped are present on the host but not owned by podcd. Neither command
	// ever touches these - the same refusal the planner gives a reconcile
	// that finds a unit it does not manage.
	Skipped []string
	// NotFound were asked for by name but are not on this host at all.
	NotFound []string
	// Failed is name -> the error removing it hit; a network is keyed as
	// "network <name>". Removal does not stop at the first failure - a
	// cleanup command that gives up after one bad application is worse than
	// one that finishes the rest and reports what did not work.
	Failed map[string]error
}

// Empty reports whether the result changed nothing.
func (r RemoveResult) Empty() bool { return len(r.Removed) == 0 && len(r.Networks) == 0 }

// OK reports whether every requested removal succeeded (Skipped and NotFound
// do not count as failures - they were never going to be removed).
func (r RemoveResult) OK() bool { return len(r.Failed) == 0 }

// RemoveCandidates inspects the host and returns what Remove would act on:
// every managed application and network with all, otherwise the applications
// named. Networks are only ever removed wholesale: one named by hand might
// still carry a pod, and the reconcile loop is the place that knows.
func (e *Engine) RemoveCandidates(ctx context.Context, names []string, all bool) (apps, networks []string, actual model.ActualState, err error) {
	actual, err = e.rt.Inspect(ctx)
	if err != nil {
		return nil, nil, actual, err
	}
	if all {
		return managedNames(actual), managedNetworkNames(actual), actual, nil
	}
	return names, nil, actual, nil
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

	apps, networks, actual, err := e.RemoveCandidates(ctx, names, all)
	if err != nil {
		return RemoveResult{Failed: map[string]error{}}, err
	}
	return e.removeApps(ctx, apps, networks, actual), nil
}

// PruneCandidates consults Git and returns the managed applications and
// networks this host has that the repository no longer declares for it. This
// is the same set a reconcile would delete; here it can be seen, and acted
// on, without also applying every other change the reconcile would make.
func (e *Engine) PruneCandidates(ctx context.Context) (apps, networks []string, actual model.ActualState, err error) {
	desired, _, _, _, err := e.desiredState(ctx)
	if err != nil {
		return nil, nil, model.ActualState{}, err
	}
	actual, err = e.rt.Inspect(ctx)
	if err != nil {
		return nil, nil, actual, err
	}
	wanted := map[string]bool{}
	for _, app := range desired.Applications {
		wanted[app.Name] = true
	}
	for _, n := range managedNames(actual) {
		if !wanted[n] {
			apps = append(apps, n)
		}
	}
	wantedNet := map[string]bool{}
	for _, n := range desired.Networks {
		wantedNet[n.Name] = true
	}
	for _, n := range managedNetworkNames(actual) {
		if !wantedNet[n] {
			networks = append(networks, n)
		}
	}
	return apps, networks, actual, nil
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

	apps, networks, actual, err := e.PruneCandidates(ctx)
	if err != nil {
		return RemoveResult{Failed: map[string]error{}}, err
	}
	return e.removeApps(ctx, apps, networks, actual), nil
}

// removeApps stops and removes each named application that podcd manages,
// then each named network, under a lock the caller already holds. The
// result's error is a summary; per-item failures are in Failed.
func (e *Engine) removeApps(ctx context.Context, targets, networks []string, actual model.ActualState) RemoveResult {
	res := RemoveResult{Failed: map[string]error{}}
	if len(targets) == 0 && len(networks) == 0 {
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

	// Networks after the applications that were on them.
	for _, name := range networks {
		if seen["network "+name] {
			continue
		}
		seen["network "+name] = true
		if net, ok := actual.Networks[name]; !ok || !net.Managed {
			continue
		}
		e.log.Warn("removing network", "network", name)
		if err := e.rt.RemoveNetwork(ctx, name); err != nil {
			res.Failed["network "+name] = err
			e.log.Error("could not remove network", "network", name, "error", err)
			continue
		}
		res.Networks = append(res.Networks, name)
	}

	sort.Strings(res.Removed)
	sort.Strings(res.Networks)
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
		return fmt.Errorf("removal failed for %d item(s)", len(r.Failed))
	}
	return nil
}

func managedNetworkNames(actual model.ActualState) []string {
	var out []string
	for _, n := range actual.NetworkNames() {
		if actual.Networks[n].Managed {
			out = append(out, n)
		}
	}
	return out
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
