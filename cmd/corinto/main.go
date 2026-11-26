// Command corinto is the single binary of the project. Each role of the
// system is one of its commands, so that one image runs all of them
// (docs/adr/0002-one-image-three-processes.md).
package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

// Exit codes of the binary.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

const usageText = `usage: corinto <command>

commands:
  api       run the api process: the management API on one port and its
            health endpoints on another
  redirect  run the redirect process: the host of the short links on one
            port and its health endpoints on another
  worker    run the worker process: it adds the accesses to the metrics of
            their links, deletes once a day what has been kept for its time,
            and has its health endpoints on a port
  migrate   apply the migrations the database does not have yet, and exit
  seed      delete the demo workspaces and create the ones of the seed
            again, with their links and a history of ninety days; print
            the key of each, once. With --if-missing, do nothing when a
            demo workspace is there. With --no-history, write no history
  operator  act on the database as the operator; run it without an action
            to see the actions
`

func main() {
	os.Exit(execute())
}

// execute owns the signal context, so that its deferred stop runs before the
// process exits.
func execute() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
}

// run is the program without the process around it: arguments, environment
// and output come in as parameters, so that a test can call it. Only the
// operator writes to stdout; the log of every command goes to stderr.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	logger := slog.New(slog.NewJSONHandler(stderr, nil))

	var err error
	switch {
	case args[0] == "operator":
		err = runOperator(ctx, args[1:], getenv, stdout, logger)
	case args[0] == "seed":
		options, known := seedOptions(args[1:])
		if !known {
			usage(stderr)
			return exitUsage
		}
		err = runSeed(ctx, options, getenv, stdout, logger)
	case len(args) != 1:
		usage(stderr)
		return exitUsage
	case args[0] == "api":
		err = runAPI(ctx, getenv, logger)
	case args[0] == "redirect":
		err = runRedirect(ctx, getenv, logger)
	case args[0] == "worker":
		err = runWorker(ctx, getenv, logger)
	case args[0] == "migrate":
		err = runMigrate(ctx, getenv, logger)
	default:
		usage(stderr)
		return exitUsage
	}
	var wrongUsage usageError
	if errors.As(err, &wrongUsage) {
		// Nothing useful can be done if writing the usage text fails.
		_, _ = io.WriteString(stderr, operatorUsageText+"\n"+wrongUsage.reason+"\n")
		return exitUsage
	}
	if err != nil {
		logger.ErrorContext(ctx, "command failed", "command", args[0], "error", err.Error())
		return exitFailure
	}
	return exitOK
}

func usage(stderr io.Writer) {
	// Nothing useful can be done if writing the usage text fails.
	_, _ = io.WriteString(stderr, usageText)
}
