package api

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"

	"github.com/majimtor/corinto/internal/limits"
)

// Serve answers the API on listener until ctx is cancelled, and then lets
// the requests in flight finish. The health endpoints and the host of the
// short links stop in the same way, each in its own package: the three
// share a shape and no value.
func Serve(ctx context.Context, listener net.Listener, handler http.Handler, logger *slog.Logger) error {
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: limits.APIReadHeaderTimeout,
		ReadTimeout:       limits.APIReadTimeout,
		WriteTimeout:      limits.APIWriteTimeout,
		IdleTimeout:       limits.APIIdleTimeout,
		// What the server itself has to say goes to the same log, in the
		// same form, as everything else.
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	failed := make(chan error, 1)
	go func() { failed <- server.Serve(listener) }()

	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}

	// ctx is already cancelled; the wait for requests in flight needs its
	// own. No request lasts longer than the time the server has to answer.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), limits.APIWriteTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-failed; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
