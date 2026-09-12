// Package health answers one question per application: is it actually working?
//
// "systemd says the unit is active" is the floor, not the answer. An app with a
// healthcheck must prove itself over the network or through an exec probe.
package health

import (
	"cmp"
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/podcd/podcd/pkg/model"
)

// Defaults used when the application does not say otherwise.
const (
	DefaultTimeout  = 5 * time.Second
	DefaultInterval = 2 * time.Second
	DefaultRetries  = 15
	DefaultHost     = "127.0.0.1"
)

// ExecFunc runs a command inside an application's container.
type ExecFunc func(ctx context.Context, app string, cmd []string) error

// Checker probes applications.
type Checker struct {
	// Exec is required for exec probes; nil means exec probes cannot run.
	Exec ExecFunc
	// HTTPClient is used for http probes. nil means a default client.
	HTTPClient *http.Client
}

// Check probes one application once.
//
// unitActive is what systemd reports. A stopped unit is unhealthy regardless of
// what a probe might say, because a probe can be answered by something else
// listening on the same port.
func (c *Checker) Check(ctx context.Context, app model.Application, unitActive bool) model.Health {
	h := model.Health{App: app.Name, CheckedAt: time.Now().UTC()}

	if !unitActive {
		h.Probe = "systemd"
		h.Status = model.HealthUnhealthy
		h.Message = "unit is not active"
		return h
	}

	hc := app.Healthcheck
	if hc == nil || (hc.HTTP == nil && hc.TCP == nil && hc.Exec == nil) {
		h.Probe = "systemd"
		h.Status = model.HealthHealthy
		h.Message = "unit is active (no healthcheck defined)"
		return h
	}

	switch {
	case hc.HTTP != nil:
		h.Probe = "http"
		h.Message, h.Status = c.checkHTTP(ctx, hc.HTTP)
	case hc.TCP != nil:
		h.Probe = "tcp"
		h.Message, h.Status = c.checkTCP(ctx, hc.TCP)
	case hc.Exec != nil:
		h.Probe = "exec"
		h.Message, h.Status = c.checkExec(ctx, app.Name, hc.Exec)
	}
	return h
}

// Wait probes until the application is healthy or the retries run out. It is
// what the reconciler uses right after applying a change: an app that never
// comes up should fail the reconcile, not quietly stay broken.
func (c *Checker) Wait(ctx context.Context, app model.Application, unitActive func(context.Context) bool) model.Health {
	retries, interval := DefaultRetries, DefaultInterval
	if hc := app.Healthcheck; hc != nil {
		retries = cmp.Or(hc.Retries, retries)
		interval = parseDuration(hc.Interval, interval)
	}

	var last model.Health
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				last.Message = "gave up waiting: " + ctx.Err().Error()
				last.Status = model.HealthUnknown
				return last
			case <-time.After(interval):
			}
		}
		last = c.Check(ctx, app, unitActive(ctx))
		if last.OK() {
			return last
		}
	}
	return last
}

func (c *Checker) checkHTTP(ctx context.Context, p *model.HTTPProbe) (string, model.HealthStatus) {
	path := "/" + strings.TrimPrefix(p.Path, "/")
	expect := cmp.Or(p.Expect, http.StatusOK)
	timeout := parseDuration(p.Timeout, DefaultTimeout)

	url := cmp.Or(p.Scheme, "http") + "://" + hostPort(p.Host, p.Port) + path
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err.Error(), model.HealthUnknown
	}
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Sprintf("GET %s: %v", url, err), model.HealthUnhealthy
	}
	defer resp.Body.Close()
	if resp.StatusCode != expect {
		return fmt.Sprintf("GET %s: status %d (want %d)", url, resp.StatusCode, expect), model.HealthUnhealthy
	}
	return fmt.Sprintf("GET %s: %d", url, resp.StatusCode), model.HealthHealthy
}

func (c *Checker) checkTCP(ctx context.Context, p *model.TCPProbe) (string, model.HealthStatus) {
	timeout := parseDuration(p.Timeout, DefaultTimeout)
	addr := hostPort(p.Host, p.Port)

	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Sprintf("dial %s: %v", addr, err), model.HealthUnhealthy
	}
	_ = conn.Close()
	return "dial " + addr + ": ok", model.HealthHealthy
}

func (c *Checker) checkExec(ctx context.Context, app string, p *model.ExecProbe) (string, model.HealthStatus) {
	if c.Exec == nil {
		return "exec probes are not supported by this runtime", model.HealthUnknown
	}
	timeout := parseDuration(p.Timeout, DefaultTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := c.Exec(ctx, app, p.Command); err != nil {
		return err.Error(), model.HealthUnhealthy
	}
	return "exec ok", model.HealthHealthy
}

// hostPort is the address a probe dials; an empty host means the loopback.
func hostPort(host string, port int) string {
	return net.JoinHostPort(cmp.Or(host, DefaultHost), strconv.Itoa(port))
}

// parseDuration reads a duration from the spec, or falls back to def when it
// is empty or not usable.
func parseDuration(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}
