package reconciler

import (
	"context"
	"errors"
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
	for {
		res, err := e.Reconcile(ctx, Options{})
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			e.log.Error("reconcile failed", "error", err, "retry_in", backoff.String())
			if !sleepCtx(ctx, backoff) {
				break
			}
			backoff = nextBackoff(backoff, e.cfg.MaxRetryInterval)
			continue
		}

		backoff = e.cfg.RetryInterval
		logReconciled(e, res)

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

func logReconciled(e *Engine, res Result) {
	if len(res.Applied) == 0 {
		e.log.Info("nothing to do", "revision", res.Desired.RevisionString(),
			"applications", len(res.Desired.Applications), "took", res.Elapsed.Round(time.Millisecond).String())
		return
	}
	e.log.Info("reconciled",
		"revision", res.Desired.RevisionString(),
		"changes", len(res.Applied),
		"actions", describeActions(res.Applied),
		"took", res.Elapsed.Round(time.Millisecond).String(),
	)
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
