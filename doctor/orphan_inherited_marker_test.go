package doctor

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// These tests pin #6578: a matching AF_HOME marker proves the SESSION was this
// home's — never that the process carrying the marker is. A second
// agent-factory install launched from inside one of this home's sessions
// inherits AF_SESSION/AF_HOME beside its own AGENT_FACTORY_HOME, so its
// daemon, its tmux server and everything they spawn read exactly like this
// home's orphans — and `af doctor --fix` armed kills on them. The kill arm
// must establish ownership from the process's OWN environment (resolved the
// way the daemon checks resolve which home a daemon serves) and from its
// ancestry, not from the marker alone.

// TestOrphanInheritedMarkerForeignOwnHome_NotKilled is the #6578 regression:
// AF_HOME names this home and AF_SESSION names a provably dead session, but
// the process's own AGENT_FACTORY_HOME names another install — the exact
// shape the leaked play-test debris on the maintainer's box carried. The
// marker was inherited, so the process is the other install's business:
// advisory, never armed.
func TestOrphanInheritedMarkerForeignOwnHome_NotKilled(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)
	foreignHome := t.TempDir()

	victim := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: "af_doctor-inherited-marker",
		tmux.EnvMarkerHome:    home,
		// The process's OWN home is the other install's — the matching
		// AF_HOME marker is only inherited session debris.
		"AGENT_FACTORY_HOME": foreignHome,
		"TMUX":               ambientTmuxEnv(t),
	})

	report, err := Run(testOptionsWithHome(t, home, true, victim.PID)) // Fix: true
	require.NoError(t, err)

	require.Empty(t, findByCheck(report, "orphaned-process"),
		"an inherited marker must not count the process among this home's orphans")
	foreign := findByCheck(report, "foreign-home-process")
	require.Len(t, foreign, 1,
		"a process whose own environment names another home must be attributed to it")
	require.Empty(t, foreign[0].FixAction,
		"another install's process must never be armed: %s", foreign[0].Detail)
	require.False(t, foreign[0].Actionable,
		"another install's process must not drive the exit code")
	require.Contains(t, foreign[0].Detail, foreignHome,
		"the finding must name the home the process actually belongs to: %s", foreign[0].Detail)
	require.True(t, alive(victim),
		"`af doctor --fix` killed another install's process on an inherited marker (#6578)")
}

// TestOrphanInheritedMarkerMatchingHomes_StillArmed is the control: the same
// shape with the process's own AGENT_FACTORY_HOME resolving to THIS home stays
// armed — the guard must not disarm a genuine same-home orphan.
func TestOrphanInheritedMarkerMatchingHomes_StillArmed(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)

	orphan := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: "af_doctor-own-home-orphan",
		tmux.EnvMarkerHome:    home,
		"AGENT_FACTORY_HOME":  home,
		"TMUX":                ambientTmuxEnv(t),
	})

	report, err := Run(testOptionsWithHome(t, home, true, orphan.PID)) // Fix: true
	require.NoError(t, err)

	findings := findByCheck(report, "orphaned-process")
	require.Len(t, findings, 1)
	require.NotEmpty(t, findings[0].FixAction,
		"a process whose own home is this install's must still arm the kill")
	require.True(t, findings[0].Fixed, "fix outcome: %v", findings[0].FixErr)
	require.Eventually(t, func() bool { return !alive(orphan) }, 5*time.Second, 25*time.Millisecond,
		"a genuine same-home orphan must still be killed by --fix")
}

// TestOrphanInheritedMarkerUnsetHomeDefault_StillArmed is the unset half of the
// control: a process carrying NO AGENT_FACTORY_HOME resolves its home the way a
// daemon's is resolved — <its HOME>/.agent-factory — which is how every pane
// process of a normal install reads. When that default IS this home the orphan
// stays armed; the guard may only disarm on a positively different home.
func TestOrphanInheritedMarkerUnsetHomeDefault_StillArmed(t *testing.T) {
	testguard.IsolateTmux(t)

	// home sits at <dir>/.agent-factory so the child's HOME-derived default
	// resolves to this install — the production shape of a same-home orphan
	// with no AGENT_FACTORY_HOME of its own.
	dir := t.TempDir()
	home := filepath.Join(dir, ".agent-factory")
	require.NoError(t, os.MkdirAll(home, 0o755))

	orphan := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: "af_doctor-home-default-orphan",
		tmux.EnvMarkerHome:    home,
		"AGENT_FACTORY_HOME":  "", // unset: the HOME-derived default applies
		"HOME":                dir,
		"TMUX":                ambientTmuxEnv(t),
	})

	report, err := Run(testOptionsWithHome(t, home, true, orphan.PID)) // Fix: true
	require.NoError(t, err)

	findings := findByCheck(report, "orphaned-process")
	require.Len(t, findings, 1)
	require.NotEmpty(t, findings[0].FixAction,
		"an orphan whose unset AGENT_FACTORY_HOME defaults to this home must stay armed")
	require.True(t, findings[0].Fixed, "fix outcome: %v", findings[0].FixErr)
	require.Eventually(t, func() bool { return !alive(orphan) }, 5*time.Second, 25*time.Millisecond,
		"a same-home orphan by HOME-derived default must still be killed by --fix")
}

