package tmux

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/log"
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
//
// R8 binds the SID voucher to the leader it names: a live leader must carry
// the StartID the evidence recorded — recorded first, else the proven
// capture's own identity — or its session cannot vouch. A leader absent from
// the table still vouches: the session outlives it through its members.
func TestCohortMembershipAdmits(t *testing.T) {
	member := proctree.Process{PID: 100, SID: 40, StartID: 9}
	leader := proctree.Process{PID: 40, SID: 40, StartID: 5}
	recycledLeader := proctree.Process{PID: 40, SID: 40, StartID: 77}
	snapWith := func(p proctree.Process) map[int]proctree.Process {
		return map[int]proctree.Process{p.PID: p, 100: member}
	}

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

	// The SID voucher is bound to the recorded leader's StartID (#7435 R8):
	// the leader still heading its session under the recorded identity
	// vouches; the SAME PID heading it under a different StartID is a
	// recycled leader — its session could belong to a replacement that
	// minted matching markers, so it vouches for nothing.
	require.True(t,
		cohortMembership{PaneSIDs: map[int]bool{40: true}, Recorded: map[int]uint64{40: 5}}.
			admits(snapWith(leader), member),
		"the recorded leader still heading its session vouches for its members")
	require.False(t,
		cohortMembership{PaneSIDs: map[int]bool{40: true}, Recorded: map[int]uint64{40: 5}}.
			admits(snapWith(recycledLeader), member),
		"a recycled leader pid heading a session it never led must not vouch")
	require.True(t,
		cohortMembership{PaneSIDs: map[int]bool{40: true}, Proven: map[int]uint64{40: 5}}.
			admits(snapWith(leader), member),
		"the proven capture's own leader identity binds the voucher the same way")
	require.False(t,
		cohortMembership{PaneSIDs: map[int]bool{40: true}}.
			admits(snapWith(leader), member),
		"a live leader the evidence cannot bind to an identity vouches for nothing — "+
			"admitting on SID alone would trust a name, not a process")
	require.True(t,
		cohortMembership{PaneSIDs: map[int]bool{40: true}, Recorded: map[int]uint64{40: 5}}.
			admits(snapWith(member), member),
		"the session persists through its members when the leader is gone")
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
		fixtureCohort(gen), membership, nil)
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
//
// The survivor is membership-ADMITTED on purpose (review R5): passed in as a
// captured candidate it carries its own (pid, StartID) proof, so the home
// gate is the ONLY verdict holding it out. A fixture that merely shares the
// "cannot be proven" substring lets a mutation that skips the unproven check
// stay green on the membership refusal; here that mutation sweeps a live
// process, which the aliveness assert catches instead.
func TestVanishedSessionSweepRefusesAnUnprovableHomeSurvivor(t *testing.T) {
	name := "af_unproven_home"
	ownHome := t.TempDir()
	gen := name + "-gen"
	survivor := spawnMarkedProcess(t, name, ".af")
	membership := cohortMembership{
		Recorded: map[int]uint64{survivor.PID: survivor.StartID},
	}

	err := reapVanishedSessionProcesses(name, ownHome,
		[]proctree.Process{survivor}, nil, membership, false)
	require.Error(t, err, "an unprovable home must refuse the sweep, not silently clear it")
	require.Contains(t, err.Error(), strconv.Itoa(survivor.PID),
		"the refusal names the pid it refused, not another pass's carrier")
	require.Contains(t, err.Error(), "cannot be proven to name this home",
		"the refusal must name the attribution failure, not the membership gate's shared wording")
	require.True(t, proctree.AliveSame(survivor),
		"the survivor stays alive AND the cleanup is not authorized — both halves of fail-closed")

	// The same process read as a candidate directly shows the per-pid split:
	// foreign is skipped by proof, unproven blocks.
	marked, _, inspectErr := markedOrphanProcesses(
		[]proctree.Process{survivor}, name, ownHome, fixtureCohort(gen), membership, nil)
	require.Error(t, inspectErr)
	require.Contains(t, inspectErr.Error(), strconv.Itoa(survivor.PID))
	require.Contains(t, inspectErr.Error(), "cannot be proven to name this home")
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
	reason, boundary := classifyCaptureAncestry(snap, 60, 50)
	require.Equal(t, protectReasonSelfSubtree, reason,
		"a captured process that reaches the reaper with no tmux between is the reaper's own descendant")
	require.False(t, boundary)
}

