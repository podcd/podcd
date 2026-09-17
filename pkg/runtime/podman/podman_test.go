package podman

import (
	"strings"
	"testing"
	"time"

	"github.com/podcd/podcd/pkg/model"
)

// A running container with no healthcheck is simply healthy: there is no
// verdict to defer to.
func TestHealthForContainersRunningWithoutACheckIsHealthy(t *testing.T) {
	got := healthForContainers("vault-dev", []containerInfo{
		{name: "vault-dev-vault", state: "running"},
	}, nil, time.Now())

	if got.Status != model.HealthHealthy {
		t.Fatalf("status = %q, want %q (%s)", got.Status, model.HealthHealthy, got.Message)
	}
}

// When the workload declares a healthcheck, podman's verdict is the answer.
func TestHealthForContainersHonoursPodmansVerdict(t *testing.T) {
	for _, tc := range []struct {
		health string
		want   model.HealthStatus
	}{
		{"healthy", model.HealthHealthy},
		{"starting", model.HealthUnknown},
		{"unhealthy", model.HealthUnhealthy},
	} {
		got := healthForContainers("vault-dev", []containerInfo{
			{name: "vault-dev-vault", state: "running", health: tc.health},
		}, nil, time.Now())
		if got.Status != tc.want {
			t.Errorf("health %q: status = %q, want %q (%s)", tc.health, got.Status, tc.want, got.Message)
		}
	}
}

// "starting" on a container that has already restarted is a crash loop: a
// liveness probe that never passes restarts the container before it can ever
// become "unhealthy". The count is what tells the two apart.
func TestHealthForContainersReportsRestartsSoACrashLoopIsVisible(t *testing.T) {
	got := healthForContainers("vault-dev", []containerInfo{
		{name: "vault-dev-vault", state: "running", health: "starting", restarts: 165},
	}, nil, time.Now())

	if got.Status != model.HealthUnknown {
		t.Fatalf("status = %q, want %q", got.Status, model.HealthUnknown)
	}
	for _, want := range []string{"not passed yet", "vault-dev-vault restarted 165 times"} {
		if !strings.Contains(got.Message, want) {
			t.Errorf("message should mention %q: %q", want, got.Message)
		}
	}
}

func TestHealthFromStatusParsesTheVerdict(t *testing.T) {
	for status, want := range map[string]string{
		"Up 2 minutes (healthy)":   "healthy",
		"Up 5 seconds (starting)":  "starting",
		"Up 1 hour (unhealthy)":    "unhealthy",
		"Up 2 minutes":             "",
		"Exited (0) 3 minutes ago": "",
	} {
		if got := healthFromStatus(status); got != want {
			t.Errorf("%q -> %q, want %q", status, got, want)
		}
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

// An init container podman still shows as running is not finished; the
// workload behind it has not been started yet either.
func TestHealthForContainersWaitsForInitContainer(t *testing.T) {
	got := healthForContainers("vault-dev", []containerInfo{
		{name: "vault-dev-seed", state: "running"},
		{name: "vault-dev-vault", state: "created"},
	}, []string{"seed"}, time.Now())

	if got.Status != model.HealthUnknown {
		t.Fatalf("status = %q, want %q (%s)", got.Status, model.HealthUnknown, got.Message)
	}
}

// kube play creates init containers as type "once" and removes them when
// they finish, so one that is no longer listed has completed. Reporting it as
// still running would keep a healthy pod failing forever.
func TestHealthForContainersTreatsVanishedInitContainerAsDone(t *testing.T) {
	got := healthForContainers("vault-dev", []containerInfo{
		{name: "vault-dev-vault", state: "running"},
	}, []string{"seed"}, time.Now())

	if got.Status != model.HealthHealthy {
		t.Fatalf("status = %q, want %q (%s)", got.Status, model.HealthHealthy, got.Message)
	}
}