// TestOrphanInheritedMarkerForeignAncestor_NotKilled covers the ancestry half
// of #6578: a process whose own environment resolves to this home but which
// runs under another install's tmux server is that install's business, not
// this home's orphan — an env scrub cannot erase the process tree it lives
// in. The companion half pins the asymmetry: an ancestor that DECLARES no
// agent-factory home is just some process and must not disarm the kill for
// its descendants (a HOME-derived guess would let pid 1 or a login shell
// neuter the arm for everything beneath it).
func TestOrphanInheritedMarkerForeignAncestor_NotKilled(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)
	foreignHome := t.TempDir()

	// Two ancestors staged into the snapshot: another install's tmux server
	// (AGENT_FACTORY_HOME declared for the other home) and a bystander that
	// declares nothing. The pids name no real process — the snapshot is
	// patched below and the env reads come from the stub, so nothing real is
	// ever inspected or signalled.
	foreignServerPID := 1 << 30
	bystanderPID := 1<<30 + 1
	stubProcessEnv(t, foreignServerPID, map[string]string{"AGENT_FACTORY_HOME": foreignHome})
	stubProcessEnv(t, bystanderPID, map[string]string{"HOME": t.TempDir()})

	under := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: "af_doctor-under-foreign-server",
		tmux.EnvMarkerHome:    home,
		"AGENT_FACTORY_HOME":  home, // own env resolves to THIS install
		"TMUX":                ambientTmuxEnv(t),
	})
	free := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: "af_doctor-under-plain-ancestor",
		tmux.EnvMarkerHome:    home,
		"AGENT_FACTORY_HOME":  home,
		"TMUX":                ambientTmuxEnv(t),
	})

	opts := testOptionsWithHome(t, home, true, under.PID, free.PID) // Fix: true
	base := opts.snapshot
	opts.snapshot = func() (map[int]proctree.Process, error) {
		snap, err := base()
		if err != nil {
			return nil, err
		}
		if v, ok := snap[under.PID]; ok {
			v.PPID = foreignServerPID
			snap[under.PID] = v
		}
		if v, ok := snap[free.PID]; ok {
			v.PPID = bystanderPID
			snap[free.PID] = v
		}
		snap[foreignServerPID] = proctree.Process{PID: foreignServerPID, PPID: 1, Comm: "tmux: server"}
		snap[bystanderPID] = proctree.Process{PID: bystanderPID, PPID: 1, Comm: "sh"}
		return snap, nil
	}

	report, err := Run(opts)
	require.NoError(t, err)

	require.True(t, alive(under),
		"`af doctor --fix` killed a process under another install's tmux server (#6578)")
	foreign := findByCheck(report, "foreign-home-process")
	require.Len(t, foreign, 1,
		"a process under another install's tree must be attributed to it, not orphaned here")
	require.Empty(t, foreign[0].FixAction,
		"a process under another install's tree must never be armed: %s", foreign[0].Detail)
	require.Contains(t, foreign[0].Detail, foreignHome,
		"the finding must name the home the ancestor declares: %s", foreign[0].Detail)

	// The bystander ancestor declares no agent-factory home, so the orphan
	// beneath it is still proven by its own environment: armed and fixed.
	orphans := findByCheck(report, "orphaned-process")
	require.Len(t, orphans, 1)
	require.NotEmpty(t, orphans[0].FixAction)
	require.True(t, orphans[0].Fixed, "fix outcome: %v", orphans[0].FixErr)
	require.Eventually(t, func() bool { return !alive(free) }, 5*time.Second, 25*time.Millisecond,
		"an orphan whose ancestors declare no foreign home must still be killed by --fix")
}

// TestForeignDaemonWithInheritedMarkers_ChecksAgree pins the cross-check
// agreement #6578 requires: a daemon serving another home is left alone by
// foreign-daemon, and the orphan arm must not arm a kill on the same process
// in the same run — even though its inherited AF_SESSION/AF_HOME markers name
// a dead session of this install.
func TestForeignDaemonWithInheritedMarkers_ChecksAgree(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)
	foreignHome := t.TempDir()

	// The debris daemon from the issue: another install's `af --daemon`, its
	// own AGENT_FACTORY_HOME intact, this home's session markers inherited.
	daemonProc := spawnWithEnv(t, "af", []string{"--daemon"}, map[string]string{
		tmux.EnvMarkerSession: "af_doctor-inherited-daemon",
		tmux.EnvMarkerHome:    home,
		"AGENT_FACTORY_HOME":  foreignHome,
		"TMUX":                ambientTmuxEnv(t),
	})
	stubDaemonProcessArgv(t, func(pid int) []string {
		if pid == daemonProc.PID {
			return []string{"af", "--daemon"}
		}
		return nil
	})

	report, err := Run(testOptionsWithHome(t, home, true, daemonProc.PID)) // Fix: true
	require.NoError(t, err)

	foreignDaemons := findByCheck(report, "foreign-daemon")
	require.Len(t, foreignDaemons, 1,
		"a daemon serving another existing home must be reported by foreign-daemon")
	require.Contains(t, foreignDaemons[0].Detail, "left alone",
		"foreign-daemon must decline to act on the other install's daemon: %s", foreignDaemons[0].Detail)
	require.Empty(t, foreignDaemons[0].FixAction)

	require.Empty(t, findByCheck(report, "orphaned-process"),
		"the orphan arm must agree with foreign-daemon: the process is not this home's orphan")
	for _, f := range findByCheck(report, "foreign-home-process") {
		require.Empty(t, f.FixAction,
			"a daemon foreign-daemon leaves alone must never be armed by the orphan arm: %s", f.Detail)
		require.False(t, f.Fixed)
	}
	require.True(t, alive(daemonProc),
		"`af doctor --fix` killed another install's daemon that foreign-daemon left alone (#6578)")
}
