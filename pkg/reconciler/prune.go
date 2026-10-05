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
	// Skipped are on the host but not managed by podcd; never touched.
	Skipped []string
	// NotFound were asked for by name but are not on this host at all.
	NotFound []string
	// Failed maps name (networks as "network <name>") to its error; removal continues past failures.
	Failed map[string]error
}

// Empty reports whether the result changed nothing.
func (r RemoveResult) Empty() bool { return len(r.Removed) == 0 && len(r.Networks) == 0 }

// OK reports whether nothing failed.
func (r RemoveResult) OK() bool { return len(r.Failed) == 0 }

// RemoveCandidates returns what Remove would act on: everything managed with
// all, otherwise the named applications (networks only with all).
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

// Remove stops and removes managed applications without consulting Git.
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

// PruneCandidates returns what a reconcile would delete: managed items Git no longer declares.
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

// Prune removes only what podcd manages but Git no longer declares.
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

// removeApps removes the named managed applications, then networks; caller holds the lock.
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
