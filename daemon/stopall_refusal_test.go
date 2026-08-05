package daemon

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
)

// TestPIDArgsAreArgvNotProse guards the split that makes AssertNoLiveDaemon's
// refusal pasteable (#2917).
//
// formatPIDList renders PIDs for a SENTENCE ("41234, 41255"); pidArgs renders
// them for an ARGV. Reusing the prose form in the suggested command would print
// `kill '41234,' 41255`, which is worse than printing nothing: it looks like a
// command, and the user pastes it into a shell that is already the last thing
// standing between them and a wiped home.
func TestPIDArgsAreArgvNotProse(t *testing.T) {
	pids := []int{41234, 41255}

	if got, want := formatPIDList(pids), "41234, 41255"; got != want {
		t.Errorf("formatPIDList = %q, want %q (prose form)", got, want)
	}

	args := pidArgs(pids)
	if len(args) != len(pids) {
		t.Fatalf("pidArgs = %v, want one argument per pid", args)
	}
	for _, a := range args {
		if strings.ContainsAny(a, ", ") {
			t.Errorf("pidArgs produced %q — a comma or space in an argv element means the shell "+
				"receives a different pid list than the one we meant", a)
		}
	}

	// The whole point: what gets printed is runnable as-is.
	if got, want := shellsuggest.Command("kill", args...), "kill 41234 41255"; got != want {
		t.Errorf("suggested command = %q, want %q", got, want)
	}
}
