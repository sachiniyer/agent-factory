package daemon

import (
	"regexp"
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
// killCommand matches a kill aimed at a pid — `kill 41234`, `kill -9 41234` —
// without matching the word "killing" in prose explaining why that is unsafe.
var killCommand = regexp.MustCompile(`\bkill\b[^\n]{0,12}?\s\d`)

func TestUnverifiedDaemonRefusalNeverSuggestsAKill(t *testing.T) {
	pids := []int{41234, 41255}

	unverified := unverifiedDaemonRefusal(pids).Error()
	// Assert on the pasteable COMMAND, not the word. The message legitimately
	// says "killing the wrong one takes down another home's daemon" — that
	// sentence is the point of the refusal, and a bare Contains("kill") check
	// flagged it. (It did: this test failed in CI on exactly that, which is what
	// a daemon-package test that cannot be run on the dev host is for.)
	if unsafe := shellsuggest.Command("kill", pidArgs(pids)...); strings.Contains(unverified, unsafe) {
		t.Errorf("the unverified-daemon refusal contains the pasteable %q: %q\n"+
			"reset refused to signal these precisely because it could not prove whose they are; "+
			"handing the user a command to kill them all undoes that safety decision", unsafe, unverified)
	}
	if killCommand.MatchString(unverified) {
		t.Errorf("the unverified-daemon refusal offers a kill aimed at a pid: %q", unverified)
	}
	// The inspection must be able to ANSWER the question. `ps -o pid,args` cannot:
	// the autostart unit is `ExecStart=<binary> --daemon` with AGENT_FACTORY_HOME
	// in the unit environment, so every daemon's argv is identical and argv-based
	// output distinguishes nothing (Codex on #2941, second round).
	if strings.Contains(unverified, "ps -o pid,args") {
		t.Errorf("refusal = %q suggests an argv-based inspection, but every af daemon's argv is "+
			"`<binary> --daemon` — it cannot reveal which AF home any of them serves", unverified)
	}
	if !strings.Contains(unverified, "AGENT_FACTORY_HOME") {
		t.Errorf("refusal = %q, want the inspection to read the value that actually identifies the "+
			"home, since that is the question the user has to answer", unverified)
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
