// Package reconciler is the agent: it loads the desired state, observes the
// host, plans, applies, checks health and records what happened.
package reconciler

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/podcd/podcd/pkg/config"
	"github.com/podcd/podcd/pkg/identity"
	"github.com/podcd/podcd/pkg/model"
	"github.com/podcd/podcd/pkg/planner"
	"github.com/podcd/podcd/pkg/renderer"
	"github.com/podcd/podcd/pkg/runtime"
	"github.com/podcd/podcd/pkg/runtime/podman"
	"github.com/podcd/podcd/pkg/secrets"
	"github.com/podcd/podcd/pkg/state"
)

// healthWaiter is implemented by runtimes that can wait for an application to
// become healthy after a change.
type healthWaiter interface {
	WaitHealthy(ctx context.Context, app model.Application) model.Health
}

// Engine wires the pieces together and is shared by the agent and the CLI, so
// `podcd plan` and the loop can never disagree about what would happen.
type Engine struct {
	cfg    config.AgentConfig
	ident  identity.Identity
	rt     runtime.Runtime
	rend   *renderer.Renderer
	store  state.Store
	source *Source
	log    *slog.Logger
}

// NewEngine builds an engine from the agent configuration.
func NewEngine(cfg config.AgentConfig, log *slog.Logger) (*Engine, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	ident, err := identity.Resolve(cfg.Host)
	if err != nil {
		return nil, err
	}

	var rt runtime.Runtime
	var rend *renderer.Renderer
	switch cfg.Runtime {
	case "podman":
		p := podman.New(podman.Options{UnitDir: cfg.UnitDir, EnvDir: cfg.SecretEnvDir(), KubeDir: cfg.KubeDir()})
		rt, rend = p, p.Renderer()
	default:
		return nil, fmt.Errorf("runtime %q is not implemented; the MVP supports rootless podman", cfg.Runtime)
	}

	e := &Engine{
		cfg:   cfg,
		ident: ident,
		rt:    rt,
		rend:  rend,
		store: state.NewFileStore(cfg.StatePath()),
		log:   log,
	}
	e.source = &Source{
		Repos:   ReposFromConfig(cfg),
		Host:    ident.Host,
		Secrets: secrets.Default(cfg.SecretsDir),
		Log:     log,
	}
	return e, nil
}

// Config returns the agent configuration in use.
func (e *Engine) Config() config.AgentConfig { return e.cfg }

// Identity returns the resolved host identity.
func (e *Engine) Identity() identity.Identity { return e.ident }

// Runtime returns the container runtime in use.
func (e *Engine) Runtime() runtime.Runtime { return e.rt }

// Store returns the local metadata store.
func (e *Engine) Store() state.Store { return e.store }

// Options controls one reconcile.
type Options struct {
	// DryRun plans without changing anything.
	DryRun bool
	// Prune overrides the configured prune policy when non-nil.
	Prune *bool
	// SkipHealth skips the post-apply health wait. Used by `podcd plan`.
	SkipHealth bool
}

// Result is everything one reconcile learned and did.
type Result struct {
	Desired model.DesiredState
	Actual  model.ActualState
	Plan    model.Plan
	Applied []model.Action
	Health  []model.Health
	Offline []string
	Started time.Time
	Elapsed time.Duration
}

// Plan loads Git, observes the host and returns what would change.
// It makes no changes of its own.
func (e *Engine) Plan(ctx context.Context) (Result, error) {
	var res Result
	res.Started = time.Now()

	load, err := e.source.LoadDesiredState(ctx)
	if err != nil {
		return res, err
	}
	res.Desired = load.Desired
	res.Offline = load.Offline

	actual, err := e.rt.Inspect(ctx)
	if err != nil {
		return res, err
	}
	res.Actual = actual

	plan, err := planner.Build(load.Desired, actual, e.rend, planner.Options{Prune: e.prune(nil)})
	if err != nil {
		return res, err
	}
	res.Plan = plan
	res.Elapsed = time.Since(res.Started)
	return res, nil
}