// The boundary verdict is reported SEPARATELY from protection: a carrier
// still behind a tmux server ends eligible for the capture walk (its own
// mid-kill server is what protects it), while the vanished-session sweep
// reads the same answer as positively foreign (#7435 R6). Both consumers are
// pinned here so a walk that stops reporting the boundary fails this test —
// the probe that drops it goes red on this pin.
func TestClassifyCaptureAncestryReportsTheTmuxBoundaryWithoutProtecting(t *testing.T) {
	// A real isolated tmux server is the only way to place a genuine tmux
	// process in a chain — TmuxIdentityOf requires the server's own process
	// identity, not a comm a fixture could launder.
	testguard.IsolateTmux(t)
	out, err := exec.Command("tmux", "new-session", "-d", "-s", "af_boundary_test", "sleep 300").CombinedOutput()
	require.NoError(t, err, "start the isolated server: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", ExactTarget("af_boundary_test")).Run() })

	var paneP proctree.Process
	require.Eventually(t, func() bool {
		lout, err := exec.Command("tmux", "list-panes", "-t", ExactTarget("af_boundary_test"), "-F", "#{pane_pid}").Output()
		if err != nil {
			return false
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(lout)))
		if err != nil {
			return false
		}
		paneP, err = proctree.Lookup(pid)
		return err == nil
	}, 5*time.Second, 20*time.Millisecond, "the pane never reported its pid")

	snap, err := proctree.Snapshot()
	require.NoError(t, err)
	reason, boundary := classifyCaptureAncestry(snap, paneP.PID, os.Getpid())
	require.Equal(t, "", reason,
		"the pane's walk ends eligible at the server — it is what protects the pane, not what protects FROM it")
	require.True(t, boundary,
		"the walk must report the tmux server it met — the vanished sweep's foreign-pane verdict rides on it")
}

// An ancestor whose daemon verdict cannot be proven is itself the guard:
// protectReasonUnproven keeps a carrier unattributable — reported, never
// silently placed, never admitted by tier 2. A synthetic af-comm ancestor is
// the only shape that reaches afDaemonUncertain on linux: same-uid argv is
// always readable, so the uncertain class needs a comm that names an af
// binary on a pid whose argv cannot resolve — a dead pid reads exactly that.
func TestClassifyCaptureAncestryProtectsUnprovenAncestry(t *testing.T) {
	deadAncestor := proctree.Process{PID: 600000, PPID: 1, Comm: "af"}
	require.Empty(t, proctree.Argv(deadAncestor.PID),
		"the fixture needs an unreadable argv or it classifies afDaemonNot, not uncertain")
	snap := map[int]proctree.Process{
		600001: {PID: 600001, PPID: 600000, Comm: "sleep"},
		600000: deadAncestor,
	}
	reason, boundary := classifyCaptureAncestry(snap, 600001, os.Getpid())
	require.Equal(t, protectReasonUnproven, reason,
		"an af-comm ancestor whose argv cannot be read keeps the carrier unattributable")
	require.False(t, boundary)
}

