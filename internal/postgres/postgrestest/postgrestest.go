// Package postgrestest gives the integration tests a real PostgreSQL: one
// server per test binary, in a container, and an empty database per test.
// The image is the one the local stack runs, read from deploy/compose.yaml,
// so that the version has one source.
package postgrestest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// adminURL is the connection URL of the server that Main started, to its
// maintenance database.
var adminURL string

var composeImage = regexp.MustCompile(`(?m)^\s*image:\s*(postgres:\S+)\s*$`)

// Main runs the tests of a package with a PostgreSQL server for all of
// them. A package with integration tests calls it from TestMain:
//
//	func TestMain(m *testing.M) { postgrestest.Main(m) }
func Main(m *testing.M) {
	code, err := run(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "postgrestest:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func run(m *testing.M) (int, error) {
	// An interruption cancels this context instead of killing the process,
	// so that the container is removed on the way out.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// When go test reaches its own timeout it kills the process and nothing
	// deferred runs. This context ends a little earlier, which does.
	if limit, limited := timeoutOfGoTest(); limited {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limit-min(10*time.Second, limit/10))
		defer cancel()
	}

	// The library can start a second container that removes the first when
	// the tests die. It is switched off: that container holds the Docker
	// socket and its image cannot be pinned by digest from here. This
	// function removes its own container instead, also when the run is
	// interrupted or about to time out. Only a process killed outright
	// leaves one behind.
	if err := os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true"); err != nil {
		return 0, err
	}
	image, err := imageOfTheLocalStack()
	if err != nil {
		return 0, err
	}
	// Generated for this run: no password is written anywhere, not even one
	// that only a test would use.
	password, err := randomName()
	if err != nil {
		return 0, err
	}
	container, err := tcpostgres.Run(ctx, image,
		tcpostgres.WithUsername("corinto"),
		tcpostgres.WithPassword(password),
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.BasicWaitStrategies(),
	)
	defer func() { _ = testcontainers.TerminateContainer(container) }()
	if err != nil {
		return 0, fmt.Errorf("start PostgreSQL in a container (is Docker running?): %w", err)
	}
	adminURL, err = container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return 0, err
	}

	// The tests run aside, so that this goroutine can still leave in order.
	finished := make(chan int, 1)
	go func() { finished <- m.Run() }()
	select {
	case code := <-finished:
		return code, nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return 0, errors.New("the tests were about to exceed the timeout of go test; the container was removed")
		}
		return 0, errors.New("interrupted; the container was removed")
	}
}

// timeoutOfGoTest returns the timeout go test was given, if any.
func timeoutOfGoTest() (time.Duration, bool) {
	flag.Parse()
	timeout := flag.Lookup("test.timeout")
	if timeout == nil {
		return 0, false
	}
	limit, err := time.ParseDuration(timeout.Value.String())
	return limit, err == nil && limit > 0
}

// NewDatabase creates an empty database on the server of this test binary
// and returns its connection URL. Each test gets its own, so tests do not
// see each other's rows and can run in any order.
func NewDatabase(t testing.TB) string {
	t.Helper()
	if adminURL == "" {
		t.Fatal("postgrestest.Main did not run: the package needs a TestMain that calls it")
	}
	name, err := randomName()
	if err != nil {
		t.Fatal(err)
	}
	name = "test_" + name

	conn, err := pgx.Connect(t.Context(), adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()
	// The name is made of hexadecimal digits generated above; a database
	// name cannot be passed as a parameter.
	if _, err := conn.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}

	database, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	database.Path = "/" + name
	return database.String()
}

// imageOfTheLocalStack reads the PostgreSQL image, with its digest, from the
// compose file of the repository.
func imageOfTheLocalStack() (string, error) {
	directory, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", errors.New("no go.mod above the working directory")
		}
		directory = parent
	}
	// Opened through a root, so that the path cannot lead out of the
	// repository whatever the working directory was.
	repository, err := os.OpenRoot(directory)
	if err != nil {
		return "", err
	}
	defer func() { _ = repository.Close() }()
	compose, err := fs.ReadFile(repository.FS(), "deploy/compose.yaml")
	if err != nil {
		return "", err
	}
	match := composeImage.FindSubmatch(compose)
	if match == nil {
		return "", errors.New("deploy/compose.yaml names no postgres image")
	}
	return string(match[1]), nil
}

func randomName() (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}
