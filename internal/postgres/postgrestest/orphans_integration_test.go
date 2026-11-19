//go:build integration

package postgrestest

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
)

// labelled returns the identifiers of the containers that carry the label
// of this package, with a given value when one is given.
func labelled(t *testing.T, value string) map[string]bool {
	t.Helper()
	docker, err := testcontainers.NewDockerClientWithOpts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = docker.Close() }()
	filter := ownerLabel
	if value != "" {
		filter += "=" + value
	}
	found, err := docker.ContainerList(t.Context(), client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", filter),
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := make(map[string]bool)
	for _, container := range found.Items {
		ids[container.ID] = true
	}
	return ids
}

// Without its label the server of a test binary could never be told from
// any other container, and nothing would remove it after a crash.
func TestTheServerOfATestBinaryCarriesItsLabel(t *testing.T) {
	t.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
	server, err := startServer(t.Context())
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(server) })
	if err != nil {
		t.Fatal(err)
	}
	if !labelled(t, owner(os.Getpid()))[server.GetContainerID()] {
		t.Errorf("the server is not labelled %s=%s", ownerLabel, owner(os.Getpid()))
	}
}

// A test binary that dies at once leaves its container running. The next
// run removes it, and only it: the container of a run that is alive and the
// one of another machine stay.
func TestTheContainerOfARunThatIsGoneIsRemovedAndNoOther(t *testing.T) {
	ctx := t.Context()
	// Switched off here as Main does, and for the same reason.
	t.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
	image, err := imageOfTheLocalStack()
	if err != nil {
		t.Fatal(err)
	}
	machine, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}

	// The image of the stack, already on the machine, doing nothing.
	start := func(label string) string {
		t.Helper()
		container, err := testcontainers.Run(ctx, image,
			testcontainers.WithEntrypoint("sleep"), testcontainers.WithCmd("300"),
			testcontainers.WithLabels(map[string]string{ownerLabel: label}))
		// Killed, not asked to stop: sleep would take the whole grace period.
		t.Cleanup(func() { _ = testcontainers.TerminateContainer(container, testcontainers.StopTimeout(0)) })
		if err != nil {
			t.Fatal(err)
		}
		return container.GetContainerID()
	}
	dead := deadProcess(t)
	gone := start(owner(dead))
	alive := start(owner(os.Getppid()))
	// The same identifier on another machine says nothing about this one.
	foreign := start("another-" + machine + "/" + strconv.Itoa(dead))

	var notes bytes.Buffer
	removeOrphans(ctx, &notes)

	left := labelled(t, "")
	if left[gone] {
		t.Error("the container of a run that is gone is still there")
	}
	if !left[alive] {
		t.Error("the container of a run that is alive was removed")
	}
	if !left[foreign] {
		t.Error("the container of another machine was removed")
	}
	// Another test binary may have removed it first; when this one did, it
	// says so.
	if strings.Contains(notes.String(), "was not removed") {
		t.Errorf("a removal failed: %s", notes.String())
	}
}

// Several test binaries start at once and find the same containers. One of
// them removes each, and the others are told by Docker that its removal is
// already in progress, or that it does not exist: for them that is not a
// failure, and when they go on the container is not there.
func TestRunsThatStartAtOnceRemoveTheSameContainersWithoutFailing(t *testing.T) {
	ctx := t.Context()
	t.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
	image, err := imageOfTheLocalStack()
	if err != nil {
		t.Fatal(err)
	}
	label := owner(deadProcess(t))
	var left []string
	for range 3 {
		container, err := testcontainers.Run(ctx, image,
			testcontainers.WithEntrypoint("sleep"), testcontainers.WithCmd("300"),
			testcontainers.WithLabels(map[string]string{ownerLabel: label}))
		t.Cleanup(func() { _ = testcontainers.TerminateContainer(container, testcontainers.StopTimeout(0)) })
		if err != nil {
			t.Fatal(err)
		}
		left = append(left, container.GetContainerID())
	}

	const runs = 8
	notes := make([]bytes.Buffer, runs)
	begin := make(chan struct{})
	var wait sync.WaitGroup
	for run := range runs {
		wait.Go(func() {
			<-begin
			removeOrphans(ctx, &notes[run])
		})
	}
	close(begin)
	wait.Wait()

	// Every run has gone on, and the containers are not there.
	still := labelled(t, label)
	for _, id := range left {
		if still[id] {
			t.Errorf("the container %.12s is still there", id)
		}
	}
	removed := 0
	for run := range notes {
		if strings.Contains(notes[run].String(), "was not removed") {
			t.Errorf("run %d says a removal failed: %s", run, notes[run].String())
		}
		removed += strings.Count(notes[run].String(), "removed a container left by an earlier run ("+label+")")
	}
	// A container is removed by one run at the most. Another test binary
	// of this same go test may have taken some first.
	if removed > len(left) {
		t.Errorf("%d removals of %d containers", removed, len(left))
	}
}

// A run that could not remove a container goes on when the container is
// not there, and not before; and it does not wait for ever for one that
// stays.
func TestAContainerIsGoneOnlyWhenItIsNotThere(t *testing.T) {
	ctx := t.Context()
	t.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
	image, err := imageOfTheLocalStack()
	if err != nil {
		t.Fatal(err)
	}
	// Labelled as of a run that is alive, so that no other test binary
	// takes it away.
	container, err := testcontainers.Run(ctx, image,
		testcontainers.WithEntrypoint("sleep"), testcontainers.WithCmd("300"),
		testcontainers.WithLabels(map[string]string{ownerLabel: owner(os.Getppid())}))
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(container, testcontainers.StopTimeout(0)) })
	if err != nil {
		t.Fatal(err)
	}
	id := container.GetContainerID()
	docker, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = docker.Close() }()

	// While it is there it is not gone, for as long as the caller waits.
	short, cancel := context.WithTimeout(ctx, time.Second)
	started := time.Now()
	stays := gone(short, docker, id)
	cancel()
	if elapsed := time.Since(started); stays || elapsed < time.Second {
		t.Fatalf("a container that is there is gone = %t after %s; want false after the second the caller gave", stays, elapsed)
	}

	// Somebody else removes it while a run waits.
	waited := make(chan bool, 1)
	go func() { waited <- gone(ctx, docker, id) }()
	select {
	case early := <-waited:
		t.Fatalf("gone = %t before the container was removed", early)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := docker.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case left := <-waited:
		if !left {
			t.Error("a container that was removed is not gone")
		}
	case <-time.After(removalWait):
		t.Error("the run that waited never saw that the container was removed")
	}
	// One that was never there is gone at once.
	started = time.Now()
	if !gone(ctx, docker, "0000000000000000000000000000000000000000000000000000000000000000") || time.Since(started) > time.Second {
		t.Errorf("a container that does not exist is not gone at once (%s)", time.Since(started))
	}
}
