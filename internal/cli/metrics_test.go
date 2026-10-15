package cli

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestMetricsAreServedUntilTheAgentStops(t *testing.T) {
	newFakeHost(t, hostWith())
	if out, code := run(t, "reconcile"); code != 0 {
		t.Fatal(out)
	}
	env, err := setup(configFlags{})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveMetrics(ctx, ln, env.engine) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("%s %s", resp.Status, resp.Header.Get("Content-Type"))
	}
	for _, line := range []string{`host="vm-1"} 1`, "podcd_last_success_timestamp_seconds ", "podcd_consecutive_failures 0", `podcd_repository_behind{repository="infrastructure"} 0`} {
		if !strings.Contains(string(body), line) {
			t.Errorf("missing %q in:\n%s", line, body)
		}
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("a stopping agent is not a metrics failure: %v", err)
	}
}

// A taken port must not stop the agent: it is logged and no server is left behind.
func TestMetricsOnATakenPortAreLogged(t *testing.T) {
	newFakeHost(t, hostWith())
	env, err := setup(configFlags{})
	if err != nil {
		t.Fatal(err)
	}
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	var logs strings.Builder
	env.log = slog.New(slog.NewTextHandler(&logs, nil))
	env.cfg.MetricsAddress = taken.Addr().String()

	startMetrics(context.Background(), env)
	if !strings.Contains(logs.String(), "metrics are not served") {
		t.Errorf("logs:\n%s", logs.String())
	}
}
