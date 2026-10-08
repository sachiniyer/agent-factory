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
// non-empty value verbatim. chdirToNeutralHome must absolutize it before
// chdir'ing and fix the env to that absolute path; otherwise the chdir moves
// the daemon into <launch-cwd>/af-home while the env stays relative, and every
// later config.GetConfigDir() resolves "af-home" against the NEW cwd (the home
// itself) — yielding a nested, nonexistent <home>/af-home that breaks
// control-socket binding and the home watcher. Deterministic in both
// directions: with the absolutize the home stays stable, without it
// GetConfigDir() nests and resolves to a path that does not exist.
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
	if canonical(t, got) != canonical(t, home) {
		t.Fatalf("daemon cwd after chdirToNeutralHome = %s, want the AF home %s; a relative "+
			"AGENT_FACTORY_HOME must chdir into the home it names, not leave the inherited cwd", got, home)
	}
	resolved, err := config.GetConfigDir()
	if err != nil {
		t.Fatalf("GetConfigDir after chdir: %v", err)
	}
	if canonical(t, resolved) != canonical(t, home) {
		t.Fatalf("config.GetConfigDir() after chdirToNeutralHome = %s, want the same home %s; "+
			"a relative AGENT_FACTORY_HOME must not nest under the chdir'd cwd", resolved, home)
	}
}