// The survivor check applies the same exclusion set signalling did: a
// positively-placed process — a daemon subtree, the reaper's own chain — is
// spared by proof and must not read as a leak; an unproven-ancestry survivor
// and an eligible one both stay reported so a cleanup-authorizing caller
// still refuses (#7435 R3/R7).
func TestReportableSurvivorsKeepsOnlyTheUnattributableAndTheEligible(t *testing.T) {
	if os.Getenv(afDaemonSleepEnv) == "1" {
		time.Sleep(5 * time.Minute)
		return
	}
	// Positively placed: a descendant of a real af --daemon argv. The helper's
	// forged argv is what afDaemonClass positively identifies, so this member
	// is spared by PROOF and filtered out of the report.
	daemon := exec.Command(os.Args[0], "-test.run=^TestReportableSurvivorsKeepsOnlyTheUnattributableAndTheEligible$")
	daemon.Args = []string{"af", "--daemon", "-test.run=^TestReportableSurvivorsKeepsOnlyTheUnattributableAndTheEligible$"}
	daemon.Env = append(os.Environ(), afDaemonSleepEnv+"=1")
	daemon.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, daemon.Start())
	t.Cleanup(func() { _ = daemon.Process.Kill() })
	require.Eventually(t, func() bool {
		return proctree.ArgvIsAFDaemon(proctree.Argv(daemon.Process.Pid))
	}, 5*time.Second, 20*time.Millisecond,
		"the helper must positively classify as an af daemon for this fixture to mean anything")

	snap, err := proctree.Snapshot()
	require.NoError(t, err)
	placed := proctree.Process{PID: 600002, PPID: daemon.Process.Pid, Comm: "sleep"}
	unproven := proctree.Process{PID: 600003, PPID: 600004, Comm: "sleep"}
	eligible := proctree.Process{PID: 600005, PPID: 1, Comm: "sleep"}
	// placed's daemon parent is in the live snapshot; unproven's ancestor is
	// an af-comm dead pid; eligible reparented to init.
	snap[placed.PID] = placed
	snap[600004] = proctree.Process{PID: 600004, PPID: 1, Comm: "af"}
	snap[600003] = unproven
	snap[600005] = eligible

	survivors := []proctree.Process{placed, unproven, eligible}
	reportable := reportableSurvivors(snap, survivors)
	require.Equal(t, []proctree.Process{unproven, eligible}, reportable,
		"a positively-placed daemon descendant is not a leak; unproven and eligible survivors stay reported")
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
	reason, boundary := classifyCaptureAncestry(snap, survivor.PID, os.Getpid())
	require.Equal(t, "", reason,
		"a survivor reparented to the unreadable top of chain stays reapable")
	require.False(t, boundary)
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

// The env flag the forged-argv daemon sleeper reexecs on.
const afDaemonSleepEnv = "AF_TEST_AFDAEMON_SLEEP"

// #7435 R6 — tier 2, the generation-identity admission: a marker carrier that
// fails every kernel membership tier because it leads its OWN kernel session
// — the #3699 re-hosted shape — is still ours when it began no earlier than
// the recorded pane root and its ancestry is positively clean. The fixture is
// a real setsid'd, SIGHUP-protected, reparented marked orphan: exactly the
// process that wedged root-reap.
func TestMarkedOrphanProcessesAdmitsARehostedMemberByGenerationIdentity(t *testing.T) {
	name := "af_tier2_admit"
	home := t.TempDir()
	carrier := spawnMarkedProcess(t, name, home)

	var warnings bytes.Buffer
	oldWarnings := log.WarningLog.Writer()
	log.WarningLog.SetOutput(&warnings)
	t.Cleanup(func() { log.WarningLog.SetOutput(oldWarnings) })

	// The recorded pane root predates the carrier by construction — the
	// launch happened first — and the carrier's own StartID is the only
	// comparison the tier needs.
	membership := cohortMembership{
		PaneSIDs: map[int]bool{400001: true},
		Recorded: map[int]uint64{400001: 1},
		Root:     proctree.Process{PID: 400001, StartID: 1},
	}
	marked, _, err := markedOrphanProcesses(
		[]proctree.Process{carrier}, name, home,
		fixtureCohort(name+"-gen"), membership, nil)
	require.NoError(t, err, "the re-hosted carrier must be admitted, not reported")
	require.Equal(t, []proctree.Process{carrier}, marked,
		"tier 2 admits the marker carrier kernel membership cannot see")

	warning := warnings.String()
	require.Contains(t, warning, strconv.Itoa(carrier.PID), "the admission WARNING names the pid")
	require.Contains(t, warning, name, "the admission WARNING names the session")
	require.Contains(t, warning, strconv.FormatUint(carrier.StartID, 10), "the admission WARNING names the StartID")
	require.Contains(t, warning, "->", "the admission WARNING carries the ancestry chain")
}

