package reconciler

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/podcd/podcd/pkg/model"
)

func TestSteadyIdleDropsToDebugUntilSomethingChanges(t *testing.T) {
	var buf bytes.Buffer
	e := &Engine{log: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	idleAt := func(rev string, offline ...string) Result {
		return Result{Desired: model.DesiredState{Revisions: map[string]string{"main": rev}}, Offline: offline}
	}
	applied := Result{Applied: []model.Action{{}}}
	const (
		idleInfo  = `level=INFO msg="nothing to do"`
		idleDebug = `level=DEBUG msg="nothing to do"`
	)

	idle := ""
	for i, step := range []struct {
		res  Result
		want string
	}{
		{idleAt("aaa"), idleInfo},
		{idleAt("aaa"), idleDebug},
		{idleAt("bbb"), idleInfo},
		{idleAt("bbb"), idleDebug},
		{applied, "level=INFO msg=reconciled"},
		{idleAt("bbb"), idleInfo},
		{idleAt("bbb", "main"), idleInfo}, // remote went offline
		{idleAt("bbb", "main"), idleDebug},
		{idleAt("bbb"), idleInfo}, // and came back at the same commit
	} {
		buf.Reset()
		idle = logReconciled(e, step.res, idle)
		if got := buf.String(); !strings.Contains(got, step.want) {
			t.Fatalf("step %d: want %q, logged %q", i, step.want, got)
		}
	}
}
