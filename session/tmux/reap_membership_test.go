package tmux

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// #7435 item 2 — classifyHomeDir replaces the lexical filepath.Clean compare
// on every kill-path AF_HOME check. Only literal equality or a SameFile stat
// proves "same directory"; a path that merely Cleans to the home, a relative
// marker, or anything unstat-able is unproven — and unproven is never same.
func TestClassifyHomeDir(t *testing.T) {
	home := t.TempDir()
	other := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(home, link))
	// Cleans to the home, but resolves through a REGULAR FILE — path
	// traversal through a non-directory fails with ENOTDIR on every platform,
	// so nothing can prove the names name the same place. The ".." must be
	// built into the string by hand: filepath.Join CLEANS its result, so a
	// Join-built alias collapses to the home literally and the fixture tests
	// literal equality instead of unprovability (#7435 CI).
	notDir := filepath.Join(filepath.Dir(home), "not-a-dir")
	require.NoError(t, os.WriteFile(notDir, []byte("x"), 0o600))
	lexicalAlias := notDir + string(os.PathSeparator) + ".." + string(os.PathSeparator) + filepath.Base(home)
	require.Equal(t, home, filepath.Clean(lexicalAlias),
		"the fixture must only differ lexically, not after Clean")

	for _, tc := range []struct {
		name string
		a, b string
		want homeMatch
	}{
		{"literal equality", home, home, homeSame},
		{"a symlink to the home stats to it", link, home, homeSame},
		{"two different existing dirs", other, home, homeForeign},
		{"a lexical alias that cannot be stat'd", lexicalAlias, home, homeUnproven},
		{"a missing marker path", filepath.Join(home, "missing"), home, homeUnproven},
		{"a relative marker", ".af", home, homeUnproven},
		{"a relative home", home, ".af", homeUnproven},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, classifyHomeDir(tc.a, tc.b))
		})
	}
}

// #7435 item 5 — the cohort gate admits only what kernel evidence proves:
// capture inside the proven live tree, a SID one of the pane roots led, a
// (pid, StartID) recorded at spawn, or descent from a proven member. No
// evidence at all means nothing is admitted: refuse, never signal (R2).
func TestCohortMembershipAdmits(t *testing.T) {
	member := proctree.Process{PID: 100, SID: 40, StartID: 9}

	require.False(t, cohortMembership{}.admits(nil, member),
		"no evidence: nothing is admitted — markers alone never prove membership")
	require.False(t, cohortMembership{PaneSIDs: map[int]bool{}}.admits(nil, member),
		"an EMPTY sid set is still no evidence: nothing is admitted")
	require.True(t, cohortMembership{PaneSIDs: map[int]bool{40: true}}.admits(nil, member))
	require.True(t, cohortMembership{Recorded: map[int]uint64{100: 9}}.admits(nil, member))
	require.True(t, cohortMembership{Proven: map[int]uint64{100: 9}}.admits(nil, member),
		"captured inside the proven live tree is membership proof on its own")
	require.False(t, cohortMembership{PaneSIDs: map[int]bool{41: true}}.admits(nil, member),
		"a foreign SID is not membership however the markers read")
	require.False(t, cohortMembership{Recorded: map[int]uint64{100: 8}}.admits(nil, member),
		"a recycled pid with a different StartID is not the recorded process")
	require.False(t, cohortMembership{Recorded: map[int]uint64{101: 9}}.admits(nil, member),
		"a different pid carrying the same StartID is not the recorded process")
	require.False(t, cohortMembership{Proven: map[int]uint64{100: 8}}.admits(nil, member),
		"a recycled pid with a different StartID was never inside the tree")
}

// A process never itself captured still proves membership by descending from
// one that was — the shape a refresh pass finds when a proven leftover forks a
// child during the sweep.
func TestCohortMembershipAdmitsByDescentFromTheProvenTree(t *testing.T) {
	proven := proctree.Process{PID: 50, PPID: 1, SID: 50, StartID: 7}
	child := proctree.Process{PID: 60, PPID: 50, SID: 60, StartID: 8}
	snap := map[int]proctree.Process{50: proven, 60: child}
	membership := cohortMembership{Proven: map[int]uint64{50: 7}}

	require.True(t, membership.admits(snap, child),
		"a child of a proven member is inside the proven tree whatever the env scan says")
	require.False(t, cohortMembership{Proven: map[int]uint64{50: 99}}.admits(snap, child),
		"a reused ancestor pid with a different StartID proves nothing")
	require.False(t, cohortMembership{PaneSIDs: map[int]bool{40: true}}.admits(snap, child),
		"descent cannot help without a proven member to reach")
}