// Tier 2's edges are what make it safe: every leg must hold or the admission
// is a marker scan with extra steps.
func TestMarkedOrphanProcessesRefusesTheTier2Edges(t *testing.T) {
	name := "af_tier2_edges"
	home := t.TempDir()
	carrier := spawnMarkedProcess(t, name, home)
	cohort := fixtureCohort(name + "-gen")
	carrierIsRefused := func(membership cohortMembership, why string) {
		t.Helper()
		marked, _, err := markedOrphanProcesses(
			[]proctree.Process{carrier}, name, home, cohort, membership, nil)
		require.Error(t, err, why)
		require.Contains(t, err.Error(), "cannot be proven a member", why)
		require.Empty(t, marked, why)
	}

	carrierIsRefused(cohortMembership{
		PaneSIDs: map[int]bool{400001: true},
		Recorded: map[int]uint64{400001: 1},
		// A root that POSTDATES the carrier: nothing that began before the
		// pane could be under its life — fail-closed on the ordering, not on
		// the markers.
		Root: proctree.Process{PID: 400001, StartID: carrier.StartID + 1},
	}, "a carrier older than the recorded root is not this generation's")

	carrierIsRefused(cohortMembership{
		PaneSIDs: map[int]bool{400001: true},
		Recorded: map[int]uint64{400001: 1},
		// No recorded root: there is nothing to anchor the ordering on, so
		// tier 2 cannot run at all.
	}, "without a recorded root there is no generation-identity tier")

	carrierIsRefused(cohortMembership{
		PaneSIDs: map[int]bool{400001: true},
		Recorded: map[int]uint64{400001: 1},
		// A root whose StartID is ZERO is no anchor at all: every carrier
		// orders at-or-after 0, so the bound admits whatever markers say.
		Root: proctree.Process{PID: 400001, StartID: 0},
	}, "a zero-StartID root makes the no-earlier-than bound vacuous")

	marked, _, err := markedOrphanProcesses(
		[]proctree.Process{carrier}, name, home, cohort,
		cohortMembership{
			PaneSIDs: map[int]bool{400001: true},
			Recorded: map[int]uint64{400001: 1},
			Root:     proctree.Process{PID: 400001, StartID: 1},
		}, nil)
	require.NoError(t, err, "the same carrier with a predating root must admit — the edge pins differ")
	require.Equal(t, []proctree.Process{carrier}, marked)
}

// A carrier still behind a tmux server is a foreign pane (#6846's global-env
// copy), not a member that escaped: positively placed OUTSIDE the cohort, so
// the vanished sweep excludes it the way signalling excludes a placed subtree
// — silently, and never reported as a leak (#7435 R6). The fixture drives the
// real boundary: an isolated second server hosting a pane stamped with the
// vanished session's exact markers.
func TestMarkedOrphanProcessesExcludesACarrierBehindAForeignTmuxServer(t *testing.T) {
	testguard.IsolateTmux(t)
	name := "af_tier2_foreign"
	home := t.TempDir()
	gen := "gen-foreign"
	sock := testguard.SocketPath(t, "tier2foreign.sock")
	out, err := exec.Command("tmux", "-S", sock, "new-session", "-d", "-s", "foreign",
		"-e", EnvMarkerSession+"="+name,
		"-e", EnvMarkerHome+"="+home,
		"-e", EnvMarkerGeneration+"="+gen,
		"sleep", "300").CombinedOutput()
	require.NoError(t, err, "start the foreign marked session: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", sock, "kill-server").Run() })

	var paneP proctree.Process
	require.Eventually(t, func() bool {
		lout, err := exec.Command("tmux", "-S", sock, "list-panes", "-t", "foreign", "-F", "#{pane_pid}").Output()
		if err != nil {
			return false
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(lout)))
		if err != nil {
			return false
		}
		paneP, err = proctree.Lookup(pid)
		return err == nil
	}, 5*time.Second, 20*time.Millisecond, "the foreign pane never reported its pid")
	t.Cleanup(func() { _ = proctree.Signal(paneP, syscall.SIGKILL) })

	marked, _, inspectErr := markedOrphanProcesses(
		[]proctree.Process{paneP}, name, home, fixtureCohort(gen),
		cohortMembership{
			PaneSIDs: map[int]bool{400001: true},
			Recorded: map[int]uint64{400001: 1},
			Root:     proctree.Process{PID: 400001, StartID: 1},
		}, nil)
	require.NoError(t, inspectErr,
		"a positively foreign carrier is excluded silently, never reported as an unattributable survivor")
	require.Empty(t, marked,
		"a carrier behind a tmux server is a foreign pane — the sweep owes it nothing")
	require.True(t, proctree.AliveSame(paneP),
		"and it must never be signalled — it is still alive")
}

