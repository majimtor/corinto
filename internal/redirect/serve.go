package redirect

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net"
	"net/http"

	"github.com/majimtor/corinto/internal/limits"
)

// Serve answers on the host of the short links until ctx is cancelled, and
// then lets the requests in flight finish. The API and the health endpoints
// stop in the same way, each in its own package: the three share a shape
// and no value.
func Serve(ctx context.Context, listener net.Listener, handler http.Handler, logger *slog.Logger) error {
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: limits.RedirectReadHeaderTimeout,
		ReadTimeout:       limits.RedirectReadTimeout,
		WriteTimeout:      limits.RedirectWriteTimeout,
		IdleTimeout:       limits.RedirectIdleTimeout,
		MaxHeaderBytes:    limits.RedirectMaxHeaderBytes,
		ErrorLog:          log.New(unnamed{ctx, logger}, "", 0),
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
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), limits.RedirectWriteTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-failed; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// unnamed is where the HTTP server of this host writes what it has to say.
// Its messages can name the address a connection came from, and the address
// of a visitor is never logged, so what goes to the log is that the server
// said something, and not what.
type unnamed struct {
	ctx    context.Context //nolint:containedctx // a log.Logger has nowhere else to take it from
	logger *slog.Logger
}

func (u unnamed) Write(message []byte) (int, error) {
	u.logger.WarnContext(u.ctx, "the HTTP server of the redirect reported a problem; its text can name a visitor and is not logged")
	return len(message), nil
}
