package tmux

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// #6258 binds a destructive kill to one probed incarnation by its tmux session
// id. That is only sound if tmux behaves the way the design leans on, so this
// test pins the four behaviours against a REAL tmux server (an isolated one, via
// testguard.IsolateTmux — it skips where tmux is absent):
//
//  1. a session id ("$N") is accepted as the target of show-environment,
//     display-message and kill-session, and show-environment on it returns the
//     variable new-session -e stamped;
//  2. ids are not reused while the server lives: a session killed and re-created
//     under the same name gets a different id, across repeated cycles;
//  3. kill-session on an id whose session is gone fails ("can't find session: $N")
//     and kills nothing, even when the name has since been re-created;
//  4. killing one id leaves a same-named replacement and other sessions alone.
func TestTmuxSessionIDSemanticsTheIncarnationBindingRelies(t *testing.T) {
	testguard.IsolateTmux(t)

	tm := func(args ...string) (stdout, stderr string, err error) {
		out, runErr := exec.Command("tmux", args...).Output()
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(exitErr.Stderr))
		}
		return strings.TrimSpace(string(out)), stderr, runErr
	}
	must := func(args ...string) string {
		t.Helper()
		out, stderr, err := tm(args...)
		if err != nil {
			t.Fatalf("tmux %s: %v: %s", strings.Join(args, " "), err, stderr)
		}
		return out
	}
	newSession := func(name, instance string) {
		t.Helper()
		must("new-session", "-d", "-s", name, "-e", EnvMarkerInstance+"="+instance, "sleep 600")
	}
	sessionID := func(name string) string {
		t.Helper()
		return must("display-message", "-p", "-t", exactTarget(name), "#{session_id}")
	}
	alive := func(name string) bool {
		_, _, err := tm("has-session", "-t", exactTarget(name))
		return err == nil
	}

	// A keeper session holds the server open while the subject is killed.
	must("new-session", "-d", "-s", "keeper", "sleep 600")

	const name = "af_id_semantics"
	newSession(name, "inst-A")
	idA := sessionID(name)
	if !strings.HasPrefix(idA, "$") {
		t.Fatalf("session_id %q is not a $-prefixed id", idA)
	}

	// 1. The id addresses the session for every command the reap issues.
	if got := must("show-environment", "-t", idA, EnvMarkerInstance); got != EnvMarkerInstance+"=inst-A" {
		t.Fatalf("show-environment -t %s = %q, want the stamped instance id", idA, got)
	}
	if got := must("display-message", "-p", "-t", idA, "#{pid} #{session_created}"); len(strings.Fields(got)) != 2 {
		t.Fatalf("display-message -t %s = %q, want server pid and creation stamp", idA, got)
	}

	// 2. Kill and re-create under the same name, repeatedly: every incarnation gets
	// a fresh id.
	seen := map[string]bool{idA: true}
	must("kill-session", "-t", idA)
	for i := 0; i < 6; i++ {
		newSession(name, "inst-cycle")
		id := sessionID(name)
		if seen[id] {
			t.Fatalf("session id %s was reused by a later incarnation of %q (ids so far: %v)", id, name, seen)
		}
		seen[id] = true
		if i < 5 {
			must("kill-session", "-t", id)
		}
	}
	idB := sessionID(name) // the live replacement

	// 3. Every id-addressed command the reap issues, aimed at the dead id — the live
	// replacement now holds the NAME the dead one had. Each must fail, name the id,
	// kill nothing, and be recognised as "gone" by the production classifier.
	//
	// tmux words a vanished session per COMMAND (measured on the CI runner:
	// show-environment says "no such session", kill-session "can't find session"),
	// so the wording is asserted per command: a tmux that changes one fails here,
	// loudly, instead of silently turning a gone incarnation into an unknown one.
	t.Logf("tmux version: %s", must("-V"))
	// An empty entry is a command whose vanished-id wording the first CI run did not
	// record; the test then only requires the classifier to recognise it and logs
	// what tmux said, to be pinned in the next commit.
	wording := map[string]string{
		"show-environment": "no such session: " + idA,
		"kill-session":     "can't find session: " + idA,
		"display-message":  "",
		"list-panes":       "",
		"has-session":      "",
	}
	vanished := map[string][]string{
		"show-environment": {"show-environment", "-t", idA, EnvMarkerInstance},
		"kill-session":     {"kill-session", "-t", idA},
		"display-message":  {"display-message", "-p", "-t", idA, "#{pid} #{session_created}"},
		"list-panes":       {"list-panes", "-s", "-t", idA, "-F", "#{pane_id}"},
		"has-session":      {"has-session", "-t", idA},
	}
	for command, args := range vanished {
		_, stderr, err := tm(args...)
		t.Logf("%s on a vanished id: exit error %v, stderr %q", command, err, stderr)
		if err == nil {
			t.Fatalf("%s of a vanished id succeeded", command)
		}
		if want := wording[command]; want != "" && stderr != want {
			t.Fatalf("%s of a vanished id said %q, want %q — tmux changed its wording; re-check every classifier on the id path",
				command, stderr, want)
		}
		if !sessionIDProvedAbsent(cmd.MakeExecutor(), err, idA) {
			t.Fatalf("%s of a vanished id (%q) is not recognised as gone by sessionIDProvedAbsent", command, stderr)
		}
	}
	if !alive(name) || sessionID(name) != idB {
		t.Fatalf("a command addressed to the dead id %s touched the replacement %s", idA, idB)
	}

	// The classifier must NOT call a live session's failure "gone": a command that
	// fails for an unrelated reason against an id the server still lists is unknown.
	liveErr := exitOneWith(t, "operation not permitted")
	if sessionIDProvedAbsent(cmd.MakeExecutor(), liveErr, idB) {
		t.Fatalf("a failure that does not say the session is gone was read as gone while %s is still listed", idB)
	}

	// 4. Killing one id leaves the others alone.
	newSession("af_id_semantics_other", "inst-other")
	must("kill-session", "-t", idB)
	if alive(name) {
		t.Fatalf("kill-session -t %s left that session alive", idB)
	}
	if !alive("af_id_semantics_other") || !alive("keeper") {
		t.Fatal("killing one session id killed another session")
	}
}