// Reconcile makes the host match Git, then checks that the result works.
//
// The lock is held for the whole apply, so a human running `podcd reconcile`
// and the agent's own loop cannot fight over the same unit files.
func (e *Engine) Reconcile(ctx context.Context, opts Options) (Result, error) {
	started := time.Now()
	res := Result{Started: started}

	lock, err := Acquire(e.cfg.LockPath())
	if err != nil {
		return res, err
	}
	defer lock.Release()

	st, err := e.store.Load()
	if err != nil {
		e.log.Warn("local state could not be read", "error", err)
	}
	st.Host = e.ident.Host
	st.MachineID = e.ident.MachineID

	load, loadErr := e.source.LoadDesiredState(ctx)
	if loadErr != nil {
		e.recordFailure(&st, started, nil, loadErr)
		return res, loadErr
	}
	res.Desired = load.Desired
	res.Offline = load.Offline

	for _, app := range load.Desired.Applications {
		if app.AllowMutableImage {
			e.log.Warn("application uses a mutable image reference; a Git commit no longer identifies what is deployed",
				"app", app.Name, "image", app.Image)
		}
	}

	actual, err := e.rt.Inspect(ctx)
	if err != nil {
		e.recordFailure(&st, started, load.Desired.Revisions, err)
		return res, err
	}
	res.Actual = actual

	plan, err := planner.Build(load.Desired, actual, e.rend, planner.Options{Prune: e.prune(opts.Prune)})
	if err != nil {
		e.recordFailure(&st, started, load.Desired.Revisions, err)
		return res, err
	}
	res.Plan = plan

	if opts.DryRun {
		res.Elapsed = time.Since(started)
		return res, nil
	}

	applied, applyErr := e.apply(ctx, plan, load.Desired, &st)
	res.Applied = applied
	if applyErr != nil {
		e.recordFailure(&st, started, load.Desired.Revisions, applyErr)
		return res, applyErr
	}

	if !opts.SkipHealth {
		health, unhealthy := e.checkHealth(ctx, load.Desired, applied, &st)
		res.Health = health
		if unhealthy != nil {
			e.recordFailure(&st, started, load.Desired.Revisions, unhealthy)
			return res, unhealthy
		}
	}

	res.Elapsed = time.Since(started)
	e.recordSuccess(&st, started, load.Desired.Revisions, applied, res.Elapsed)
	return res, nil
}

// apply executes the plan, one action at a time, loudly.
func (e *Engine) apply(ctx context.Context, plan model.Plan, desired model.DesiredState, st *state.State) ([]model.Action, error) {
	var applied []model.Action
	rev := desired.RevisionString()

	for _, action := range plan.Actions {
		if action.Type == model.ActionNoOp {
			continue
		}
		if err := ctx.Err(); err != nil {
			return applied, err
		}

		switch action.Type {
		case model.ActionDelete:
			// Say it before doing it: a destructive change must never be a
			// surprise found later in a journal.
			e.log.Warn("removing application", "app", action.App, "reason", action.Reason)
			if err := e.rt.Remove(ctx, action.App); err != nil {
				return applied, fmt.Errorf("removing %s: %w", action.App, err)
			}
			st.Forget(action.App)

		case model.ActionCreate, model.ActionUpdate:
			e.log.Info(string(action.Type)+" application", "app", action.App, "reason", action.Reason,
				"image", action.Application.Image)
			for _, d := range action.Details {
				e.log.Debug("change", "app", action.App, "detail", d)
			}
			if err := e.rt.Apply(ctx, *action.Application); err != nil {
				return applied, fmt.Errorf("applying %s: %w", action.App, err)
			}
			st.RecordApplied(*action.Application, rev, time.Now())

		case model.ActionRestart:
			e.log.Info("restarting application", "app", action.App, "reason", action.Reason)
			if err := e.rt.Restart(ctx, action.App); err != nil {
				return applied, fmt.Errorf("restarting %s: %w", action.App, err)
			}
		}
		applied = append(applied, action)
	}
	return applied, nil
}

