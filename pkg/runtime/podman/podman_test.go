package podman

import (
	"testing"
	"time"

	"github.com/podcd/podcd/pkg/model"
)

func TestHealthForContainersIgnoresHealthcheckState(t *testing.T) {
	got := healthForContainers("vault-dev", []containerInfo{
		{name: "vault-dev-vault", state: "running"},
	}, nil, time.Now())

	if got.Status != model.HealthHealthy {
		t.Fatalf("status = %q, want %q (%s)", got.Status, model.HealthHealthy, got.Message)
	}
}

func TestHealthForContainersRequiresAllContainersRunning(t *testing.T) {
	got := healthForContainers("vault-dev", []containerInfo{
		{name: "vault-dev-vault", state: "running"},
		{name: "vault-dev-agent", state: "exited"},
	}, nil, time.Now())

	if got.Status != model.HealthUnhealthy {
		t.Fatalf("status = %q, want %q", got.Status, model.HealthUnhealthy)
	}
}

func TestHealthForContainersAcceptsCompletedInitContainer(t *testing.T) {
	got := healthForContainers("vault-dev", []containerInfo{
		{name: "vault-dev-seed", state: "exited", exitCode: 0},
		{name: "vault-dev-vault", state: "running"},
	}, []string{"seed"}, time.Now())

	if got.Status != model.HealthHealthy {
		t.Fatalf("status = %q, want %q (%s)", got.Status, model.HealthHealthy, got.Message)
	}
}

func TestHealthForContainersRejectsFailedInitContainer(t *testing.T) {
	got := healthForContainers("vault-dev", []containerInfo{
		{name: "vault-dev-seed", state: "exited", exitCode: 1},
		{name: "vault-dev-vault", state: "running"},
	}, []string{"seed"}, time.Now())

	if got.Status != model.HealthUnhealthy {
		t.Fatalf("status = %q, want %q (%s)", got.Status, model.HealthUnhealthy, got.Message)
	}
}

func TestHealthForContainersWaitsForInitContainer(t *testing.T) {
	got := healthForContainers("vault-dev", []containerInfo{
		{name: "vault-dev-vault", state: "running"},
	}, []string{"seed"}, time.Now())

	if got.Status != model.HealthUnknown {
		t.Fatalf("status = %q, want %q (%s)", got.Status, model.HealthUnknown, got.Message)
	}
}
