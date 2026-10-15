package cli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/podcd/podcd/pkg/reconciler"
)

// startMetrics serves metrics in the background until ctx ends. Metrics are an
// aid: a taken port is logged, never a reason to stop deploying.
func startMetrics(ctx context.Context, env *environment) {
	addr := env.cfg.MetricsAddress
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		env.log.Error("metrics are not served", "address", addr, "error", err)
		return
	}
	env.log.Info("serving metrics", "url", "http://"+ln.Addr().String()+"/metrics")
	go func() {
		if err := serveMetrics(ctx, ln, env.engine); err != nil {
			env.log.Error("metrics server stopped", "error", err)
		}
	}()
}

// serveMetrics serves GET /metrics on ln until ctx ends.
func serveMetrics(ctx context.Context, ln net.Listener, engine *reconciler.Engine) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		// It only fails writing, once the scraper is gone: nobody left to tell.
		_ = engine.WriteMetrics(w, Version)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
