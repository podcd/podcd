package reconciler

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/podcd/podcd/pkg/model"
)

const (
	idleInfo  = `level=INFO msg="nothing to do"`
	idleDebug = `level=DEBUG msg="nothing to do"`
)

func TestSteadyIdleDropsToDebugUntilSomethingChanges(t *testing.T) {
	var buf bytes.Buffer
	e := &Engine{log: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	idleAt := func(rev string, offline ...string) Result {
		return Result{Desired: model.DesiredState{Revisions: map[string]string{"main": rev}}, Offline: offline}
	}
	applied := Result{Applied: []model.Action{{}}}

	var idle idleLog
	for i, step := range []struct {
		res  Result
		aged bool // the last info line is older than idleHeartbeat
		want string
	}{
		{idleAt("aaa"), false, idleInfo},
		{idleAt("aaa"), false, idleDebug},
		{idleAt("aaa"), true, idleInfo}, // heartbeat
		{idleAt("aaa"), false, idleDebug},
		{idleAt("bbb"), false, idleInfo},
		{idleAt("bbb"), false, idleDebug},
		{applied, false, "level=INFO msg=reconciled"},
		{idleAt("bbb"), false, idleInfo},
		{idleAt("bbb", "main"), false, idleInfo}, // remote went offline
		{idleAt("bbb", "main"), false, idleDebug},
		{idleAt("bbb"), false, idleInfo}, // and came back at the same commit
		{idleAt("0123456789ab-1"), false, idleInfo},
		{idleAt("0123456789ab-2"), false, idleInfo}, // same short sha, different commit
	} {
		if step.aged {
			idle.at = idle.at.Add(-idleHeartbeat)
		}
		buf.Reset()
		idle = logReconciled(context.Background(), e, step.res, idle)
		if got := buf.String(); !strings.Contains(got, step.want) {
			t.Fatalf("step %d: want %q, logged %q", i, step.want, got)
		}
	}
}

// syncBuffer lets the test read what the Run goroutine logs.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) count(s string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Count(b.buf.String(), s)
}

func TestRunLoopLogsRecoveryAtInfoAfterAFailure(t *testing.T) {
	rt := newFakeRuntime()
	e, _ := newTestEngine(t, rt, twoApps)
	e.cfg.Interval = 5 * time.Millisecond
	e.cfg.Jitter = 0
	e.cfg.RetryInterval = 5 * time.Millisecond
	var logs syncBuffer
	e.log = slog.New(slog.NewTextHandler(&logs, nil))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	defer func() { cancel(); <-done }()

	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for !ok() {
			select {
			case <-deadline:
				t.Fatalf("timed out waiting for %s", what)
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	setInspectErr := func(err error) {
		rt.mu.Lock()
		rt.inspectErr = err
		rt.mu.Unlock()
	}

	waitFor("a first idle pass", func() bool { return logs.count(idleInfo) == 1 })
	setInspectErr(errors.New("runtime is down"))
	waitFor("a failed pass", func() bool { return logs.count(`msg="reconcile failed"`) > 0 })
	setInspectErr(nil)
	waitFor("recovery at info, same revision", func() bool { return logs.count(idleInfo) == 2 })
}

func TestOfflineRemoteIsWarnedAboutOncePerOutage(t *testing.T) {
	rt := newFakeRuntime()
	e, repoDir := newTestEngine(t, rt, twoApps)
	var logs bytes.Buffer
	e.source.Log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	reconcile(t, e)

	gone := repoDir + ".gone"
	if err := os.Rename(repoDir, gone); err != nil {
		t.Fatal(err)
	}
	reconcile(t, e)
	reconcile(t, e)
	if err := os.Rename(gone, repoDir); err != nil {
		t.Fatal(err)
	}
	reconcile(t, e)
	reconcile(t, e)

	got := logs.String()
	for line, want := range map[string]int{
		`level=WARN msg="git remote unreachable`:      1,
		`level=DEBUG msg="git remote unreachable`:     1,
		`level=INFO msg="git remote reachable again"`: 1,
	} {
		if n := strings.Count(got, line); n != want {
			t.Errorf("%s: logged %d times, want %d\n%s", line, n, want, got)
		}
	}
}
