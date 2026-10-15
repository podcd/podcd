package reconciler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"time"
)

// Run reconciles until the context is cancelled; failures are retried, never fatal.
func (e *Engine) Run(ctx context.Context) error {
	e.log.Info("agent starting",
		"host", e.ident.Host,
		"identity_source", e.ident.Source,
		"runtime", e.rt.Name(),
		"interval", e.cfg.Interval.String(),
		"unit_dir", e.cfg.UnitDir,
		"state", e.store.Path(),
	)
	if ok, why := e.rt.Available(ctx); !ok {
		// Keep going; the runtime may still be installing.
		e.log.Error("container runtime is not usable yet", "runtime", e.rt.Name(), "reason", why)
	}

	backoff := e.cfg.RetryInterval
	var lastImagePrune time.Time
	var idle idleLog // reset by a failure or a change
	for {
		res, err := e.Reconcile(ctx, Options{})
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			idle = idleLog{}
			e.log.Error("reconcile failed", "error", err, "retry_in", backoff.String())
			if !sleepCtx(ctx, backoff) {
				break
			}
			backoff = nextBackoff(backoff, e.cfg.MaxRetryInterval)
			continue
		}

		backoff = e.cfg.RetryInterval
		idle = logReconciled(ctx, e, res, idle)

		// Only after success: every desired app is running, so its image is in use.
		if e.cfg.ImagePrune > 0 && time.Since(lastImagePrune) >= e.cfg.ImagePrune {
			if err := e.pruneImages(ctx); err != nil {
				e.log.Warn("image prune failed", "error", err)
			} else {
				lastImagePrune = time.Now()
			}
		}

		if !sleepCtx(ctx, withJitter(e.cfg.Interval, e.cfg.Jitter)) {
			break
		}
	}

	e.log.Info("agent stopping", "reason", context.Cause(ctx))
	if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// pruneImages holds the reconcile lock so it cannot race a pull.
func (e *Engine) pruneImages(ctx context.Context) error {
	lock, err := Acquire(e.cfg.LockPath())
	if err != nil {
		return err
	}
	defer lock.Release()
	n, err := e.rt.PruneImages(ctx)
	if err != nil {
		return err
	}
	e.log.Info("pruned unused images", "removed", n)
	return nil
}

// idleHeartbeat is how often a steady idle agent still says so at info, as a sign of life.
const idleHeartbeat = time.Hour

// idleLog is the last "nothing to do" logged at info; the zero value forces the next one.
type idleLog struct {
	key string
	at  time.Time
}

// logReconciled reports an idle pass at info once, then at debug until the revisions, the
// application count or the offline repositories change, something is applied, a reconcile
// fails or idleHeartbeat passes. It returns the new idle state.
func logReconciled(ctx context.Context, e *Engine, res Result, idle idleLog) idleLog {
	rev := res.Desired.RevisionString()
	took := res.Elapsed.Round(time.Millisecond).String()
	if len(res.Applied) > 0 {
		e.log.Info("reconciled",
			"revision", rev,
			"changes", len(res.Applied),
			"actions", describeActions(res.Applied),
			"took", took,
		)
		return idleLog{}
	}
	args := []any{"revision", rev, "applications", len(res.Desired.Applications), "took", took}
	if len(res.Offline) > 0 {
		args = append(args, "offline", res.Offline)
	}
	// Full shas and quoted names, so no two different states share a key.
	key := fmt.Sprintf("%q|%d|%q", res.Desired.Revisions, len(res.Desired.Applications), res.Offline)
	if key == idle.key && time.Since(idle.at) < idleHeartbeat {
		e.log.Log(ctx, slog.LevelDebug, "nothing to do", args...)
		return idle
	}
	e.log.Log(ctx, slog.LevelInfo, "nothing to do", args...)
	return idleLog{key: key, at: time.Now()}
}

// sleepCtx waits for d, returning false if the context ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// withJitter adds a random delay up to jitter.
func withJitter(interval, jitter time.Duration) time.Duration {
	if jitter <= 0 {
		return interval
	}
	return interval + time.Duration(rand.Int63n(int64(jitter)))
}

func nextBackoff(current, max time.Duration) time.Duration {
	next := current * 2
	if next > max {
		return max
	}
	return next
}
