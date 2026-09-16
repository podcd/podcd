package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/podcd/podcd/pkg/model"
)

func TestLoadMissingFileIsEmptyNotAnError(t *testing.T) {
	s := NewFileStore(filepath.Join(t.TempDir(), "state.json"))
	got, err := s.Load()
	if err != nil {
		t.Fatalf("a first run must not fail: %v", err)
	}
	if len(got.Applications) != 0 {
		t.Fatal("a fresh state should be empty")
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewFileStore(path)

	st, _ := store.Load()
	st.Host = "prod-web-01"
	st.RecordApplied(model.Application{Name: "api", Image: "img@sha256:a"}, "infra=abc123", time.Now())
	st.LastSuccess = &Attempt{At: time.Now().UTC(), Actions: []string{"create api"}}
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}

	back, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if back.Host != "prod-web-01" {
		t.Errorf("host = %q", back.Host)
	}
	rec := back.Applications["api"]
	if rec.Image != "img@sha256:a" || rec.SpecHash == "" || rec.Revision != "infra=abc123" {
		t.Errorf("application record did not survive: %+v", rec)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("state file mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestPreviousDeploymentIsKept(t *testing.T) {
	st := State{Applications: map[string]AppRecord{}}
	now := time.Now()
	st.RecordApplied(model.Application{Name: "api", Image: "img@sha256:old"}, "infra=aaa", now)
	st.RecordApplied(model.Application{Name: "api", Image: "img@sha256:new"}, "infra=bbb", now)

	rec := st.Applications["api"]
	if rec.Image != "img@sha256:new" {
		t.Errorf("current image = %q", rec.Image)
	}
	if rec.PreviousImage != "img@sha256:old" || rec.PreviousRevision != "infra=aaa" {
		t.Errorf("the previous deployment was not kept: %+v", rec)
	}
}

func TestUnchangedApplicationDoesNotOverwriteHistory(t *testing.T) {
	st := State{Applications: map[string]AppRecord{}}
	app := model.Application{Name: "api", Image: "img@sha256:a"}
	st.RecordApplied(app, "infra=aaa", time.Now())
	st.RecordApplied(app, "infra=bbb", time.Now())
	if st.Applications["api"].PreviousImage != "" {
		t.Error("re-applying the same spec must not invent a previous deployment")
	}
}

func TestCorruptStateStartsFreshAndSaysSo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewFileStore(path)
	st, err := store.Load()
	if err == nil {
		t.Fatal("a corrupt state file must be reported")
	}
	if st.Applications == nil {
		t.Fatal("the agent must still be able to carry on with an empty state")
	}
}


func TestHealthIsRecorded(t *testing.T) {
	st := State{Applications: map[string]AppRecord{}}
	st.RecordHealth(model.Health{App: "api", Status: model.HealthHealthy, Message: "GET /health: 200", CheckedAt: time.Now()})
	if st.Applications["api"].Health != "healthy" {
		t.Fatalf("health was not recorded: %+v", st.Applications["api"])
	}
}
