package tmux

import (
	"fmt"
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

	// What each command does when aimed at a vanished id — exit code, stdout, stderr —
	// observed and pinned. They DIFFER: display-message succeeds with empty output,
	// the others fail with their own wording. A change in any of them fails here,
	// loudly, instead of silently turning a gone incarnation into an unknown one.
	type behaviour struct {
		exit           int
		stdout, stderr string
	}
	observe := func(args ...string) behaviour {
		out, runErr := exec.Command("tmux", args...).Output()
		b := behaviour{stdout: strings.TrimSpace(string(out))}
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			b.exit = exitErr.ExitCode()
			b.stderr = strings.TrimSpace(string(exitErr.Stderr))
		} else if runErr != nil {
			t.Fatalf("tmux %v: %v", args, runErr)
		}
		return b
	}
	// The format every identity read uses: the answer must carry #{session_id}.
	identity := "#{session_id} #{pid} #{session_created}"
	observed := map[string]behaviour{
		"display-message":  observe("display-message", "-p", "-t", idA, identity),
		"show-environment": observe("show-environment", "-t", idA, EnvMarkerInstance),
		"kill-session":     observe("kill-session", "-t", idA),
		"list-panes":       observe("list-panes", "-s", "-t", idA, "-F", "#{session_id}|#{pid}|#{session_created}"),
		"has-session":      observe("has-session", "-t", idA),
		"if-shell":         observe("if-shell", "-F", "-t", idA, "#{==:#{session_id},"+idA+"}", "display-message -p af-branch-true", "display-message -p af-branch-false"),
	}
	for command, got := range observed {
		t.Logf("%s on a vanished id: exit %d, stdout %q, stderr %q", command, got.exit, got.stdout, got.stderr)
	}
	pinned := map[string]behaviour{
		"show-environment": {exit: 1, stderr: "no such session: " + idA},
		"kill-session":     {exit: 1, stderr: "can't find session: " + idA},
	}
	for command, want := range pinned {
		if got := observed[command]; got != want {
			t.Errorf("%s on a vanished id = %+v, want %+v — tmux changed; re-check every classifier on the id path", command, got, want)
		}
	}
	// Every command that fails must be recognised as gone by the production classifier.
	for _, command := range []string{"show-environment", "kill-session", "list-panes", "has-session", "if-shell"} {
		got := observed[command]
		if got.exit == 0 {
			continue
		}
		_, runErr := exec.Command("tmux", map[string][]string{
			"show-environment": {"show-environment", "-t", idA, EnvMarkerInstance},
			"kill-session":     {"kill-session", "-t", idA},
			"list-panes":       {"list-panes", "-s", "-t", idA},
			"has-session":      {"has-session", "-t", idA},
			"if-shell":         {"if-shell", "-F", "-t", idA, "1", "display-message -p x", "display-message -p y"},
		}[command]...).Output()
		if !sessionIDProvedAbsent(cmd.MakeExecutor(), runErr, idA) {
			t.Errorf("%s of a vanished id (%+v) is not recognised as gone by sessionIDProvedAbsent", command, got)
		}
	}
	// Exit 0 is not proof. On a vanished id display-message SUCCEEDS (measured on CI
	// tmux 3.4: exit 0, stdout the server pid, evaluated in a session-less fallback
	// context), so a read of the identity is only trustworthy if its answer carries
	// #{session_id} equal to $N. Here it carries none — and the production reader
	// must therefore reject it as unknown, not read it as a match or as absence.
	dm := observed["display-message"]
	if dm.exit != 0 {
		t.Errorf("display-message on a vanished id exited %d; expected a success with a context-less answer, got %+v", dm.exit, dm)
	}
	if strings.Contains(dm.stdout, idA) || len(strings.Fields(dm.stdout)) == 3 {
		t.Errorf("display-message on a vanished id returned something shaped like an identity: %+v", dm)
	}
	probe := NewTmuxSessionFromSanitizedName(name, "sleep 600")
	// A plausible-looking verified identity for the dead id: the pid and stamp of the
	// live server. Only the session id can tell the answers apart.
	livePID := must("display-message", "-p", "-t", exactTarget(name), "#{pid}")
	liveCreated := must("display-message", "-p", "-t", exactTarget(name), "#{session_created}")
	same, known, perr := probe.confirmIncarnation(&tmuxGeneration{sessionID: idA, serverPID: livePID, created: liveCreated})
	if same || known || perr == nil {
		t.Errorf("confirmIncarnation on a vanished id = (same=%v, known=%v, err=%v), want unknown", same, known, perr)
	}
	// The guard cannot be true for a vanished id because it compares #{session_id}:
	// whatever -t resolves to (nothing, an error, or some other session), the
	// true branch must not run for $idA.
	if got := observed["if-shell"]; got.stdout == "af-branch-true" {
		t.Errorf("an if-shell -F conditioned on #{session_id}==%s took the TRUE branch for the vanished id: %+v", idA, got)
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

	// 5. The guarded kill (#6258): the check and the act are ONE server-side
	// command. Verified here against the runner's tmux (the version is logged
	// above): the #{==:} and #{&&:} format operators, if-shell -F with a session-id
	// target, the branch commands, and a pane listing stamped with the server's
	// own pid and creation stamp.
	const guard = "af_id_semantics_guard"
	newSession(guard, "inst-guard")
	gid := sessionID(guard)
	pid := must("display-message", "-p", "-t", gid, "#{pid}")
	created := must("display-message", "-p", "-t", gid, "#{session_created}")
	condFor := func(p, c string) string {
		return fmt.Sprintf("#{&&:#{==:#{session_id},%s},#{&&:#{==:#{pid},%s},#{==:#{session_created},%s}}}", gid, p, c)
	}
	if got := must("display-message", "-p", "-t", gid, condFor(pid, created)); got != "1" {
		t.Fatalf("the verified pid and creation stamp must evaluate true, got %q", got)
	}
	for name, cond := range map[string]string{
		"a different server pid":     condFor(pid+"1", created),
		"a different creation stamp": condFor(pid, created+"1"),
		"a different session id":     strings.Replace(condFor(pid, created), gid, "$999999", 1),
	} {
		if got := must("display-message", "-p", "-t", gid, cond); got != "0" {
			t.Fatalf("%s must evaluate false, got %q", name, got)
		}
	}
	rows := must("list-panes", "-s", "-t", gid, "-F", "#{session_id}|#{pid}|#{session_created}|#{pane_pid}")
	if !strings.HasPrefix(rows, gid+"|"+pid+"|"+created+"|") {
		t.Fatalf("a pane row must carry the server pid and creation stamp, got %q", rows)
	}
	guarded := func(cond string) string {
		t.Helper()
		return must("if-shell", "-F", "-t", gid, cond,
			"display-message -p -t '"+gid+"' af-reap-fired ; kill-session -t '"+gid+"'",
			"display-message -p -t '"+gid+"' af-reap-skipped")
	}
	for name, cond := range map[string]string{
		"a different server pid":     condFor(pid+"1", created),
		"a different creation stamp": condFor(pid, created+"1"),
		"a different session id":     strings.Replace(condFor(pid, created), gid, "$999999", 1),
	} {
		if got := guarded(cond); got != "af-reap-skipped" || !alive(guard) {
			t.Fatalf("the guard with %s must take the false branch and kill nothing; printed %q, alive=%v", name, got, alive(guard))
		}
	}
	if got := guarded(condFor(pid, created)); got != "af-reap-fired" {
		t.Fatalf("the verified guard must take the true branch and print its marker, printed %q", got)
	}
	if alive(guard) {
		t.Fatal("the true branch printed its marker but the session is still alive")
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
