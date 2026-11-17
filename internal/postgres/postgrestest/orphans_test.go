package postgrestest

import (
	"os"
	"os/exec"
	"strconv"
	"testing"
)

// deadProcess returns the identifier of a process of this machine that
// existed and is gone: this test binary, run with no test selected.
func deadProcess(t *testing.T) int {
	t.Helper()
	//nolint:gosec // the command is this test binary itself, with a fixed argument
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$")
	if err := child.Run(); err != nil {
		t.Fatal(err)
	}
	return child.Process.Pid
}

// A container is removed only when the test binary that started it is
// surely gone. Every doubt keeps it: removing one in use would fail the
// tests of somebody else.
func TestOnlyAContainerOfARunThatIsGoneIsAnOrphan(t *testing.T) {
	machine, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	dead := strconv.Itoa(deadProcess(t))
	cases := []struct {
		name  string
		label string
		want  bool
	}{
		{"a run that is gone", machine + "/" + dead, true},
		{"an earlier process with the identifier of this one", owner(os.Getpid()), true},
		{"the run that started this test", owner(os.Getppid()), false},
		{"a run of another machine", "another-" + machine + "/" + dead, false},
		{"no machine", dead, false},
		{"an identifier that is not a number", machine + "/soon", false},
		{"the identifier of no process", machine + "/0", false},
		// To the system a negative identifier names a group of processes,
		// and one that does not exist looks like a process that is gone.
		{"a negative identifier", machine + "/-99999", false},
		{"no label", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := orphan(c.label); got != c.want {
				t.Errorf("orphan(%q) = %v, want %v", c.label, got, c.want)
			}
		})
	}
}
