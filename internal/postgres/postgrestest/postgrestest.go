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
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// adminURL is the connection URL of the server that Main started, to its
// maintenance database.
var adminURL string

// ownerLabel marks a container of this package with the test binary that
// started it: the name of the machine, a slash and the process identifier.
const ownerLabel = "corinto.postgrestest.owner"

// removalWait is how long a container that could not be removed is given
// to disappear. Several test binaries start at once and find the same
// container: one removes it, and the others are told that its removal is
// already in progress, or that it does not exist.
const removalWait = 10 * time.Second

// starts is how many times a test binary tries to start its server. Docker
// on a busy machine sometimes does not have a container ready in the time
// the library gives it; a second container usually is.
const starts = 2

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
	// interrupted or about to time out. A test binary that dies at once, by
	// a kill or by a panic in a test, leaves its container behind, and the
	// next run removes it.
	if err := os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true"); err != nil {
		return 0, err
	}
	removeOrphans(ctx, os.Stderr)
	container, err := started(ctx, startServer, remove, os.Stderr)
	defer remove(container)
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

// remove removes a container of this test binary, also one that did not
// finish starting. With none there is nothing to do.
func remove(container *tcpostgres.PostgresContainer) {
	_ = testcontainers.TerminateContainer(container)
}

// started returns a server that started. One that did not is removed and
// another is started in its place, up to the number of starts; that it
// happened is said, so that a slow run can be told from a quick one. When
// none starts, the error is the one of the last.
func started(
	ctx context.Context, start func(context.Context) (*tcpostgres.PostgresContainer, error),
	remove func(*tcpostgres.PostgresContainer), said io.Writer,
) (*tcpostgres.PostgresContainer, error) {
	for attempt := 1; ; attempt++ {
		container, err := start(ctx)
		if err == nil {
			return container, nil
		}
		// A start that failed can leave its container behind: it goes
		// before the next one, and after the last.
		remove(container)
		if attempt == starts || ctx.Err() != nil {
			return nil, err
		}
		_, _ = fmt.Fprintf(said, "postgrestest: PostgreSQL did not start in a container and is started once more: %v\n", err)
	}
}

// startServer starts the PostgreSQL of this test binary, labelled with it.
// The container is returned with an error too, when there is one to remove.
func startServer(ctx context.Context) (*tcpostgres.PostgresContainer, error) {
	image, err := imageOfTheLocalStack()
	if err != nil {
		return nil, err
	}
	// Generated for this run: no password is written anywhere, not even one
	// that only a test would use.
	password, err := randomName()
	if err != nil {
		return nil, err
	}
	return tcpostgres.Run(ctx, image,
		tcpostgres.WithUsername("corinto"),
		tcpostgres.WithPassword(password),
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.BasicWaitStrategies(),
		testcontainers.WithLabels(map[string]string{ownerLabel: owner(os.Getpid())}),
	)
}

// owner returns the value of the label for a test binary of this machine.
func owner(pid int) string {
	// Without a name the machine is the empty one, here and in every run.
	machine, _ := os.Hostname()
	return machine + "/" + strconv.Itoa(pid)
}

// removeOrphans removes the containers that test binaries of this machine
// started and did not live to remove. It is housekeeping: what it cannot do
// it says in notes and leaves for the next run, since several test binaries
// start at once and may find the same container.
func removeOrphans(ctx context.Context, notes io.Writer) {
	docker, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		// Starting the container reports the same problem, with its cause.
		return
	}
	defer func() { _ = docker.Close() }()
	labelled, err := docker.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", ownerLabel),
	})
	if err != nil {
		return
	}
	for _, container := range labelled.Items {
		if !orphan(container.Labels[ownerLabel]) {
			continue
		}
		_, err := docker.ContainerRemove(ctx, container.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		switch {
		case err == nil:
			_, _ = fmt.Fprintf(notes, "postgrestest: removed a container left by an earlier run (%s)\n", container.Labels[ownerLabel])
		case gone(ctx, docker, container.ID):
			// Another run was removing it, and has. What matters is that
			// the container is not there, not who took it away.
		default:
			_, _ = fmt.Fprintf(notes, "postgrestest: a container left by an earlier run was not removed: %v\n", err)
		}
	}
}

// gone waits for a container to disappear, for removalWait at the most,
// and reports whether it did. It asks for the container by listing it,
// which answers the same whatever the reason the removal was refused for.
func gone(ctx context.Context, docker client.ContainerAPIClient, id string) bool {
	ctx, cancel := context.WithTimeout(ctx, removalWait)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		found, err := docker.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: make(client.Filters).Add("id", id)})
		if err == nil && len(found.Items) == 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

// orphan reports whether the test binary that a label names is gone. On any
// doubt the answer is no: a container in use must never be removed. So a
// label of another machine is left alone, because its process identifiers
// mean nothing here, and so is one that cannot be read.
func orphan(label string) bool {
	machine, number, found := strings.Cut(label, "/")
	if !found || owner(0) != machine+"/0" {
		return false
	}
	pid, err := strconv.Atoi(number)
	if err != nil || pid <= 0 {
		return false
	}
	// This binary has not started its container yet, so one that carries
	// its identifier belongs to an earlier process that had the same one.
	if pid == os.Getpid() {
		return true
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return true
	}
	defer func() { _ = process.Release() }()
	// Signal 0 delivers nothing and only asks whether the process exists.
	err = process.Signal(syscall.Signal(0))
	return errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH)
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
