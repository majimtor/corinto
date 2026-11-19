// Package health serves the liveness and readiness endpoints of a process.
//
// They listen on a port of their own, apart from the one that serves
// traffic, so that a probe never shares a path grammar, a rate limit or an
// authentication rule with a request from outside.
package health

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Check reports whether the process can do its work. A nil error means it
// can.
type Check func(ctx context.Context) error

const (
	// checkTimeout bounds one readiness check, so that a probe gets an answer
	// even when the dependency it asks about hangs.
	checkTimeout = 2 * time.Second

	// shutdownTimeout bounds the wait for probes in flight when the process
	// stops.
	shutdownTimeout = 5 * time.Second
)

// Handler answers GET /livez, which says that the process runs, and
// GET /readyz, which says whether ready reports that it can do its work.
func Handler(ready Check, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		respond(w, http.StatusOK, "live")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), checkTimeout)
		defer cancel()
		if err := ready(ctx); err != nil {
			// The reason goes to the log, not to whoever asks.
			logger.WarnContext(ctx, "readiness check failed", "error", err.Error())
			respond(w, http.StatusServiceUnavailable, "not ready")
			return
		}
		respond(w, http.StatusOK, "ready")
	})
	return mux
}

// Serve answers the health endpoints on listener until ctx is cancelled, and
// then lets the probes in flight finish. The API and the host of the short
// links stop in the same way, each in its own package: the three share a
// shape and no value.
func Serve(ctx context.Context, listener net.Listener, ready Check, logger *slog.Logger) error {
	server := &http.Server{
		Handler:           Handler(ready, logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      checkTimeout + 5*time.Second,
		IdleTimeout:       30 * time.Second,
	}

	failed := make(chan error, 1)
	go func() { failed <- server.Serve(listener) }()

	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}

	// ctx is already cancelled; the wait for probes in flight needs its own.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-failed; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// respond writes a short plain-text answer with the headers every response
// of this project carries.
func respond(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	// A probe that went away cannot be told so.
	_, _ = w.Write([]byte(body + "\n"))
}
