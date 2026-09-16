package podman

import (
	"testing"
	"time"

	"github.com/podcd/podcd/pkg/model"
)

func TestHealthForContainersIgnoresHealthcheckState(t *testing.T) {
	got := healthForContainers("vault-dev", []containerInfo{
		{name: "vault-dev-vault", state: "running"},
	}, time.Now())

	if got.Status != model.HealthHealthy {
		t.Fatalf("status = %q, want %q (%s)", got.Status, model.HealthHealthy, got.Message)
	}
}

func TestHealthForContainersRequiresAllContainersRunning(t *testing.T) {
	got := healthForContainers("vault-dev", []containerInfo{
		{name: "vault-dev-vault", state: "running"},
		{name: "vault-dev-agent", state: "exited"},
	}, time.Now())

	if got.Status != model.HealthUnhealthy {
		t.Fatalf("status = %q, want %q", got.Status, model.HealthUnhealthy)
	}
}
