package health

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/podcd/podcd/pkg/model"
)

func TestNoHealthcheckFallsBackToSystemd(t *testing.T) {
	c := &Checker{}
	h := c.Check(context.Background(), model.Application{Name: "api"}, true)
	if !h.OK() || h.Probe != "systemd" {
		t.Fatalf("an active unit with no probe should be healthy, got %+v", h)
	}

	h = c.Check(context.Background(), model.Application{Name: "api"}, false)
	if h.OK() {
		t.Fatal("an inactive unit is never healthy")
	}
}

func TestStoppedUnitIsUnhealthyEvenIfTheProbeWouldPass(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	host, port := splitHostPort(t, srv.Listener.Addr().String())

	c := &Checker{}
	app := model.Application{
		Name:        "api",
		Healthcheck: &model.Healthcheck{HTTP: &model.HTTPProbe{Port: port, Host: host, Path: "/"}},
	}
	// Something else could be answering on that port; the unit is what matters.
	if h := c.Check(context.Background(), app, false); h.OK() {
		t.Fatal("a stopped unit must be unhealthy regardless of the probe")
	}
}

func TestHTTPProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	host, port := splitHostPort(t, srv.Listener.Addr().String())

	c := &Checker{}
	good := model.Application{Name: "api", Healthcheck: &model.Healthcheck{
		HTTP: &model.HTTPProbe{Port: port, Host: host, Path: "/health"}}}
	if h := c.Check(context.Background(), good, true); !h.OK() {
		t.Fatalf("want healthy, got %+v", h)
	}

	bad := model.Application{Name: "api", Healthcheck: &model.Healthcheck{
		HTTP: &model.HTTPProbe{Port: port, Host: host, Path: "/nope"}}}
	h := c.Check(context.Background(), bad, true)
	if h.OK() {
		t.Fatal("a 500 must not be reported as healthy")
	}
	if h.Message == "" {
		t.Error("an unhealthy result must say why")
	}
}

func TestHTTPProbeWithExpectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	host, port := splitHostPort(t, srv.Listener.Addr().String())

	c := &Checker{}
	app := model.Application{Name: "api", Healthcheck: &model.Healthcheck{
		HTTP: &model.HTTPProbe{Port: port, Host: host, Expect: 204}}}
	if h := c.Check(context.Background(), app, true); !h.OK() {
		t.Fatalf("want healthy for the expected status, got %+v", h)
	}
}

func TestTCPProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	host, port := splitHostPort(t, ln.Addr().String())

	c := &Checker{}
	app := model.Application{Name: "db", Healthcheck: &model.Healthcheck{
		TCP: &model.TCPProbe{Port: port, Host: host}}}
	if h := c.Check(context.Background(), app, true); !h.OK() {
		t.Fatalf("want healthy, got %+v", h)
	}

	ln.Close()
	if h := c.Check(context.Background(), app, true); h.OK() {
		t.Fatal("a closed port must not be healthy")
	}
}

func TestExecProbe(t *testing.T) {
	var gotApp string
	var gotCmd []string
	c := &Checker{Exec: func(_ context.Context, app string, cmd []string) error {
		gotApp, gotCmd = app, cmd
		return nil
	}}
	app := model.Application{Name: "api", Healthcheck: &model.Healthcheck{
		Exec: &model.ExecProbe{Command: []string{"true"}}}}
	if h := c.Check(context.Background(), app, true); !h.OK() {
		t.Fatalf("want healthy, got %+v", h)
	}
	if gotApp != "api" || len(gotCmd) != 1 || gotCmd[0] != "true" {
		t.Errorf("exec probe called with %q %v", gotApp, gotCmd)
	}

	c.Exec = func(context.Context, string, []string) error { return errors.New("exit status 1") }
	if h := c.Check(context.Background(), app, true); h.OK() {
		t.Fatal("a failing command must be unhealthy")
	}
}

func TestExecProbeWithoutSupportIsUnknownNotHealthy(t *testing.T) {
	c := &Checker{}
	app := model.Application{Name: "api", Healthcheck: &model.Healthcheck{
		Exec: &model.ExecProbe{Command: []string{"true"}}}}
	h := c.Check(context.Background(), app, true)
	if h.Status != model.HealthUnknown {
		t.Fatalf("want unknown when exec is unsupported, got %s", h.Status)
	}
}

func TestWaitRetriesUntilHealthy(t *testing.T) {
	calls := 0
	c := &Checker{Exec: func(context.Context, string, []string) error {
		calls++
		if calls < 3 {
			return errors.New("not yet")
		}
		return nil
	}}
	app := model.Application{Name: "api", Healthcheck: &model.Healthcheck{
		Exec:     &model.ExecProbe{Command: []string{"true"}},
		Retries:  5,
		Interval: "1ms",
	}}
	h := c.Wait(context.Background(), app, func(context.Context) bool { return true })
	if !h.OK() {
		t.Fatalf("want healthy after retries, got %+v", h)
	}
	if calls != 3 {
		t.Errorf("want 3 attempts, got %d", calls)
	}
}

func TestWaitGivesUp(t *testing.T) {
	c := &Checker{Exec: func(context.Context, string, []string) error { return errors.New("nope") }}
	app := model.Application{Name: "api", Healthcheck: &model.Healthcheck{
		Exec:     &model.ExecProbe{Command: []string{"true"}},
		Retries:  2,
		Interval: "1ms",
	}}
	if h := c.Wait(context.Background(), app, func(context.Context) bool { return true }); h.OK() {
		t.Fatal("an app that never comes up must not be reported healthy")
	}
}

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return host, port
}
