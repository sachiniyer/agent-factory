package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestChdirToNeutralHome_MovesOffWorktree is the source-level half of the
// inherited-cwd fix (#5206): the daemon must chdir off whatever cwd the
// spawning `af` invocation handed it onto the AF home, so no daemon-spawned
// process (tmux, gh, hooks, watch tasks, and the exec.Command sites outside
// session/git) can inherit a managed worktree as its cwd and become a false
// positive for the worktree writer-reaper's cwd match. The git runners keep
// their own cmd.Dir as defence in depth; this is the property that holds for
// the whole class — "no daemon-spawned process can have a worktree as its cwd
// unless that worktree is the one it's working on".
//
// It models the hazard directly: set the daemon's "inherited" cwd to a
// worktree (a temp dir standing in for a managed worktree the auto-start `af`
// was running in), then call chdirToNeutralHome and assert the cwd moved to
// the AF home and is no longer under the worktree. Deterministic in both
// directions: with the chdir the cwd is the home, without it the cwd stays
// the worktree. t.Chdir restores the test's cwd on exit so the rest of the
// package run is unaffected.
func TestChdirToNeutralHome_MovesOffWorktree(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	// The daemon was spawned from inside a managed worktree; model that by
	// making the test process's cwd the worktree.
	worktree := testguard.CanonicalTempDir(t)
	t.Chdir(worktree)
	if got, _ := os.Getwd(); canonical(t, got) != canonical(t, worktree) {
		t.Fatalf("sanity: cwd before chdir = %s, want the worktree %s", got, worktree)
	}

	chdirToNeutralHome()

	got, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	gotC := canonical(t, got)
	wantC := canonical(t, home)
	if gotC != wantC {
		t.Fatalf("daemon cwd after chdirToNeutralHome = %s, want the AF home %s; the daemon must "+
			"not keep the managed worktree (%s) as its cwd", got, home, worktree)
	}
	if gotC == canonical(t, worktree) {
		t.Fatalf("daemon cwd is still the worktree %s after chdirToNeutralHome", worktree)
	}
}

// TestChdirToNeutralHome_UnresolvableHomeLeavesCwd is the fail-soft companion:
// when the AF home cannot be resolved (an invalid AGENT_FACTORY_HOME the
// resolver rejects), chdirToNeutralHome is best-effort and must leave the
// inherited cwd in place rather than failing or chdir'ing to a literal "~"
// path. Under systemd that inherited cwd is /; under an ad-hoc start it is
// the user's — neither a managed worktree in practice, and the reaper
// excludes the scanning process itself.
func TestChdirToNeutralHome_UnresolvableHomeLeavesCwd(t *testing.T) {
	worktree := testguard.CanonicalTempDir(t)
	t.Chdir(worktree)
	// "~user" is a form ConfigDirFor rejects, so configHomeDir returns false.
	t.Setenv("AGENT_FACTORY_HOME", "~nobody")

	chdirToNeutralHome()

	got, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if canonical(t, got) != canonical(t, worktree) {
		t.Fatalf("daemon cwd changed to %s after chdirToNeutralHome with an unresolvable home; "+
			"a resolution failure must leave the inherited cwd (%s) in place", got, worktree)
	}
}

// TestChdirToNeutralHome_RelativeHomeStaysResolvable is the relative-home half
// of the inherited-cwd fix. AGENT_FACTORY_HOME may be spelled relative to the
// spawner's cwd (an operator may write "af-home"), and ConfigDirFor preserves a
// non-empty value verbatim. chdirToNeutralHome must NOT chdir for a relative
// home: the daemon's /proc/<pid>/environ is fixed at exec and keeps the relative
// spelling for the life of the process, so classifyDaemonHome resolves it against
// /proc/<pid>/cwd, and the rotating log writer caches the relative log path
// against the launch cwd. A chdir to the absolutized home would move the
// daemon's cwd to <launch-cwd>/af-home while both of those consumers still hold
// the relative value, so the classifier would resolve <launch-cwd>/af-home/af-home
// and mark the live daemon foreign (StopDaemon drops its PID file and leaves it
// running) and a size-triggered log rotation would reopen a nested, nonexistent
// <home>/af-home/agent-factory.log and fall back to stderr. Keeping the launch
// cwd leaves the original frame externally verifiable via /proc/<pid>/cwd, so
// both consumers keep resolving the relative value the way the spawner did, and
// config.GetConfigDir() keeps naming the same home — no nesting. Deterministic
// in both directions: with the guard the cwd and the home stay stable, without it
// the chdir nests GetConfigDir() and breaks the classifier's frame.
func TestChdirToNeutralHome_RelativeHomeStaysResolvable(t *testing.T) {
	home := testguard.SocketTempDir(t)
	// Spell the home RELATIVE to the spawner's cwd, the way an operator may.
	rel := filepath.Base(home)
	t.Chdir(filepath.Dir(home))
	t.Setenv("AGENT_FACTORY_HOME", rel)

	chdirToNeutralHome()

	got, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// The daemon must NOT chdir for a relative home: the launch cwd is the
	// frame the classifier (/proc/<pid>/environ + /proc/<pid>/cwd) and the log
	// writer's cached relative path both resolve against, so it must stay
	// externally verifiable. chdir'ing to the absolutized home would move
	// /proc/<pid>/cwd off that frame.
	if canonical(t, got) != canonical(t, filepath.Dir(home)) {
		t.Fatalf("daemon cwd after chdirToNeutralHome = %s, want the launch cwd %s (unchanged); a "+
			"relative AGENT_FACTORY_HOME must keep the launch frame so /proc/<pid>/environ and the log "+
			"writer keep resolving the relative value the way the spawner did", got, filepath.Dir(home))
	}
	// AGENT_FACTORY_HOME must stay relative and unmutated: an os.Setenv to the
	// absolutized home would not rewrite the immutable /proc/<pid>/environ the
	// classifier reads, so it would only desync the runtime env from that
	// externally-visible value.
	if env := os.Getenv("AGENT_FACTORY_HOME"); env != rel {
		t.Fatalf("AGENT_FACTORY_HOME after chdirToNeutralHome = %q, want the relative %q unchanged; a "+
			"relative home must not be rewritten to an absolute value the classifier cannot see", env, rel)
	}
	resolved, err := config.GetConfigDir()
	if err != nil {
		t.Fatalf("GetConfigDir after chdir: %v", err)
	}
	// config.GetConfigDir() preserves the relative value verbatim; consumers
	// resolve it against the daemon's cwd. With the cwd unchanged above, that
	// resolution must name the same home — not a nested <home>/af-home a
	// chdir'd cwd would have produced. EvalSymlinks errors (and fails the
	// test) if the resolved path does not exist, so a nesting regression that
	// pointed at a nonexistent <home>/af-home is caught here too.
	if canonical(t, filepath.Join(got, resolved)) != canonical(t, home) {
		t.Fatalf("config.GetConfigDir() %q resolved against the daemon cwd %s = %s, want the same home %s; "+
			"a relative AGENT_FACTORY_HOME must not nest under the chdir'd cwd", resolved, got,
			filepath.Join(got, resolved), home)
	}
}