// A refused carrier is reported ONCE no matter how many sweep passes see it
// (#7435 R8): the shared dedupe set reapVanishedSessionProcessCohort threads
// through every markedOrphanProcesses call keeps one line per pid — without
// it the same survivor repeats in the returned error once per pass and the
// report reads as a bigger leak than exists.
func TestMarkedOrphanProcessesReportsARefusedPidOnceAcrossPasses(t *testing.T) {
	name := "af_dedupe_report"
	home := t.TempDir()
	carrier := spawnMarkedProcess(t, name, home)
	reported := make(map[int]bool)

	var errs []error
	for range 3 {
		_, _, err := markedOrphanProcesses(
			[]proctree.Process{carrier}, name, home,
			fixtureCohort(name+"-gen"), cohortMembership{}, reported)
		errs = append(errs, err)
	}
	require.Error(t, errs[0], "the first pass that sees a refused pid must report it")
	require.NoError(t, errs[1], "the repeat is dropped — the verdict was already reported")
	require.NoError(t, errs[2], "the repeat is dropped — the verdict was already reported")
	joined := errors.Join(errs...)
	require.Equal(t, 1, strings.Count(joined.Error(), "pid "+strconv.Itoa(carrier.PID)+" "),
		"three passes over the same refused pid must report it once")
}

// SeedPaneRootEvidence is the restore path's half of anchor coverage: the
// persisted runtime pair re-primes a session whose pane vanished before any
// live resolution could stamp it.
func TestSeedPaneRootEvidence(t *testing.T) {
	ts := NewTmuxSessionFromSanitizedName("af_seed_test", "sleep 300")
	require.Nil(t, ts.paneRootEvidence.Load())

	ts.SeedPaneRootEvidence(0, 0)
	require.Nil(t, ts.paneRootEvidence.Load(), "a zero pair carries no identity and must not stamp")

	ts.SeedPaneRootEvidence(4242, 0)
	require.Nil(t, ts.paneRootEvidence.Load(),
		"a 0 StartID is no anchor — tier 2's no-earlier-than bound would pass every carrier")

	ts.SeedPaneRootEvidence(4242, 99)
	root := ts.paneRootEvidence.Load()
	require.NotNil(t, root)
	require.Equal(t, 4242, root.PID)
	require.Equal(t, uint64(99), root.StartID)

	ts.SeedPaneRootEvidence(7777, 55)
	require.Equal(t, *root, *ts.paneRootEvidence.Load(),
		"a live resolution that already stamped stays — the persisted pair is older evidence")
}

// The spawn path's half of anchor coverage: Start must leave the pane-root
// identity recorded on the session, or a session whose pane later vanishes
// has nothing for the sweep to anchor on (#7435 R6). Without this stamp a
// probe that drops it still passes every fixture that stamps the evidence by
// hand — this is the only test that sees the stamp exist.
func TestStartStampsPaneRootEvidence(t *testing.T) {
	testguard.IsolateTmux(t)
	ts := NewTmuxSessionFromSanitizedName(
		fmt.Sprintf("af_stamp_%d", time.Now().UnixNano()), "sleep 300")
	require.NoError(t, ts.Start(t.TempDir()))
	t.Cleanup(func() {
		_ = exec.Command("tmux", "kill-session", "-t", ExactTarget(ts.sanitizedName)).Run()
	})

	root := ts.paneRootEvidence.Load()
	require.NotNil(t, root,
		"a spawned session must carry the recorded pane root — the vanished sweep's only anchor")
	live, err := ts.PaneRootProcess()
	require.NoError(t, err)
	require.Equal(t, live.PID, root.PID)
	require.Equal(t, live.StartID, root.StartID)
}
