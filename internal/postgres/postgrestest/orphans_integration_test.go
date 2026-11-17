//go:build integration

package postgrestest

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"testing"

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
