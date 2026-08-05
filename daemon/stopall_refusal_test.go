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

// TestUnverifiedDaemonRefusalNeverSuggestsAKill locks the distinction between
// the two refusals (Codex on #2941).
//
// StopOrphanDaemons deliberately does NOT signal a daemon whose AF home it could
// not establish: "I could not tell" must never resolve to "kill it", because the
// cost of guessing wrong is killing a working daemon serving another home or
// another user. Printing a pasteable `kill <every unverified pid>` hands the
// user exactly the action reset declined to take — laundering a safety refusal
// into an instruction, and worse for being copy-pasteable.
//
// The proven-ours refusal may suggest a kill, because there the ownership
// question is settled. That asymmetry is the point, so both halves are asserted.
func TestUnverifiedDaemonRefusalNeverSuggestsAKill(t *testing.T) {
	pids := []int{41234, 41255}

	unverified := unverifiedDaemonRefusal(pids).Error()
	if strings.Contains(unverified, "kill") {
		t.Errorf("the unverified-daemon refusal suggests a kill: %q\n"+
			"reset refused to signal these precisely because it could not prove whose they are; "+
			"telling the user to kill them all undoes that safety decision", unverified)
	}
	if !strings.Contains(unverified, "ps ") {
		t.Errorf("refusal = %q, want an inspection command so the user can identify which daemon "+
			"serves this home before stopping anything", unverified)
	}
	for _, pid := range []string{"41234", "41255"} {
		if !strings.Contains(unverified, pid) {
			t.Errorf("refusal = %q, want it to name pid %s", unverified, pid)
		}
	}

	// The other half: ownership settled, so a kill is the right remedy.
	own := ownDaemonRefusal(pids).Error()
	if !strings.Contains(own, "kill 41234 41255") {
		t.Errorf("refusal for PROVEN-ours daemons = %q, want a pasteable `kill 41234 41255` — "+
			"reset has already tried and failed to stop them itself", own)
	}
}
