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
	e := &Engine{log: slog.New(slog.NewTextHandler(&buf, nil))} // info and up
	idleAt := func(rev string) Result {
		return Result{Desired: model.DesiredState{Revisions: map[string]string{"main": rev}}}
	}
	applied := Result{Applied: []model.Action{{}}}

	idle := ""
	for i, step := range []struct {
		res  Result
		want string // substring of the info line, "" = none
	}{
		{idleAt("aaa"), "nothing to do"},
		{idleAt("aaa"), ""},
		{idleAt("bbb"), "nothing to do"},
		{idleAt("bbb"), ""},
		{applied, "reconciled"},
		{idleAt("bbb"), "nothing to do"},
	} {
		buf.Reset()
		idle = logReconciled(e, step.res, idle)
		if got := buf.String(); (step.want == "") != (got == "") || !strings.Contains(got, step.want) {
			t.Fatalf("step %d: want %q, logged %q", i, step.want, got)
		}
	}
}
