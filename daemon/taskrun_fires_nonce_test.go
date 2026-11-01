package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
)

// The fake-tmux harness below descends from Detail's #6252 (installFakeTmuxForEvidence),
// which showed these probes can be answered hermetically by a fake `tmux` on
// PATH. It is rewritten around the proof #6258 settled on: the instance id the
// fire recorded BEFORE its spawn, read from the probed incarnation's own
// environment, and a kill bound to that incarnation's tmux session id.

// fakeReapTmux describes how the fake server's one session answers.
type fakeReapTmux struct {
	// instance is the AF_INSTANCE_ID the session carries; "" means it carries none
	// (a session from before the marker existed).
	instance string
	// home is the AF_HOME it carries.
	home string
	// unreadable makes every show-environment fail with an error that is not
	// "unknown variable".
	unreadable bool
	// absent makes `tmux ls` report no sessions at all — the leftover under test
	// has no live runtime (a worktree-only leftover, #6259).
	absent bool
}

// installFakeReapTmux puts a fake `tmux` first on PATH for the test's duration and
// returns the file every invocation's argv is appended to. The fake never starts a
// server and cannot touch a real one.
func installFakeReapTmux(t *testing.T, sanitized string, spec fakeReapTmux) (logPath string) {
	t.Helper()
	dir := resolvedTempDir(t)
	logPath = filepath.Join(dir, "tmux.log")
	instanceBranch := `echo "unknown variable: AF_INSTANCE_ID" >&2; exit 1`
	if spec.instance != "" {
		instanceBranch = `echo "AF_INSTANCE_ID=` + spec.instance + `"`
	}
	envGuard := ""
	if spec.unreadable {
		envGuard = `echo "protocol version mismatch" >&2; exit 1`
	}
	lsAnswer := `echo '` + sanitized + `'`
	if spec.absent {
		lsAnswer = `:`
	}
	listPanesAnswer := `:`

	script := `#!/bin/sh
echo "$*" >> '` + logPath + `'
case "$1" in
ls) ` + lsAnswer + ` ;;
display-message)
  for last; do :; done
  case "$last" in
    '#{session_id} #{pid} #{session_created}') echo '$7 4242 1700000000' ;;
    '#{pid} #{session_created}') echo '4242 1700000000' ;;
    *) exit 1 ;;
  esac ;;
list-panes)
  ` + listPanesAnswer + ` ;;
show-environment)
  ` + envGuard + `
  if [ -z "$4" ]; then exit 0; fi
  case "$4" in
    AF_INSTANCE_ID) ` + instanceBranch + ` ;;
    AF_HOME) echo 'AF_HOME=` + spec.home + `' ;;
    AF_SESSION) echo 'AF_SESSION=` + sanitized + `' ;;
    *) echo "unknown variable: $4" >&2; exit 1 ;;
  esac ;;
if-shell)
  # if-shell -F -t $N COND TRUE FALSE. The server evaluates COND as a format and
  # runs one branch. This fake's one session has a fixed id, server pid and creation
  # stamp, so the condition production sends is true exactly when it names all three
  # of them: the full string must equal the one built from those values.
  want='#{&&:#{==:#{session_id},$7},#{&&:#{==:#{pid},4242},#{==:#{session_created},1700000000}}}'
  if [ "$4" = '$7' ] && [ "$5" = "$want" ]; then
    echo 'af-reap-fired'
    echo "kill-session -t $4" >> '` + logPath + `'
  else
    echo 'af-reap-skipped'
  fi ;;
*) exit 0 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func fakeTmuxLog(t *testing.T, logPath string) []string {
	t.Helper()
	raw, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read fake tmux log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func killsIn(lines []string) []string {
	var kills []string
	for _, l := range lines {
		if strings.HasPrefix(l, "kill-session") {
			kills = append(kills, l)
		}
	}
	return kills
}

func fakeReapFixture(t *testing.T, title string) (repoPath string, repo *config.RepoContext, sanitized, home string) {
	t.Helper()
	home = testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	repoPath = setupTaskRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	return repoPath, repo, tmux.SanitizedNameForRepo(title, repo.IdentityPath()), home
}

const reapInstanceID = "11111111-2222-4333-8444-555555555555"

// TestTaskFireLeftover_ReapsOnlyAnIncarnationThatProvesTheFiresInstanceID is the
// table of every way the proof can fail, each asserted on the fake tmux's recorded
// argv: a session that does not prove this fire's spawn is never sent a
// kill-session, by id or by name, and its leftover is quarantined for inspection.
// The one passing row kills the probed incarnation by its tmux id.
func TestTaskFireLeftover_ReapsOnlyAnIncarnationThatProvesTheFiresInstanceID(t *testing.T) {
	cases := []struct {
		name string
		// markerInstance is what the fire's marker recorded before the spawn.
		markerInstance string
		session        func(home string) fakeReapTmux
		wantKill       bool
	}{
		{
			name:           "the session carries the instance id the fire recorded",
			markerInstance: reapInstanceID,
			session:        func(home string) fakeReapTmux { return fakeReapTmux{instance: reapInstanceID, home: home} },
			wantKill:       true,
		},
		{
			name:           "same name and home, a different instance id (a concurrent create)",
			markerInstance: reapInstanceID,
			session:        func(home string) fakeReapTmux { return fakeReapTmux{instance: "someone-elses-attempt", home: home} },
		},
		{
			name:           "no instance id on the session (created before the marker existed)",
			markerInstance: reapInstanceID,
			session:        func(home string) fakeReapTmux { return fakeReapTmux{home: home} },
		},
		{
			name:           "the fire recorded no instance id before its spawn",
			markerInstance: "",
			session:        func(home string) fakeReapTmux { return fakeReapTmux{instance: reapInstanceID, home: home} },
		},
		{
			name:           "another install's home",
			markerInstance: reapInstanceID,
			session:        func(string) fakeReapTmux { return fakeReapTmux{instance: reapInstanceID, home: "/some/other/af-home"} },
		},
		{
			name:           "an environment that cannot be read",
			markerInstance: reapInstanceID,
			session: func(home string) fakeReapTmux {
				return fakeReapTmux{instance: reapInstanceID, home: home, unreadable: true}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			title := "nonce-leftover"
			repoPath, repo, sanitized, home := fakeReapFixture(t, title)
			logPath := installFakeReapTmux(t, sanitized, tc.session(home))

			fire := &taskFireMarker{
				TaskID: "ab0r7140", IntendedAt: time.Now().Add(-time.Minute), Title: title, InstanceID: tc.markerInstance,
			}
			row, err := taskFireLeftoverRecord(
				&task.Task{ID: "ab0r7140", ProjectPath: repoPath, Prompt: "p", Program: "claude"},
				fire, repo, nil)
			if err != nil {
				t.Fatalf("taskFireLeftoverRecord: %v", err)
			}
			kills := killsIn(fakeTmuxLog(t, logPath))

			if tc.wantKill {
				if row != nil {
					t.Fatalf("a proven tmux-only leftover must reap to nil, got %+v", row)
				}
				if len(kills) != 1 || kills[0] != "kill-session -t $7" {
					t.Fatalf("the kill must target the probed incarnation id and nothing else; got %q", kills)
				}
				return
			}
			if len(kills) != 0 {
				t.Fatalf("an unproven session must not be killed; tmux was sent %q", kills)
			}
			if row == nil || !row.StartupStateUnknown || row.UserKilled {
				t.Fatalf("an unproven leftover must be quarantined (startup-state-unknown, no teardown), got %+v", row)
			}
			if row.TmuxName != sanitized {
				t.Fatalf("the quarantine record must name the session it stands for, got %q want %q", row.TmuxName, sanitized)
			}
		})
	}
}

// A fire that recorded no instance id has nothing to compare, so af does not even
// ask tmux about the session's environment.
func TestTaskFireLeftover_WithoutARecordedInstanceIDDoesNotProbeTheEnvironment(t *testing.T) {
	title := "no-recorded-id"
	repoPath, repo, sanitized, home := fakeReapFixture(t, title)
	logPath := installFakeReapTmux(t, sanitized, fakeReapTmux{instance: reapInstanceID, home: home})

	fire := &taskFireMarker{TaskID: "ab0r7141", IntendedAt: time.Now().Add(-time.Minute), Title: title}
	if _, err := taskFireLeftoverRecord(
		&task.Task{ID: "ab0r7141", ProjectPath: repoPath, Prompt: "p", Program: "claude"},
		fire, repo, nil); err != nil {
		t.Fatalf("taskFireLeftoverRecord: %v", err)
	}
	for _, line := range fakeTmuxLog(t, logPath) {
		if strings.HasPrefix(line, "show-environment") || strings.HasPrefix(line, "kill-session") {
			t.Fatalf("a fire with no recorded instance id must not read or touch the session: %q", line)
		}
	}
}

// Worktrees are never proven (#6258, option a): the only evidence for one was a
// registration timestamp, a clock. A fire that left a worktree is quarantined and
// the live session beside it is NOT killed, even when it proves the fire's id.
func TestTaskFireLeftover_AWorktreeLeftoverIsQuarantinedAndItsSessionKept(t *testing.T) {
	title := "worktree-leftover"
	repoPath, repo, sanitized, home := fakeReapFixture(t, title)
	cfg, err := config.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	branch := git.BranchForTitle(cfg.BranchPrefix, title)
	wtPath := filepath.Join(resolvedTempDir(t), "leftover-wt")
	if out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "-b", branch, wtPath).CombinedOutput(); err != nil {
		t.Fatalf("seed leftover worktree: %v: %s", err, out)
	}
	logPath := installFakeReapTmux(t, sanitized, fakeReapTmux{instance: reapInstanceID, home: home})

	fire := &taskFireMarker{
		TaskID: "ab0r7142", IntendedAt: time.Now().Add(-time.Minute), Title: title, InstanceID: reapInstanceID,
	}
	row, err := taskFireLeftoverRecord(
		&task.Task{ID: "ab0r7142", ProjectPath: repoPath, Prompt: "p", Program: "claude"},
		fire, repo, nil)
	if err != nil {
		t.Fatalf("taskFireLeftoverRecord: %v", err)
	}
	if kills := killsIn(fakeTmuxLog(t, logPath)); len(kills) != 0 {
		t.Fatalf("a worktree leftover is unproven; its session must not be killed, tmux was sent %q", kills)
	}
	if row == nil || !row.StartupStateUnknown || row.UserKilled {
		t.Fatalf("a worktree leftover must be quarantined, never tombstoned for teardown, got %+v", row)
	}
	if row.Worktree.WorktreePath != wtPath {
		t.Fatalf("the quarantine record must keep the worktree handle %q, got %q", wtPath, row.Worktree.WorktreePath)
	}
	if _, statErr := os.Stat(wtPath); statErr != nil {
		t.Fatalf("the worktree and the work in it must be left in place: %v", statErr)
	}
}

// seedRealLeftoverSession starts a session on an isolated real tmux server under the
// sanitized name with the markers a real af create stamps, plus the given extras.
func seedRealLeftoverSession(t *testing.T, sanitized, home string, extra ...string) {
	t.Helper()
	args := []string{"new-session", "-d", "-s", sanitized,
		"-e", tmux.EnvMarkerSession + "=" + sanitized,
		"-e", tmux.EnvMarkerGeneration + "=gen-real",
		"-e", tmux.EnvMarkerHome + "=" + home}
	args = append(args, extra...)
	args = append(args, "sh")
	if out, err := exec.Command("tmux", args...).CombinedOutput(); err != nil {
		t.Fatalf("seed tmux session: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+sanitized).Run() })
}

// TestTaskFireLeftoverRecord_ConcurrentCreateNotReaped is Detail's #6252
// regression, re-expressed for the nonce, against a real tmux server. A live
// session under the colliding name carries every marker a real create stamps —
// AF_SESSION, AF_HOME, AF_SESSION_GEN — and was created AFTER the intent, so every
// name- and clock-based check passes. It carries a different instance id, so it is
// not this fire's spawn and must survive the reconcile.
func TestTaskFireLeftoverRecord_ConcurrentCreateNotReaped(t *testing.T) {
	testguard.IsolateTmux(t)
	title := "concurrent-leftover"
	repoPath, repo, sanitized, home := fakeReapFixture(t, title)
	seedRealLeftoverSession(t, sanitized, home, "-e", tmux.EnvMarkerInstance+"=a-concurrent-attempt")

	fire := &taskFireMarker{
		TaskID: "ab0r7120", IntendedAt: time.Now().Add(-time.Minute), Title: title, InstanceID: reapInstanceID,
	}
	row, err := taskFireLeftoverRecord(
		&task.Task{ID: "ab0r7120", ProjectPath: repoPath, Prompt: "p", Program: "claude"},
		fire, repo, nil)
	if err != nil {
		t.Fatalf("taskFireLeftoverRecord: %v", err)
	}
	if row != nil && row.UserKilled {
		t.Fatalf("a concurrent create's live session must not be reaped, got a teardown tombstone: %+v", row)
	}
	if out, err := exec.Command("tmux", "has-session", "-t", "="+sanitized).CombinedOutput(); err != nil {
		t.Fatalf("a concurrent create's live session was killed by the reconcile: %v: %s", err, out)
	}
}

// TestTaskFireLeftoverRecord_TmuxOnlyProvenReap is Detail's genuine-leftover case
// against a real tmux server: the session carries the instance id the fire
// recorded, so the reconcile stops it and leaves nothing behind.
func TestTaskFireLeftoverRecord_TmuxOnlyProvenReap(t *testing.T) {
	testguard.IsolateTmux(t)
	title := "tmux-only-leftover"
	repoPath, repo, sanitized, home := fakeReapFixture(t, title)
	seedRealLeftoverSession(t, sanitized, home, "-e", tmux.EnvMarkerInstance+"="+reapInstanceID)

	fire := &taskFireMarker{
		TaskID: "ab0r7121", IntendedAt: time.Now().Add(-time.Minute), Title: title, InstanceID: reapInstanceID,
	}
	row, err := taskFireLeftoverRecord(
		&task.Task{ID: "ab0r7121", ProjectPath: repoPath, Prompt: "p", Program: "claude"},
		fire, repo, nil)
	if err != nil {
		t.Fatalf("taskFireLeftoverRecord: %v", err)
	}
	if row != nil {
		t.Fatalf("a proven tmux-only leftover must reap to nil (nothing left to record), got %+v", row)
	}
	if out, err := exec.Command("tmux", "has-session", "-t", "="+sanitized).CombinedOutput(); err == nil {
		t.Fatalf("the proven leftover's runtime must be stopped inside reconcile, still alive: %s", out)
	}
}

// TestTaskFireLeftoverRecord_PreNonceSessionNotReaped: a session af launched before
// AF_INSTANCE_ID existed carries AF_SESSION, AF_HOME and a generation but no
// instance id. After an upgrade it cannot be proven this fire's, so it is left
// running and quarantined (#6258).
func TestTaskFireLeftoverRecord_PreNonceSessionNotReaped(t *testing.T) {
	testguard.IsolateTmux(t)
	title := "pre-nonce-leftover"
	repoPath, repo, sanitized, home := fakeReapFixture(t, title)
	seedRealLeftoverSession(t, sanitized, home)

	fire := &taskFireMarker{
		TaskID: "ab0r7122", IntendedAt: time.Now().Add(-time.Minute), Title: title, InstanceID: reapInstanceID,
	}
	row, err := taskFireLeftoverRecord(
		&task.Task{ID: "ab0r7122", ProjectPath: repoPath, Prompt: "p", Program: "claude"},
		fire, repo, nil)
	if err != nil {
		t.Fatalf("taskFireLeftoverRecord: %v", err)
	}
	if row == nil || !row.StartupStateUnknown || row.UserKilled {
		t.Fatalf("a pre-nonce session must be quarantined, got %+v", row)
	}
	if out, err := exec.Command("tmux", "has-session", "-t", "="+sanitized).CombinedOutput(); err != nil {
		t.Fatalf("a session that cannot be proven this fire's must not be killed: %v: %s", err, out)
	}
}
