// Command corinto is the single binary of the project. Each role of the
// system is one of its commands, so that one image runs all of them
// (docs/adr/0002-one-image-three-processes.md).
package main

import (
	"context"
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
  api      run the api process; in this version it only answers its health
           endpoints
  migrate  apply the migrations the database does not have yet, and exit
`

func main() {
	os.Exit(execute())
}

// execute owns the signal context, so that its deferred stop runs before the
// process exits.
func execute() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx, os.Args[1:], os.Getenv, os.Stderr)
}

// run is the program without the process around it: arguments, environment
// and output come in as parameters, so that a test can call it.
func run(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer) int {
	if len(args) != 1 {
		usage(stderr)
		return exitUsage
	}
	logger := slog.New(slog.NewJSONHandler(stderr, nil))

	var err error
	switch args[0] {
	case "api":
		err = runAPI(ctx, getenv, logger)
	case "migrate":
		err = runMigrate(ctx, getenv, logger)
	default:
		usage(stderr)
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