// #7435 item 5 — inherited markers are never membership proof. Three live
// helpers carry identical AF_SESSION/AF_HOME/AF_SESSION_GEN markers:
//
//   - a survivor still in the recorded pane root's kernel session: a member,
//     admitted by its SID;
//   - a same-shaped survivor whose leader's session was never recorded: the
//     inherited-marker case — REPORTED, not silently dropped (R3), because
//     "nothing found" on this path authorizes worktree cleanup;
//   - a setsid'd child of the reaper itself: positively placed in the
//     reaper's own subtree, so it is excluded silently the way signalling
//     excludes it — neither a member nor a reported leak.
func TestMarkedOrphanProcessesRequiresSessionMembershipBeyondMarkers(t *testing.T) {
	name := "af_membership"
	home := t.TempDir()
	gen := "gen-membership"

	root, member := spawnMarkedLeaderWithOrphanedChild(t, name, home, gen)
	_, unadmitted := spawnMarkedLeaderWithOrphanedChild(t, name, home, gen)
	ownChild := spawnCohortSleeper(t, name, home, gen, true)
	membership := cohortMembership{
		PaneSIDs: map[int]bool{root.PID: true},
		Recorded: map[int]uint64{root.PID: root.StartID},
	}

	marked, _, err := markedOrphanProcesses(
		[]proctree.Process{member, unadmitted, ownChild}, name, home,
		fixtureCohort(gen), membership)
	require.Error(t, err, "an unadmitted marker-carrier is reported, not silently skipped")
	require.Contains(t, err.Error(), "cannot be proven a member")
	require.Contains(t, err.Error(), strconv.Itoa(unadmitted.PID),
		"the report names the carrier it could not place")
	require.NotContains(t, err.Error(), strconv.Itoa(ownChild.PID),
		"the reaper's own descendant is positively placed — excluded, not reported")
	require.Equal(t, []proctree.Process{member}, marked,
		"only the survivor still in the recorded root's kernel session is admitted")

	require.True(t, proctree.AliveSame(member))
	require.True(t, proctree.AliveSame(unadmitted))
	require.True(t, proctree.AliveSame(ownChild))
}

// #7435 Codex — an UNPROVABLE home is never "not ours, carry on". A survivor
// whose AF_HOME is relative (.af) can name this home or another's, so the
// vanished-session sweep must refuse on it: error, not an empty survivor set
// — the empty answer is what licensed deleting the worktree underneath it.
func TestVanishedSessionSweepRefusesAnUnprovableHomeSurvivor(t *testing.T) {
	name := "af_unproven_home"
	ownHome := t.TempDir()
	gen := "gen-unproven"
	survivor := spawnCohortSleeper(t, name, ".af", gen, false)

	err := reapVanishedSessionProcesses(name, ownHome,
		[]proctree.Process{survivor}, nil, cohortMembership{}, false)
	require.Error(t, err, "an unprovable home must refuse the sweep, not silently clear it")
	require.Contains(t, err.Error(), "cannot be proven",
		"the refusal must name the attribution failure, not a generic skip")
	require.True(t, proctree.AliveSame(survivor),
		"the survivor stays alive AND the cleanup is not authorized — both halves of fail-closed")

	// The same process read as a candidate directly shows the per-pid split:
	// foreign is skipped by proof, unproven blocks.
	marked, _, inspectErr := markedOrphanProcesses(
		[]proctree.Process{survivor}, name, ownHome, fixtureCohort(gen), cohortMembership{})
	require.Error(t, inspectErr)
	require.Contains(t, inspectErr.Error(), "cannot be proven")
	require.Empty(t, marked)
}

