package cli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/podcd/podcd/pkg/reconciler"
)

// serveMetrics serves GET /metrics on ln until ctx ends.
func serveMetrics(ctx context.Context, ln net.Listener, engine *reconciler.Engine) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if err := engine.WriteMetrics(w, Version); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
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
