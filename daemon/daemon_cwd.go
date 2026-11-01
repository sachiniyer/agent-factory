package daemon

import (
	"os"
	"path/filepath"

	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// chdirToNeutralHome moves the daemon off whatever cwd the spawning process
// handed it and onto the AF home, so no daemon-spawned process can inherit a
// managed worktree as its cwd. See runDaemon for the full rationale; this is
// the single helper that holds the property "no daemon-spawned process can
// have a worktree as its cwd unless that worktree is the one it's working on"
// for every exec.Command site the daemon forks without setting cmd.Dir.
// Best-effort and non-fatal: a resolution failure leaves the inherited cwd,
// which under systemd is / and under an ad-hoc start is the user's.
func chdirToNeutralHome() {
	dir, ok := configHomeDir()
	if !ok {
		return
	}
	// Record the daemon's launch cwd before chdir'ing so the git runners can
	// resolve a relative persisted path (NewGitWorktreeFromStorage stores paths
	// verbatim) against it rather than the AF home we are about to move into.
	// Without this the chdir would make a relative `-C path` resolve beneath the
	// AF home and break the restored session (see daemonLaunchCwd in
	// session/git/worktree_git.go).
	if cwd, err := os.Getwd(); err == nil {
		sessiongit.SetDaemonLaunchCwd(cwd)
	}
	// A relative AGENT_FACTORY_HOME (ConfigDirFor preserves a non-empty value
	// verbatim, e.g. "af-home") must NOT chdir. Two externally-visible consumers
	// hold the RELATIVE value and resolve it against the daemon's cwd, so
	// moving that cwd breaks them:
	//
	//   - classifyDaemonHome reads the daemon's home out of /proc/<pid>/environ,
	//     which is FIXED AT EXEC and keeps the relative spelling for the life of
	//     the process (os.Setenv does not rewrite it), and resolves it against
	//     /proc/<pid>/cwd. Chdir'ing to the absolutized home moves that cwd to
	//     <launch-cwd>/af-home, so the classifier resolves <launch-cwd>/af-home
	//     + "af-home" = <launch-cwd>/af-home/af-home, compares unequal to the
	//     caller's home, and marks the LIVE daemon foreign — the normal
	//     PID-based StopDaemon then removes its PID file and leaves it running.
	//   - log.Initialize runs before RunDaemon (commands/root.go), so the
	//     rotating writer caches the relative log path "af-home/agent-factory.log"
	//     against the launch cwd. A later size-triggered rotation after a chdir
	//     reopens it against the NEW cwd → <home>/af-home/agent-factory.log, a
	//     nonexistent nested path, and silently falls back to stderr.
	//
	// Keeping the launch cwd leaves the original resolution frame externally
	// verifiable via /proc/<pid>/cwd, so both the classifier and the log writer
	// keep resolving the relative value the way the spawner did, and
	// config.GetConfigDir() (which resolves a relative value against the cwd)
	// keeps naming the same home — no nesting, no foreign classification. The
	// reaper fix this function exists for still applies to the normal
	// absolute-home case, which is the only placement the reaper hazard
	// (#5206) actually arises in. A relative home whose launch cwd is a managed
	// worktree resolves the home INSIDE that worktree — an unsupported placement
	// where chdir-to-home is not neutral either (the reaper matches cwd "at or
	// under" the worktree), already accepted as out of scope; the git runners
	// keep their own cmd.Dir as defence in depth so their children are never
	// false positives regardless.
	if !filepath.IsAbs(dir) {
		return
	}
	_ = os.Chdir(dir)
}

// chdirToNeutralHomeFn is the injection point runDaemon calls. Tests that run
// RunDaemon in-process stub it to a no-op so the process-wide os.Chdir does
// not leak into later tests' cwd assumptions (a temp AF home a test set via
// t.Setenv is removed on cleanup, leaving the process cwd pointing at a
// deleted directory — the "getwd: no such file or directory" failure). The
// daemon_cwd_test.go tests call chdirToNeutralHome directly to exercise the
// real behaviour.
var chdirToNeutralHomeFn = chdirToNeutralHome
