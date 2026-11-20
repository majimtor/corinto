// Package health serves the liveness and readiness endpoints of a process,
// and what the process measures about itself when it has something to say.
//
// They listen on a port of their own, apart from the one that serves
// traffic, so that a probe never shares a path grammar, a rate limit or an
// authentication rule with a request from outside.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Check reports whether the process can do its work. A nil error means it
// can.
type Check func(ctx context.Context) error

// Status returns what a process measures about itself, for whoever operates
// it. It is answered as JSON, and carries nothing of a request.
type Status func() any

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
// With a status it also answers GET /statusz, with what status returns; a
// process that has none passes nil, and has no such endpoint.
func Handler(ready Check, status Status, logger *slog.Logger) http.Handler {
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
	if status != nil {
		mux.HandleFunc("GET /statusz", func(w http.ResponseWriter, r *http.Request) {
			body, err := json.Marshal(status())
			if err != nil {
				// A status that cannot be written is a mistake in the code.
				logger.ErrorContext(r.Context(), "the status could not be written", "error", err.Error())
				respond(w, http.StatusInternalServerError, "no status")
				return
			}
			write(w, http.StatusOK, "application/json", append(body, '\n'))
		})
	}
	return mux
}

// Serve answers the health endpoints on listener until ctx is cancelled, and
// then lets the probes in flight finish. The API and the host of the short
// links stop in the same way, each in its own package: the three share a
// shape and no value.
func Serve(ctx context.Context, listener net.Listener, ready Check, status Status, logger *slog.Logger) error {
	server := &http.Server{
		Handler:           Handler(ready, status, logger),
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

// respond writes a short plain-text answer.
func respond(w http.ResponseWriter, status int, body string) {
	write(w, status, "text/plain", []byte(body+"\n"))
}

// write answers with the headers every response of this project carries.
func write(w http.ResponseWriter, status int, mediaType string, body []byte) {
	w.Header().Set("Content-Type", mediaType+"; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	// A probe that went away cannot be told so.
	_, _ = w.Write(body)
}