// spawnCohortSleeper starts a live sleeper carrying this session's full
// marker environment, setsid'd out of it when asked.
func spawnCohortSleeper(t *testing.T, name, home, gen string, newSession bool) proctree.Process {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	cmd.Env = append(os.Environ(),
		EnvMarkerSession+"="+name,
		EnvMarkerHome+"="+home,
		EnvMarkerGeneration+"="+gen,
	)
	if newSession {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	var p proctree.Process
	var err error
	require.Eventually(t, func() bool {
		p, err = proctree.Lookup(cmd.Process.Pid)
		return err == nil && p.SID > 0
	}, 5*time.Second, 20*time.Millisecond)
	if newSession {
		require.Equal(t, p.PID, p.SID, "the setsid'd helper must lead its own session")
	}
	return p
}

// The reaper's own descendant is excluded like any af daemon's subtree —
// no tmux server has to sit above the reaper for the exclusion to hold
// (#7435 R2): a marker-carrying child of the reaper is the EnsureDaemon'd
// shape Codex's e5b repro signalled. Only a tmux boundary BETWEEN them
// keeps a pane process eligible; that direction is the live chain fixture
// TestReapSessionProcessesReapsAPaneWhoseServerIsADescendantOfTheReaper.
func TestClassifyCaptureAncestryProtectsTheReapersOwnDescendant(t *testing.T) {
	snap := map[int]proctree.Process{
		60: {PID: 60, PPID: 50, Comm: "sleep"},
		50: {PID: 50, PPID: 1, Comm: "af"},
	}
	require.Equal(t, protectReasonSelfSubtree,
		classifyCaptureAncestry(snap, 60, 50),
		"a captured process that reaches the reaper with no tmux between is the reaper's own descendant")
}

// commNamesAFDaemonBinary mirrors the {af, agent-factory} name set
// proctree.ArgvIsAFBinary checks: the kernel task name is the same
// executable's name, so the same set applies.
func TestCommNamesAFDaemonBinary(t *testing.T) {
	for _, tc := range []struct {
		comm string
		want bool
	}{
		{"af", true},
		{"agent-factory", true},
		{"launchd", false},
		{"sleep", false},
		{"af-helper", false},
		{"agent-factory-dev", false},
		{"", false},
	} {
		t.Run(tc.comm, func(t *testing.T) {
			require.Equal(t, tc.want, commNamesAFDaemonBinary(tc.comm))
		})
	}
}

// When a process's argv cannot be read — routine on darwin for a
// foreign-uid ancestor like launchd (the kernel declines KERN_PROCARGS2) —
// the kernel task name rules the daemon verdict out instead: a comm that
// cannot name an af binary cannot be an af daemon. Without the fallback
// every orphan reparented to launchd reported "unproven" and was protected,
// which is why darwin reaped nothing (#7435 CI). The verdict is the same on
// both platforms — linux reaches it through /proc/1/cmdline instead.
func TestAFDaemonClassRulesOutAnUnreadableAncestorByItsTaskName(t *testing.T) {
	// pid 1 exists on every platform; on darwin its argv is unreadable to a
	// non-root caller, on linux it reads as init/systemd. Either way it is
	// not an af daemon.
	initProc := proctree.Process{PID: 1, Comm: "launchd"}
	require.Equal(t, afDaemonNot, afDaemonClass(initProc),
		"a non-af task name rules the daemon verdict out when argv cannot be read")

	// An af-NAMED comm with unreadable argv is the case that must stay
	// uncertain: a real foreign-uid af daemon.
	if len(proctree.Argv(1)) == 0 {
		// Only assertable where pid 1's argv is genuinely unreadable (darwin).
		require.Equal(t, afDaemonUncertain,
			afDaemonClass(proctree.Process{PID: 1, Comm: "af"}),
			"an af-named comm keeps the unreadable-argv daemon case protected")
	}
}

// The reparented-survivor shape darwin hands every vanished pane: the
// survivor's only ancestor left is launchd (pid 1), whose argv the kernel
// will not give a foreign uid. The chain must end ELIGIBLE there — the
// survivor is not the reaper's chain, not a daemon subtree, not behind a
// tmux boundary — so the reap verdict equals linux's.
func TestClassifyCaptureAncestryEndsEligibleAtAnUnreadableTopOfChain(t *testing.T) {
	survivor := proctree.Process{PID: 4242, PPID: 1, Comm: "sleep"}
	snap := map[int]proctree.Process{
		4242: survivor,
		1:    {PID: 1, PPID: 0, Comm: "launchd"},
	}
	require.Equal(t, "", classifyCaptureAncestry(snap, survivor.PID, os.Getpid()),
		"a survivor reparented to the unreadable top of chain stays reapable")
}

// Darwin-specific pin for the missing input itself: the kernel refuses
// KERN_PROCARGS2 for a foreign-uid process, so a reparented survivor's
// launchd ancestor can never be argv-classified — the kernel task name is
// the membership proof darwin does have (#7435 CI on macOS).
func TestAFDaemonClassOnDarwinUsesTheKernelTaskNameForLaunchd(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the unreadable-argv premise is darwin's")
	}
	require.Empty(t, proctree.Argv(1),
		"launchd must not answer KERN_PROCARGS2 to a foreign uid — the premise this test pins")
	require.Equal(t, afDaemonNot,
		afDaemonClass(proctree.Process{PID: 1, Comm: "launchd"}),
		"launchd's task name rules it out — the survivor reparented to it stays reapable")
	require.Equal(t, afDaemonUncertain,
		afDaemonClass(proctree.Process{PID: 1, Comm: "af"}),
		"an af-named unreadable process stays protected even at the top of the chain")
}
