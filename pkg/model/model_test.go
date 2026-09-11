package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSpecHashIgnoresMapOrderAndProvenance(t *testing.T) {
	a := Application{
		Name:  "api",
		Image: "img@sha256:a",
		Env:   map[string]string{"A": "1", "B": "2"},
	}
	b := Application{
		Name:       "api",
		Image:      "img@sha256:a",
		Env:        map[string]string{"B": "2", "A": "1"},
		SourceRepo: "somewhere-else",
		Origins:    []string{"group/web"},
	}
	if a.SpecHash() != b.SpecHash() {
		t.Fatal("the hash must not depend on map order or on where the YAML lived")
	}
}

func TestSpecHashChangesWithTheSpec(t *testing.T) {
	a := Application{Name: "api", Image: "img@sha256:a"}
	b := Application{Name: "api", Image: "img@sha256:b"}
	if a.SpecHash() == b.SpecHash() {
		t.Fatal("a different image must produce a different hash")
	}
}

func TestSpecHashCoversSecretValues(t *testing.T) {
	a := Application{Name: "api", Image: "img@sha256:a", SecretEnv: map[string]string{"T": "old"}}
	b := Application{Name: "api", Image: "img@sha256:a", SecretEnv: map[string]string{"T": "new"}}
	if a.SpecHash() == b.SpecHash() {
		t.Fatal("rotating a secret must change the hash, otherwise nothing ever restarts")
	}
}

func TestSecretsAreRedactedInJSON(t *testing.T) {
	a := Application{Name: "api", Image: "img@sha256:a", SecretEnv: map[string]string{"TOKEN": "s3cret"}}
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "s3cret") {
		t.Fatalf("secret leaked: %s", data)
	}
	if !strings.Contains(string(data), "TOKEN") {
		t.Error("the name of a secret is not itself a secret and should still be visible")
	}
}

func TestPlanEmptyIgnoresNoOps(t *testing.T) {
	p := Plan{Actions: []Action{{Type: ActionNoOp, App: "api"}}}
	if !p.Empty() {
		t.Fatal("a plan of no-ops changes nothing")
	}
	p.Actions = append(p.Actions, Action{Type: ActionCreate, App: "web"})
	if p.Empty() {
		t.Fatal("a create is a change")
	}
	if len(p.Changes()) != 1 {
		t.Fatalf("Changes() should drop no-ops, got %d", len(p.Changes()))
	}
}

func TestRevisionStringIsStable(t *testing.T) {
	d := DesiredState{Revisions: map[string]string{
		"b-repo": "1234567890abcdef",
		"a-repo": "abcdef1234567890",
	}}
	want := "a-repo=abcdef123456 b-repo=1234567890ab"
	for i := 0; i < 5; i++ {
		if got := d.RevisionString(); got != want {
			t.Fatalf("RevisionString() = %q, want %q", got, want)
		}
	}
}
