package planner

import (
	"strings"
	"testing"

	"github.com/podcd/podcd/pkg/model"
	"github.com/podcd/podcd/pkg/renderer"
)

func rend() *renderer.Renderer { return &renderer.Renderer{UnitDir: "/units", EnvDir: "/env"} }

func app(name, image string) model.Application {
	return model.Application{Name: name, Image: image, RestartPolicy: "always"}
}

func desired(apps ...model.Application) model.DesiredState {
	return model.DesiredState{Host: "vm-1", Applications: apps}
}

// running builds the actual state for an application that is exactly in sync.
func running(t *testing.T, a model.Application) model.ActualApp {
	t.Helper()
	u, err := rend().Render(a)
	if err != nil {
		t.Fatal(err)
	}
	return model.ActualApp{
		Name:         a.Name,
		Managed:      true,
		UnitFile:     u.Path,
		UnitFileHash: model.HashBytes(u.Content),
		UnitContent:  u.Content,
		SpecHash:     u.SpecHash,
		SecretsHash:  u.SecretsHash,
		UnitName:     u.ServiceName,
		UnitState:    model.UnitActive,
	}
}

func actual(apps ...model.ActualApp) model.ActualState {
	s := model.ActualState{Runtime: "podman", Apps: map[string]model.ActualApp{}}
	for _, a := range apps {
		s.Apps[a.Name] = a
	}
	return s
}

func TestCreateWhenMissing(t *testing.T) {
	p, err := Build(desired(app("api", "img@sha256:a")), actual(), rend(), Options{Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.Empty() {
		t.Fatal("a missing application must produce a change")
	}
	if p.Actions[0].Type != model.ActionCreate || p.Actions[0].App != "api" {
		t.Fatalf("got %+v", p.Actions[0])
	}
}

func TestNoOpWhenAlreadyCorrect(t *testing.T) {
	a := app("api", "img@sha256:a")
	p, err := Build(desired(a), actual(running(t, a)), rend(), Options{Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Empty() {
		t.Fatalf("a converged host must plan nothing, got %+v", p.Actions)
	}
	if len(p.Actions) != 1 || p.Actions[0].Type != model.ActionNoOp {
		t.Fatalf("the application should still be reported as a no-op, got %+v", p.Actions)
	}
}

func TestUpdateWhenImageChanges(t *testing.T) {
	before := app("api", "img@sha256:a")
	after := app("api", "img@sha256:b")
	p, err := Build(desired(after), actual(running(t, before)), rend(), Options{Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.Actions[0].Type != model.ActionUpdate {
		t.Fatalf("got %+v", p.Actions[0])
	}
	if p.Actions[0].Reason != "configuration in Git changed" {
		t.Errorf("reason = %q", p.Actions[0].Reason)
	}
	details := strings.Join(p.Actions[0].Details, "\n")
	if !strings.Contains(details, "- Image=img@sha256:a") || !strings.Contains(details, "+ Image=img@sha256:b") {
		t.Errorf("the diff should show the image change, got:\n%s", details)
	}
}

func TestRestartWhenUnitIsNotRunning(t *testing.T) {
	a := app("api", "img@sha256:a")
	cur := running(t, a)
	cur.UnitState = model.UnitFailed
	p, err := Build(desired(a), actual(cur), rend(), Options{Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.Actions[0].Type != model.ActionRestart {
		t.Fatalf("a failed unit must be restarted, got %+v", p.Actions[0])
	}
}

func TestDeleteWhenNoLongerInGit(t *testing.T) {
	old := app("old", "img@sha256:a")
	p, err := Build(desired(), actual(running(t, old)), rend(), Options{Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.Actions[0].Type != model.ActionDelete || !p.Actions[0].Destructive {
		t.Fatalf("an orphan must be deleted and marked destructive, got %+v", p.Actions[0])
	}
	if !p.Destructive() {
		t.Error("the plan should report that it destroys something")
	}
}

func TestPruneDisabledReportsOrphansWithoutDeleting(t *testing.T) {
	old := app("old", "img@sha256:a")
	p, err := Build(desired(), actual(running(t, old)), rend(), Options{Prune: false})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Empty() {
		t.Fatal("pruning is off, so nothing should change")
	}
	if len(p.Actions) != 1 || !strings.Contains(p.Actions[0].Reason, "pruning is disabled") {
		t.Fatalf("the orphan must still be visible, got %+v", p.Actions)
	}
}

func TestUnmanagedUnitIsRefused(t *testing.T) {
	a := app("api", "img@sha256:a")
	cur := running(t, a)
	cur.Managed = false
	_, err := Build(desired(a), actual(cur), rend(), Options{Prune: true})
	if err == nil || !strings.Contains(err.Error(), "not managed by podcd") {
		t.Fatalf("podcd must refuse to take over someone else's unit, got: %v", err)
	}
}

func TestForeignUnmanagedOrphanIsLeftAlone(t *testing.T) {
	p, err := Build(desired(), actual(model.ActualApp{Name: "theirs", Managed: false}), rend(), Options{Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Actions) != 0 {
		t.Fatalf("a unit podcd does not own must not appear in the plan, got %+v", p.Actions)
	}
}

func TestDeletesAreOrderedBeforeCreates(t *testing.T) {
	// Renaming an application must free its host port before the new one binds it.
	old := app("old", "img@sha256:a")
	p, err := Build(desired(app("new", "img@sha256:b")), actual(running(t, old)), rend(), Options{Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Actions) != 2 {
		t.Fatalf("want two actions, got %+v", p.Actions)
	}
	if p.Actions[0].Type != model.ActionDelete || p.Actions[1].Type != model.ActionCreate {
		t.Fatalf("delete must come first, got %s then %s", p.Actions[0].Type, p.Actions[1].Type)
	}
}

func TestSecretRotationPlansAnUpdate(t *testing.T) {
	a := app("api", "img@sha256:a")
	a.SecretEnv = map[string]string{"TOKEN": "old"}
	cur := running(t, a)

	rotated := app("api", "img@sha256:a")
	rotated.SecretEnv = map[string]string{"TOKEN": "new"}

	p, err := Build(desired(rotated), actual(cur), rend(), Options{Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.Empty() || p.Actions[0].Type != model.ActionUpdate {
		t.Fatalf("a rotated secret must be applied, got %+v", p.Actions)
	}
}

func TestPlanIsStableAcrossRuns(t *testing.T) {
	d := desired(app("b", "img@sha256:b"), app("a", "img@sha256:a"))
	var first string
	for i := 0; i < 10; i++ {
		p, err := Build(d, actual(), rend(), Options{Prune: true})
		if err != nil {
			t.Fatal(err)
		}
		var sb strings.Builder
		for _, a := range p.Actions {
			sb.WriteString(string(a.Type) + " " + a.App + ";")
		}
		if i == 0 {
			first = sb.String()
			continue
		}
		if sb.String() != first {
			t.Fatalf("plan order is not stable: %q vs %q", first, sb.String())
		}
	}
}