// checkHealth probes every desired application, waiting only on the ones that
// just changed.
func (e *Engine) checkHealth(ctx context.Context, desired model.DesiredState, applied []model.Action, st *state.State) ([]model.Health, error) {
	changed := map[string]bool{}
	for _, a := range applied {
		changed[a.App] = true
	}
	waiter, canWait := e.rt.(healthWaiter)

	var results []model.Health
	var bad []string
	for _, app := range desired.Applications {
		var h model.Health
		if changed[app.Name] && canWait {
			h = waiter.WaitHealthy(ctx, app)
		} else {
			var err error
			h, err = e.rt.Health(ctx, app)
			if err != nil {
				h = model.Health{App: app.Name, Status: model.HealthUnknown, Message: err.Error(), CheckedAt: time.Now().UTC()}
			}
		}
		st.RecordHealth(h)
		results = append(results, h)
		if !h.OK() {
			bad = append(bad, fmt.Sprintf("%s (%s: %s)", app.Name, h.Status, h.Message))
			e.log.Error("application is not healthy", "app", app.Name, "status", string(h.Status), "detail", h.Message)
		} else {
			e.log.Debug("application is healthy", "app", app.Name, "probe", h.Probe, "detail", h.Message)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return results, fmt.Errorf("unhealthy after reconcile: %s", strings.Join(bad, ", "))
	}
	return results, nil
}

// Health probes the desired applications without changing anything.
func (e *Engine) Health(ctx context.Context) ([]model.Health, error) {
	load, err := e.source.LoadDesiredState(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.Health
	for _, app := range load.Desired.Applications {
		h, err := e.rt.Health(ctx, app)
		if err != nil {
			h = model.Health{App: app.Name, Status: model.HealthUnknown, Message: err.Error(), CheckedAt: time.Now().UTC()}
		}
		out = append(out, h)
	}
	return out, nil
}

// Status is what `podcd status` reports: local, fast, and it works offline.
type Status struct {
	Identity  identity.Identity       `json:"identity"`
	Config    string                  `json:"configPath"`
	Runtime   string                  `json:"runtime"`
	Available bool                    `json:"runtimeAvailable"`
	Why       string                  `json:"runtimeUnavailableReason,omitempty"`
	State     state.State             `json:"state"`
	Actual    model.ActualState       `json:"actual"`
	Repos     []config.RepositorySpec `json:"repositories"`
}

// Status reports what the agent knows without contacting Git.
func (e *Engine) Status(ctx context.Context) (Status, error) {
	st, err := e.store.Load()
	if err != nil {
		e.log.Warn("local state could not be read", "error", err)
	}
	available, why := e.rt.Available(ctx)
	s := Status{
		Identity:  e.ident,
		Config:    e.cfg.Path,
		Runtime:   e.rt.Name(),
		Available: available,
		Why:       why,
		State:     st,
		Repos:     e.cfg.Repositories,
	}
	actual, err := e.rt.Inspect(ctx)
	if err != nil {
		return s, err
	}
	s.Actual = actual
	return s, nil
}

// Logs returns recent output for one application.
func (e *Engine) Logs(ctx context.Context, app string, lines int) (string, error) {
	return e.rt.Logs(ctx, app, lines)
}

func (e *Engine) prune(override *bool) bool {
	if override != nil {
		return *override
	}
	return e.cfg.PruneEnabled()
}

func (e *Engine) recordSuccess(st *state.State, started time.Time, revs map[string]string, applied []model.Action, elapsed time.Duration) {
	att := &state.Attempt{
		At:        started.UTC(),
		Revisions: revs,
		Actions:   describeActions(applied),
		Duration:  elapsed.Round(time.Millisecond).String(),
	}
	st.LastAttempt = att
	st.LastSuccess = att
	st.FailureCount = 0
	e.save(st)
}

func (e *Engine) recordFailure(st *state.State, started time.Time, revs map[string]string, cause error) {
	att := &state.Attempt{
		At:        started.UTC(),
		Revisions: revs,
		Error:     cause.Error(),
		Duration:  time.Since(started).Round(time.Millisecond).String(),
	}
	st.LastAttempt = att
	st.LastFailure = att
	st.FailureCount++
	e.save(st)
}

func (e *Engine) save(st *state.State) {
	if err := e.store.Save(*st); err != nil {
		e.log.Error("could not write local state", "error", err, "path", e.store.Path())
	}
}

func describeActions(actions []model.Action) []string {
	out := make([]string, 0, len(actions))
	for _, a := range actions {
		out = append(out, string(a.Type)+" "+a.App)
	}
	return out
}
