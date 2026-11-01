package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Serve publishes the process's metrics at addr/metrics until ctx is
// cancelled (SIGINT/SIGTERM), then shuts down gracefully and returns nil.
// A listen failure — typically a port conflict at boot — returns an error
// so the caller fails the process instead of running unobserved (#55: every
// Go process serves /metrics).
func (c *Collectors) Serve(ctx context.Context, addr string, log *slog.Logger) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("metrics: listen %s: %w", addr, err)
	}
	return c.ServeOn(ctx, lis, log)
}

// ServeOn is Serve on an already-bound listener; Serve delegates here. It
// exists separately so tests can bind :0 and discover the chosen port.
func (c *Collectors) ServeOn(ctx context.Context, lis net.Listener, log *slog.Logger) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(c.registry, promhttp.HandlerOpts{}))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(lis) }()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("metrics: serve %s: %w", lis.Addr(), err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Warn("metrics server shutdown failed", "error", err)
		}
		return nil
	}
}
