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
//     reaper's own subtree, so it is excluded like signalling excludes it —
//     never signalled — and NAMED at WARNING all the same: an excluded
//     marked carrier is never a silent keep. Item 1 of #7484 moved the
//     naming out of the blocking error — the carrier is LEFT ALIVE under a
//     worktree a Known close may remove, not a reason the close is Unknown
//     (#7435, Codex 4597265500; channel change is intended).
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

	var warnings bytes.Buffer
	oldWarnings := log.WarningLog.Writer()
	log.WarningLog.SetOutput(&warnings)
	t.Cleanup(func() { log.WarningLog.SetOutput(oldWarnings) })

	marked, _, err := markedOrphanProcesses(
		[]proctree.Process{member, unadmitted, ownChild}, name, home,
		fixtureCohort(gen), membership, nil)
	require.Error(t, err, "an unadmitted marker-carrier is reported, not silently skipped")
	require.Contains(t, err.Error(), "cannot be proven a member")
	require.Contains(t, err.Error(), strconv.Itoa(unadmitted.PID),
		"the report names the carrier it could not place")
	require.NotContains(t, err.Error(), strconv.Itoa(ownChild.PID),
		"a positively-placed carrier is NOT a blocking refusal — it is named, not reported (#7484 item 1)")
	require.Contains(t, warnings.String(), strconv.Itoa(ownChild.PID),
		"the reaper's own descendant is excluded — and an excluded marked carrier is NAMED, never silently left")
	require.Contains(t, warnings.String(), "LEFT ALIVE",
		"the naming says the carrier was left running, not signalled")
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
	reason, _, boundary := classifyCaptureAncestry(snap, 60, 50)
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
	reason, _, boundary := classifyCaptureAncestry(snap, paneP.PID, os.Getpid())
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
	reason, _, boundary := classifyCaptureAncestry(snap, 600001, os.Getpid())
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
	// Positively placed: a descendant of a real af-module build running
	// af --daemon argv. The helper's argv CLAIMS the daemon; the test
	// binary's kernel-attested module build is what VERIFIES it (#7491),
	// so this member is spared by PROOF and filtered out of the report.
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
	reason, _, boundary := classifyCaptureAncestry(snap, survivor.PID, os.Getpid())
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

// #7491 item 1 — the claim and the attestation are different verdicts. A
// daemon placement that authorizes a close must rest on identity the
// carrier cannot write: argv CLAIMS, and a process can write argv
// (`exec -a af … --daemon`), while the kernel-attested executable's Go
// build info either names this module or does not.
//
//   - VERIFIED: the test binary itself — a real af-module build — running
//     af-daemon argv attests at whatever path it was built to (the #7484
//     R18 shape: another home's real daemon must still verify);
//   - UNVERIFIED: a foreign binary (`yes`) under the same forged argv —
//     claimed, never proven: still never signalled, but blocking.
func TestAFDaemonClassDistinguishesTheClaimFromTheKernelAttestation(t *testing.T) {
	if os.Getenv(afDaemonSleepEnv) == "1" {
		time.Sleep(5 * time.Minute)
		return
	}
	verified := exec.Command(os.Args[0])
	verified.Args = []string{"af", "--daemon",
		"-test.run=^TestAFDaemonClassDistinguishesTheClaimFromTheKernelAttestation$"}
	verified.Env = append(os.Environ(), afDaemonSleepEnv+"=1")
	require.NoError(t, verified.Start())
	t.Cleanup(func() { _ = verified.Process.Kill(); _, _ = verified.Process.Wait() })
	require.Eventually(t, func() bool {
		return proctree.ArgvIsAFDaemon(proctree.Argv(verified.Process.Pid))
	}, 5*time.Second, 20*time.Millisecond, "the daemon argv must land before the verdict is read")
	verifiedProc, err := proctree.Lookup(verified.Process.Pid)
	require.NoError(t, err)
	require.Equal(t, afDaemonIs, afDaemonClass(verifiedProc),
		"a real af build running daemon argv is VERIFIED — at any install path")

	yesPath, err := exec.LookPath("yes")
	require.NoError(t, err)
	forged := exec.Command(yesPath, "--daemon")
	forged.Args[0] = "af"
	forged.Stdout = nil
	require.NoError(t, forged.Start())
	t.Cleanup(func() { _ = forged.Process.Kill(); _, _ = forged.Process.Wait() })
	require.Eventually(t, func() bool {
		return proctree.ArgvIsAFDaemon(proctree.Argv(forged.Process.Pid))
	}, 5*time.Second, 20*time.Millisecond, "the forged argv must land before the verdict is read")
	forgedProc, err := proctree.Lookup(forged.Process.Pid)
	require.NoError(t, err)
	require.Equal(t, afDaemonUnverified, afDaemonClass(forgedProc),
		"a foreign binary under af argv CLAIMS the daemon — it can never prove it")
}

// #7491 item 2 — the live-capture close path names a positively-placed
// exclusion at WARNING, the same severity the blind path uses (#7484): the
// process keeps running under a Known close that may remove the worktree it
// typically sits inside, so the line must name the pid, its placement, and
// that it was LEFT ALIVE.
func TestReapSessionProcessesNamesAPositivePlacementAtWarning(t *testing.T) {
	shrinkReapWaits(t)
	sleeper := exec.Command("sleep", "300")
	require.NoError(t, sleeper.Start())
	t.Cleanup(func() { _ = sleeper.Process.Kill(); _, _ = sleeper.Process.Wait() })
	proc, err := proctree.Lookup(sleeper.Process.Pid)
	require.NoError(t, err)

	var warnings bytes.Buffer
	oldWarnings := log.WarningLog.Writer()
	log.WarningLog.SetOutput(&warnings)
	t.Cleanup(func() { log.WarningLog.SetOutput(oldWarnings) })

	protected := protectedSet{proc.PID: {
		reason:   protectReasonDaemonDescendant(4242),
		positive: true,
	}}
	remaining := reapSessionProcesses(reapOnRequest, "af_positive_warning",
		[]proctree.Process{proc}, time.Millisecond, 100*time.Millisecond, protected)
	require.Empty(t, remaining,
		"a positively-placed exclusion is exempt from signals and not a leak")
	require.Contains(t, warnings.String(), strconv.Itoa(proc.PID),
		"the WARNING names the pid it left alive")
	require.Contains(t, warnings.String(), "LEFT ALIVE",
		"the WARNING says the carrier keeps running under a close that may remove its worktree")
	require.Contains(t, warnings.String(), "descendant of agent-factory daemon",
		"the WARNING names the placement, not just the pid")
	require.True(t, proctree.AliveSame(proc),
		"excluded means never signalled")
}

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
	// comparison the tier needs. The carrier IS the fixture's program (the
	// test binary's own comm), so it passes the R9 executable-identity
	// allowlist too.
	membership := cohortMembership{
		PaneSIDs: map[int]bool{400001: true},
		Recorded: map[int]uint64{400001: 1},
		Root:     proctree.Process{PID: 400001, StartID: 1},
		AgentExe: filepath.Base(os.Args[0]),
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

	carrierIsRefused(cohortMembership{
		PaneSIDs: map[int]bool{400001: true},
		Recorded: map[int]uint64{400001: 1},
		// An anchored root but NO program identity: the R9 allowlist has
		// nothing to match on, so tier 2 refuses rather than falling back to
		// the open "clean ancestry" rule it replaced.
		Root: proctree.Process{PID: 400001, StartID: 1},
	}, "without the session's program identity the allowlist admits nothing")

	carrierIsRefused(cohortMembership{
		PaneSIDs: map[int]bool{400001: true},
		Recorded: map[int]uint64{400001: 1},
		// A program identity that is NOT the carrier's: the carrier is some
		// other program (the screen/daemon-helper class), not the session's
		// own re-host.
		Root:     proctree.Process{PID: 400001, StartID: 1},
		AgentExe: "sleep",
	}, "a carrier that is not the session's own program is refused")

	marked, _, err := markedOrphanProcesses(
		[]proctree.Process{carrier}, name, home, cohort,
		cohortMembership{
			PaneSIDs: map[int]bool{400001: true},
			Recorded: map[int]uint64{400001: 1},
			Root:     proctree.Process{PID: 400001, StartID: 1},
			AgentExe: filepath.Base(os.Args[0]),
		}, nil)
	require.NoError(t, err, "the same carrier with a predating root must admit — the edge pins differ")
	require.Equal(t, []proctree.Process{carrier}, marked)
}

// #7435 R9/R14 — tier 2 is an ALLOWLIST on the session's own AGENT
// identity, and every admission is by a process's OWN executable identity:
// the program itself, a `claude`-named host re-hosting a `claude`-named
// window (the real bg-pty-host shape), and the program's marked subtree.
// The refused shapes pin the other side: a marked host that is NOT the
// program is refused even when it parents a program-named window — a host
// is never admitted by its child's identity (#7435 R14) — and a
// multiplexer pair or lone non-program orphan is reported and left alive.
func TestMarkedOrphanProcessesTier2AdmitsOnlyTheAgentProgram(t *testing.T) {
	home := t.TempDir()
	cohort := fixtureCohort("af_tier2_allow-gen")
	sweep := func(agentExe string, candidates ...proctree.Process) ([]proctree.Process, error) {
		marked, _, err := markedOrphanProcesses(
			candidates, "af_tier2_allow", home, cohort,
			cohortMembership{
				PaneSIDs: map[int]bool{400001: true},
				Recorded: map[int]uint64{400001: 1},
				Root:     proctree.Process{PID: 400001, StartID: 1},
				AgentExe: agentExe,
			}, nil)
		return marked, err
	}
	exe := filepath.Base(os.Args[0])

	// The program itself re-hosted: the escapee helper IS the fixture's
	// program (this test binary's comm).
	agent := spawnMarkedProcess(t, "af_tier2_allow", home)
	marked, err := sweep(exe, agent)
	require.NoError(t, err)
	require.Equal(t, []proctree.Process{agent}, marked,
		"the session's own re-hosted program is admitted by its own identity")

	// The two-level #3699 shape on OWN identity (#7435 R14): a
	// `claude`-named host shim re-hosting a `claude`-named window — each
	// admits by what IT is, the way the real re-exec'd bg-pty-host does.
	agentSpec := agentSleeperSpec(t)
	host, rehosted := spawnMarkedProcessWithChild(t, "af_tier2_allow", home, "af_tier2_allow-gen",
		agentSpec[0], agentSpec[1:]...)
	marked, err = sweep("claude", host, rehosted)
	require.NoError(t, err)
	require.ElementsMatch(t, []proctree.Process{host, rehosted}, marked,
		"the claude-named host and its window each admit by their own identity")

	// The R14 property, negative leg: a marked `sh` host parenting a window
	// that IS the program. The window admits by its own identity; the host
	// does not inherit the admission — it is refused by ITS OWN identity,
	// reported, and left alive. A refused process's host is never
	// signalled through it.
	screenHost, programWin := spawnMarkedHostPair(t, "af_tier2_allow", home, "af_tier2_allow-gen",
		os.Args[0], markedEscapeeArg, filepath.Join(t.TempDir(), "rehost.pid"))
	marked, err = sweep(exe, screenHost, programWin)
	require.Error(t, err, "a host that is not the program is REFUSED even beside a program window")
	require.Contains(t, err.Error(), "pid "+strconv.Itoa(screenHost.PID)+" ",
		"the refusal names the host the one-hop rule used to admit")
	require.Equal(t, []proctree.Process{programWin}, marked,
		"the program-named window admits by its own identity; its host does not")
	require.True(t, proctree.AliveSame(screenHost),
		"a refused process's host is never signalled through it")

	// The plain screen shape: an `sh` host and `tail` window, neither the
	// program — the pair is refused wholesale like a user multiplexer.
	screenHost2, screenWin := spawnMarkedHostPair(t, "af_tier2_allow", home, "af_tier2_allow-gen",
		"tail", "-f", "/dev/null")
	marked, err = sweep(exe, screenHost2, screenWin)
	require.Error(t, err, "the multiplexer pair must be REFUSED, not silently kept")
	require.Contains(t, err.Error(), "pid "+strconv.Itoa(screenHost2.PID)+" ",
		"the refusal names the screen host's pid")
	require.Contains(t, err.Error(), "pid "+strconv.Itoa(screenWin.PID)+" ",
		"the refusal names the screen window's pid")
	require.Empty(t, marked)
	require.True(t, proctree.AliveSame(screenHost2), "refused means never signalled")
	require.True(t, proctree.AliveSame(screenWin), "refused means never signalled")

	// A lone marked orphan that is not the program — the escaped
	// daemon-helper shape: refused, reported, alive.
	helper := spawnMarkedProgram(t, "af_tier2_allow", home, "af_tier2_allow-gen",
		"tail", "-f", "/dev/null")
	marked, err = sweep(exe, helper)
	require.Error(t, err)
	require.Contains(t, err.Error(), "pid "+strconv.Itoa(helper.PID)+" ")
	require.Empty(t, marked)
	require.True(t, proctree.AliveSame(helper))
}

// The subtree leg in isolation (#7435 R9): a marked helper descending from
// the program through marked ancestors is the program's own workload —
// admitted with it. An equally-shaped helper under a NON-program host is
// refused — the walk closes at the first ancestor that is not the program.
func TestMarkedOrphanProcessesTier2AdmitsOnlyTheProgramSubtree(t *testing.T) {
	home := t.TempDir()
	membership := cohortMembership{
		PaneSIDs: map[int]bool{400001: true},
		Recorded: map[int]uint64{400001: 1},
		Root:     proctree.Process{PID: 400001, StartID: 1},
		AgentExe: "claude",
	}
	sweep := func(candidates ...proctree.Process) ([]proctree.Process, error) {
		marked, _, err := markedOrphanProcesses(
			candidates, "af_tier2_sub", home, fixtureCohort("af_tier2_sub-gen"), membership, nil)
		return marked, err
	}

	// `claude`-named escapee (= the program's own identity) -> tail: the
	// marked child descends from the program inside the eligible cohort —
	// admitted as its subtree even though its own comm is not the
	// program's.
	program, helper := spawnMarkedProcessWithChild(t, "af_tier2_sub", home, "af_tier2_sub-gen",
		"tail", "-f", "/dev/null")
	marked, err := sweep(program, helper)
	require.NoError(t, err)
	require.ElementsMatch(t, []proctree.Process{program, helper}, marked,
		"the program's marked descendants are its subtree — admitted with it")
}

// copy), not a member that escaped: it holds no kernel proof, so proof
// cannot place it and the boundary refuses it — REPORTED, never silently
// left, and never signalled (#7435 R6, Codex 4597265500). The fixture
// drives the real boundary: an isolated second server hosting a pane
// stamped with the vanished session's exact markers.
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
	require.Error(t, inspectErr,
		"an excluded carrier is REPORTED, never silently left (#7435, Codex 4597265500)")
	require.Contains(t, inspectErr.Error(), "pid "+strconv.Itoa(paneP.PID)+" ",
		"the refusal names the foreign carrier's pid")
	require.Empty(t, marked,
		"a carrier behind a tmux server is refused — the sweep owes it no signal")
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

// RetirePaneRootEvidence is the R11 half: a seed outlives the claim it came
// from only while the claim stands. Retiring the pair clears the anchor;
// retiring a DIFFERENT pair — a claim this seed never made — leaves it.
func TestRetirePaneRootEvidence(t *testing.T) {
	ts := NewTmuxSessionFromSanitizedName("af_retire_test", "sleep 300")

	ts.SeedPaneRootEvidence(4242, 99)
	_, _, ok := ts.PaneRootEvidence()
	require.True(t, ok)

	ts.RetirePaneRootEvidence(7777, 55)
	pid, _, ok := ts.PaneRootEvidence()
	require.True(t, ok, "a different pair is a different claim — the anchor stays")
	require.Equal(t, 4242, pid)

	ts.RetirePaneRootEvidence(4242, 99)
	_, _, ok = ts.PaneRootEvidence()
	require.False(t, ok,
		"the retired claim's anchor retires with it — a disproven root cannot floor tier 2")
}

// The R13 anchor rule: with several pane roots the floor is the EARLIEST
// recorded root, chosen deterministically — map order must never pick the
// anchor.
func TestEarliestRecordedRootIsDeterministic(t *testing.T) {
	sids := map[int]bool{10: true, 20: true, 30: true, 40: true}
	recorded := map[int]uint64{10: 50, 30: 40, 40: 40}
	proven := map[int]uint64{20: 70}

	// Earliest by StartID: 30 and 40 both carry 40 — the lower pid breaks
	// the tie, and it wins no matter which order the map range hands them.
	want := proctree.Process{PID: 30, StartID: 40}
	for range 50 {
		require.Equal(t, want, earliestRecordedRoot(sids, recorded, proven))
	}

	// A sid with no pair anywhere contributes nothing; a zero StartID is no
	// anchor even though it "orders earliest".
	require.Equal(t, proctree.Process{},
		earliestRecordedRoot(map[int]bool{90: true}, recorded, proven))
	require.Equal(t, proctree.Process{PID: 11, StartID: 5},
		earliestRecordedRoot(map[int]bool{11: true, 12: true},
			map[int]uint64{11: 5}, map[int]uint64{12: 0}),
		"a zero-StartID leader is skipped, never treated as the earliest root")
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

// #7435 R15 — the allowlist's identity is an AGENT executable only:
// agentProgramExe answers "" for every general-purpose runtime, so a
// shell or process tab's vanished-session sweep gets kernel evidence only
// and a carrier can never be admitted "because it ran the same
// interpreter". The gate is AgentForCommand — the same predicate the
// launch's credential boundary trusts — so a look-alike name
// (claude-helper, myclaude) or a compound command resolves "" too.
func TestAgentProgramExeNamesOnlyAnAgentIdentity(t *testing.T) {
	for _, tc := range []struct {
		program string
		want    string
	}{
		{"claude", "claude"},
		{"claude --resume", "claude"},
		{"aider --model x", "aider"},
		{"codex", "codex"},
		{"env TERM=x claude", "claude"},
		{"bash", ""},
		{"bash pane.sh", ""},
		{"sh", ""},
		{"zsh -i", ""},
		{"fish", ""},
		{"python server.py", ""},
		{"python3 -m http.server", ""},
		{"node srv.js", ""},
		{"npm run dev", ""},
		{"sleep 300", ""},
		{"claude-helper", ""},
		{"myclaude", ""},
		{"cd /x && claude", ""},
	} {
		s := NewTmuxSessionFromSanitizedName("af_prog_gate", tc.program)
		require.Equal(t, tc.want, s.agentProgramExe(), "program %q", tc.program)
	}
}

// #7435 R16 — the recorded program lives in the pane root's argv, never
// its Comm: the root is af's `sh` wrapper on every launch, so Comm is
// exactly the wrong place to learn the identity. recordedPaneProgramExe
// recovers it from the `sh -c <command>` payload (post-exec), the
// exec-shim line (pre-exec, where the command is the last word), or the
// exec'd-through root's own argv[0] — and returns "" for anything that is
// not a supported agent identity (#7435 R15).
func TestRecordedPaneProgramExeReadsTheProgramNotTheWrapper(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{"post-exec sh -c agent", []string{"/bin/sh", "-c", "claude --resume"}, "claude"},
		{"post-exec sh -c path agent", []string{"/bin/sh", "-c", "/opt/x/claude --foo"}, "claude"},
		{"post-exec sh -c env-wrapped agent", []string{"/bin/sh", "-c", "env TERM=x claude"}, "claude"},
		{"pre-exec exec-shim line", []string{"/bin/sh", "-c",
			"/usr/local/bin/af __af-session-env-exec claude 2 PATH HOME 'claude --resume'"}, "claude"},
		{"pre-exec account-shim line", []string{"/bin/sh", "-c",
			"/usr/local/bin/af __af-session-env-exec-account /run/af.sock tok 'claude'"}, "claude"},
		{"exec'd-through root", []string{"/d/pbin/claude", "300"}, "claude"},
		{"exec'd-through path with spaces", []string{"/opt/My Apps/claude", "--resume"}, "claude"},
		{"exec'd-through path with spaces, non-agent", []string{"/opt/My Apps/sleep", "300"}, ""},
		{"codex -c config flag", []string{"codex", "-c", "check_for_update_on_startup=false"}, "codex"},
		{"python -c agent-looking script", []string{"python", "-c", "claude --resume"}, ""},
		{"path-qualified zsh -c", []string{"/usr/bin/zsh", "-c", "claude"}, "claude"},
		{"bare agent root", []string{"claude"}, "claude"},
		{"argv0 look-alike", []string{"claude-helper", "--serve"}, ""},
		{"shell tab sh -c", []string{"/bin/sh", "-c", "bash pane.sh"}, ""},
		{"process tab", []string{"/bin/sh", "-c", "sleep 300"}, ""},
		{"interpreter", []string{"/bin/sh", "-c", "python x.py"}, ""},
		{"node", []string{"/bin/sh", "-c", "node srv.js"}, ""},
		{"compound command", []string{"/bin/sh", "-c", "cd /x && claude"}, ""},
		{"sleep root", []string{"sleep", "300"}, ""},
		{"empty argv", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, recordedPaneProgramExe(tc.argv))
		})
	}
}

// #7435 R15 — an EMPTY allowlist identity matches nothing. Without the
// guard a carrier whose own Comm the kernel would not name would satisfy
// "" == "" and admit itself under a shell or process tab's shut tier 2 —
// the wrong side of the same gate.
func TestExeMatchesProgramMatchesNothingOnAnEmptyIdentity(t *testing.T) {
	nameless := proctree.Process{PID: 4242, StartID: 1, Comm: ""}
	require.False(t, exeMatchesProgram(nameless, ""),
		"a comm-less carrier must not admit itself against an empty allowlist")
	named := proctree.Process{PID: 4242, StartID: 1, Comm: "claude"}
	require.False(t, exeMatchesProgram(named, ""),
		"no identity admits against an empty allowlist")
	require.True(t, exeMatchesProgram(named, "claude"))
}

// #7435 R16, end to end at the cohort: the vanished-session sweep takes
// the allowlist's identity from the pane's RECORDED program — the map the
// pre-marker capture filled from the root's argv — never from the anchor
// root's Comm. A cohort whose anchor recorded `claude` admits and reaps a
// `claude`-named carrier; the same carrier under an anchor with NO
// recorded program (and Comm left holding "claude" as bait — the deleted
// Root.Comm path would have taken it) is refused, reported, and left
// alive.
func TestReapVanishedSessionProcessCohortTakesAgentExeFromTheRecordedProgram(t *testing.T) {
	shrinkReapWaits(t)
	home := t.TempDir()
	name := "af_tier2_recorded"
	gen := name + "-gen"
	agentSpec := agentSleeperSpec(t)

	carrier := spawnMarkedProgram(t, name, home, gen, agentSpec[0], agentSpec[1:]...)
	require.NoError(t, reapVanishedSessionProcessCohort(name, home,
		nil, nil, fixtureCohort(gen),
		cohortMembership{
			PaneSIDs:         map[int]bool{400001: true},
			Recorded:         map[int]uint64{400001: 1},
			RecordedPrograms: map[int]string{400001: "claude"},
		}, false),
		"the anchor root's recorded `claude` program admits the claude-named carrier")
	require.Eventually(t, func() bool {
		return !proctree.AliveSame(carrier)
	}, 5*time.Second, 20*time.Millisecond, "the admitted carrier must be reaped")

	// The same carrier under an anchor whose own Comm would have named
	// the program — the exact substitution R16 removes: nothing admits,
	// the carrier is refused and left alive.
	carrier2 := spawnMarkedProgram(t, name, home, gen, agentSpec[0], agentSpec[1:]...)
	err := reapVanishedSessionProcessCohort(name, home,
		nil, nil, fixtureCohort(gen),
		cohortMembership{
			PaneSIDs: map[int]bool{400002: true},
			Recorded: map[int]uint64{400002: 1},
			Root:     proctree.Process{PID: 400002, StartID: 1, Comm: "claude"},
			// A recorded program on a DIFFERENT pane root must not leak
			// into this anchor's identity either.
			RecordedPrograms: map[int]string{999999: "claude"},
		}, false)
	require.Error(t, err,
		"no recorded program on the ANCHOR root means no allowlist identity — even when Comm says claude")
	require.Contains(t, err.Error(), "pid "+strconv.Itoa(carrier2.PID)+" ",
		"the refusal names the refused carrier's pid")
	require.True(t, proctree.AliveSame(carrier2),
		"refused means never signalled — Root.Comm must not substitute for the recorded program")
}

// The reexec'd fake-daemon branch of the decision-table test: argv claims
// `af --daemon` and the test binary's kernel-attested module build VERIFIES
// it (#7491), so afDaemonClass answers afDaemonIs, and it spawns a marked
// sleep as a direct child so the child's chain is child → daemon.
const (
	decisionDaemonEnv     = "AF_TEST_DECISION_DAEMON"
	decisionDaemonSpecEnv = "AF_TEST_DECISION_DAEMON_SPEC"
)

// THE DECISION TABLE (#7435, Codex 4597265500 + 4598089582): admit = proof
// AND NOT foreign evidence about THIS process. Both P1s were ORDERING
// regressions — boundary consulted before proof left this session's own
// recorded root behind the shared server unreported and alive; proof
// consulted before the generation gate admitted a changed-generation
// descendant of a proven parent. Each row below drives a real carrier
// through markedOrphanProcesses and asserts the verdict: `owned` carriers
// land in the marked set; every refusal is reported by pid and left alive,
// never signalled.
//
//	proof side:   none | kernel SID | recorded pair | descent from proven
//	foreign side: none | generation differs | generation missing |
//	              tmux-parent boundary | reaper subtree | af-daemon subtree
//
// The two rows that cannot fit a unit table — a recorded pane root parented
// by a REAL shared server, and a sibling session's pane under it — are the
// close_dead_pane_test.go pair TestCloseAndWaitForPaneExit_VanishedSighup*
// on an isolated server.
func TestMarkedOrphanProcessesDecisionTable(t *testing.T) {
	if os.Getenv(decisionDaemonEnv) == "1" {
		// Inside the forged-argv daemon: publish the marked child's pid,
		// then park. exec.Command with a clean Env hands the child only the
		// fixture's markers.
		spec := strings.Split(os.Getenv(decisionDaemonSpecEnv), "\t")
		require.Len(t, spec, 4)
		cmd := exec.Command("sleep", "300")
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			EnvMarkerSession + "=" + spec[0],
			EnvMarkerHome + "=" + spec[1],
			EnvMarkerGeneration + "=" + spec[2],
		}
		require.NoError(t, cmd.Start())
		require.NoError(t, os.WriteFile(spec[3],
			[]byte(strconv.Itoa(cmd.Process.Pid)), 0o600))
		_, _ = cmd.Process.Wait()
		time.Sleep(5 * time.Minute)
		return
	}
	shrinkReapWaits(t)
	const name = "af_decision_table"
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	gen := name + "-gen"
	otherGen := "gen-other"

	spawnDaemonChild := func(t *testing.T) proctree.Process {
		dir := t.TempDir()
		childPidFile := filepath.Join(dir, "child.pid")
		daemon := exec.Command(os.Args[0])
		daemon.Args = []string{"af", "--daemon",
			"-test.run=^TestMarkedOrphanProcessesDecisionTable$"}
		daemon.Env = append(os.Environ(),
			decisionDaemonEnv+"=1",
			decisionDaemonSpecEnv+"="+name+"\t"+home+"\t"+gen+"\t"+childPidFile)
		daemon.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		require.NoError(t, daemon.Start())
		t.Cleanup(func() {
			_ = daemon.Process.Kill()
			_, _ = daemon.Process.Wait()
		})
		require.Eventually(t, func() bool {
			return proctree.ArgvIsAFDaemon(proctree.Argv(daemon.Process.Pid))
		}, 5*time.Second, 20*time.Millisecond,
			"the daemon argv claim must land — the test binary's kernel exe VERIFIES it")
		waitForPIDFile(t, childPidFile)
		return processFromPIDFile(t, childPidFile)
	}

	// spawnForgedDaemonChild spawns a marked sleep whose parent CLAIMS the
	// daemon through argv — `exec -a af <non-af binary> --daemon` — on an
	// executable the kernel attests is NOT an af build (#7491 item 1). bash
	// forks the sleep and then execs the forged process in place, so the
	// child's parent chain is child → forged-daemon. The claim still spares
	// the child from signals; it can never authorize a close.
	spawnForgedDaemonChild := func(t *testing.T) proctree.Process {
		dir := t.TempDir()
		childPidFile := filepath.Join(dir, "child.pid")
		yesPath, err := exec.LookPath("yes")
		require.NoError(t, err)
		forged := exec.Command("bash", "-c",
			`sleep 300 & echo $! > "`+childPidFile+`" && exec -a af "`+yesPath+`" --daemon`)
		forged.Env = append(os.Environ(),
			EnvMarkerSession+"="+name,
			EnvMarkerHome+"="+home,
			EnvMarkerGeneration+"="+gen)
		require.NoError(t, forged.Start())
		t.Cleanup(func() {
			_ = forged.Process.Kill()
			_, _ = forged.Process.Wait()
		})
		require.Eventually(t, func() bool {
			return proctree.ArgvIsAFDaemon(proctree.Argv(forged.Process.Pid))
		}, 5*time.Second, 20*time.Millisecond,
			"the forged argv must CLAIM daemon identity before the child is read")
		waitForPIDFile(t, childPidFile)
		return processFromPIDFile(t, childPidFile)
	}

	rows := []struct {
		name string
		// build spawns the row's real carriers and returns them plus the
		// cohort membership the sweep is asked to judge them against. Rows
		// whose verdict needs a sibling carrier in the cohort (a refused
		// ancestor that would vouch for a descendant) return all of them.
		build func(t *testing.T) (candidates []proctree.Process, membership cohortMembership)
		gens  []string
		// wantPlaced rows are refused like wantErr rows — never marked,
		// never signalled — but the placement is positive PROOF the carrier
		// is foreign, so it is named in the log instead of the refusal set
		// that makes the close Unknown (#7484 item 1).
		wantPlaced string
		wantOwned  bool
		wantErr    string
	}{
		{
			// Baseline member: descent from a proven parent with clean
			// markers — proof AND NOT foreign evidence = admit.
			name: "descent proof, clean markers — owned",
			build: func(t *testing.T) ([]proctree.Process, cohortMembership) {
				parent, child := spawnMarkedParentWithMarkedChild(t, name, home, &gen)
				return []proctree.Process{child}, cohortMembership{
					Proven: map[int]uint64{parent.PID: parent.StartID},
				}
			},
			wantOwned: true,
		},
		{
			// Codex 4598089582, first shape: the SAME descent proof with a
			// CHANGED generation — foreign evidence about the process beats
			// its proof.
			name: "descent proof, generation differs — refused",
			build: func(t *testing.T) ([]proctree.Process, cohortMembership) {
				parent, child := spawnMarkedParentWithMarkedChild(t, name, home, &otherGen)
				return []proctree.Process{child}, cohortMembership{
					Proven: map[int]uint64{parent.PID: parent.StartID},
				}
			},
			wantErr: "outside the vanished session",
		},
		{
			// Codex 4598089582, second shape: the SAME descent proof with a
			// MISSING generation — the cohort gate requires one, so the
			// absence is foreign evidence.
			name: "descent proof, generation missing — refused",
			build: func(t *testing.T) ([]proctree.Process, cohortMembership) {
				parent, child := spawnMarkedParentWithMarkedChild(t, name, home, nil)
				return []proctree.Process{child}, cohortMembership{
					Proven: map[int]uint64{parent.PID: parent.StartID},
				}
			},
			wantErr: "has no",
		},
		{
			// Self-proof cannot waive a differing generation either: the
			// recorded (pid, StartID) pair is proof, the changed marker is
			// foreign — refuse.
			name: "recorded pair, generation differs — refused",
			build: func(t *testing.T) ([]proctree.Process, cohortMembership) {
				_, child := spawnMarkedParentWithMarkedChild(t, name, home, &otherGen)
				return []proctree.Process{child}, cohortMembership{
					Recorded: map[int]uint64{child.PID: child.StartID},
				}
			},
			wantErr: "outside the vanished session",
		},
		{
			// Codex 4597265500: kernel-SID proof with a tmux parent — the
			// parent being a server is NOT foreign evidence, so the member
			// is owned. The real-server row is the close_dead_pane SIGHUP
			// test; this is the same verdict on a tmux-comm'd fixture.
			name: "kernel SID, tmux parent — owned",
			build: func(t *testing.T) ([]proctree.Process, cohortMembership) {
				_, child := spawnTmuxNamedParentWithMarkedChild(t, name, home, gen)
				return []proctree.Process{child}, cohortMembership{
					PaneSIDs: map[int]bool{child.PID: true},
					Recorded: map[int]uint64{child.PID: child.StartID},
				}
			},
			wantOwned: true,
		},
		{
			// The boundary's other half: an UNPROVEN carrier behind the
			// same tmux parent — the sibling-pane shape — is refused and
			// reported, never signalled.
			name: "no proof, tmux parent — refused",
			build: func(t *testing.T) ([]proctree.Process, cohortMembership) {
				_, child := spawnTmuxNamedParentWithMarkedChild(t, name, home, gen)
				return []proctree.Process{child}, cohortMembership{Recorded: map[int]uint64{child.PID: child.StartID + 1}}
			},
			wantErr: "sits behind a tmux server",
		},
		{
			// Nothing below a DIFFERENT server inherits: the walk must stop
			// at the tmux ancestor even though a proven member sits above
			// it in the same chain.
			name: "proven member above a different server — refused",
			build: func(t *testing.T) ([]proctree.Process, cohortMembership) {
				leader, _, child := spawnProvenLeaderBehindTmuxParent(t, name, home, gen)
				return []proctree.Process{child}, cohortMembership{
					Proven: map[int]uint64{leader.PID: leader.StartID},
				}
			},
			wantErr: "sits behind a tmux server",
		},
		{
			// The reaper's own subtree is foreign evidence (#7435 R2):
			// refused and never signalled — but a POSITIVELY placed
			// carrier is named in the log, not the refusal set: it does
			// not block the close (#7484 item 1).
			name: "no proof, reaper subtree — refused, named not blocking",
			build: func(t *testing.T) ([]proctree.Process, cohortMembership) {
				child := spawnCohortSleeper(t, name, home, gen, true)
				return []proctree.Process{child}, cohortMembership{Recorded: map[int]uint64{child.PID: child.StartID + 1}}
			},
			wantPlaced: "ancestry places it under",
		},
		{
			// A positively-identified af-daemon subtree is foreign
			// evidence: refused and never signalled — named in the log,
			// never blocking the close on a proven-foreign carrier
			// (#7484 item 1).
			name: "no proof, af-daemon subtree — refused, named not blocking",
			build: func(t *testing.T) ([]proctree.Process, cohortMembership) {
				child := spawnDaemonChild(t)
				return []proctree.Process{child}, cohortMembership{Recorded: map[int]uint64{child.PID: child.StartID + 1}}
			},
			wantPlaced: "ancestry places it under",
		},
		{
			// #7491 item 1: argv CLAIMS daemon ancestry the kernel
			// executable does not attest — `exec -a af` on a NON-af
			// binary. The claim still spares the child from signals
			// (the safe direction), but it can never make the close
			// Known around it: refused, reported, BLOCKING.
			name: "no proof, forged af-daemon argv — refused, blocking",
			build: func(t *testing.T) ([]proctree.Process, cohortMembership) {
				child := spawnForgedDaemonChild(t)
				return []proctree.Process{child}, cohortMembership{Recorded: map[int]uint64{child.PID: child.StartID + 1}}
			},
			wantErr: "cannot be proven",
		},
		{
			// Every foreign-evidence column needs a row with proof
			// PRESENT (#7484 item 2): hoisting `case proven` above the
			// placement cases must turn a row red, and with only
			// mismatched-Recorded rows there was nothing to go red on.
			// A captured Proven pair is proof; the daemon-subtree
			// placement is foreign — foreign still wins.
			name: "capture-proven child, af-daemon subtree — refused, named not blocking",
			build: func(t *testing.T) ([]proctree.Process, cohortMembership) {
				child := spawnDaemonChild(t)
				return []proctree.Process{child}, cohortMembership{Proven: map[int]uint64{child.PID: child.StartID}}
			},
			wantPlaced: "ancestry places it under",
		},
		{
			// Same column, reaper subtree: a captured SETSID'D child of
			// the reaper — the Proven pair is its only possible proof
			// (it leads its own kernel session, so no pane SID can
			// vouch) — and the placement still wins.
			name: "capture-proven setsid'd child, reaper subtree — refused, named not blocking",
			build: func(t *testing.T) ([]proctree.Process, cohortMembership) {
				child := spawnCohortSleeper(t, name, home, gen, true)
				return []proctree.Process{child}, cohortMembership{Proven: map[int]uint64{child.PID: child.StartID}}
			},
			wantPlaced: "ancestry places it under",
		},
		{
			// No proof and no foreign evidence: the carrier stays eligible
			// for tier 2 and — with no agent identity on this row — is
			// reported as unprovable, never silently dropped.
			name: "no proof, no foreign evidence — reported",
			build: func(t *testing.T) ([]proctree.Process, cohortMembership) {
				child := spawnMarkedProcess(t, name, home)
				return []proctree.Process{child}, cohortMembership{Recorded: map[int]uint64{child.PID: child.StartID + 1}}
			},
			gens:    []string{gen},
			wantErr: "cannot be proven",
		},
		{
			// Codex 4598849310: an agent-identity carrier that PREDATES the
			// recorded root is refused by the StartID guard — yet the old
			// loop keyed it into exeMatch on identity alone, so its newer
			// marked child was admitted by descent from a refused ancestor.
			// Anchors are only carriers that pass EVERY tier-2 guard;
			// descent is admitted only from an admitted anchor — both
			// refuse, both are reported, neither is signalled. The escapee
			// pauses between publishing itself and spawning the child so the
			// two StartIDs sit on different kernel ticks and the root's
			// bound lands strictly between them.
			name: "pre-root agent does not vouch for its newer child — refused",
			build: func(t *testing.T) ([]proctree.Process, cohortMembership) {
				host, child := spawnMarkedProcessWithChild(t, name, home, gen, "sleep", "300")
				require.Less(t, host.StartID, child.StartID,
					"the fixture must order the agent strictly before the child — StartIDs are kernel ticks")
				return []proctree.Process{host, child}, cohortMembership{
					AgentExe: "claude",
					Root:     proctree.Process{PID: 1, StartID: child.StartID},
				}
			},
			gens:    []string{gen},
			wantErr: "cannot be proven",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			candidates, membership := row.build(t)
			gens := row.gens
			if gens == nil {
				gens = []string{gen}
			}
			var placedLog bytes.Buffer
			oldWarnings := log.WarningLog.Writer()
			log.WarningLog.SetOutput(&placedLog)
			defer log.WarningLog.SetOutput(oldWarnings)
			marked, _, err := markedOrphanProcesses(
				candidates, name, home,
				fixtureCohort(gens...), membership, map[int]bool{})
			for _, candidate := range candidates {
				pid := strconv.Itoa(candidate.PID)
				if row.wantPlaced != "" {
					// Refused, but the refusal is positively PLACED —
					// named in the log, never in the refusal set that
					// blocks the close (#7484 item 1).
					for _, m := range marked {
						require.NotEqual(t, candidate.PID, m.PID,
							"a refused carrier must never reach the kill set")
					}
					if err != nil {
						require.NotContains(t, err.Error(), "pid "+pid,
							"a positively-placed carrier must not appear in the refusals that block the close")
					}
					require.Contains(t, placedLog.String(), "pid "+pid,
						"a positively-placed carrier is NAMED in the log — never silent")
					require.Contains(t, placedLog.String(), row.wantPlaced)
					require.Contains(t, placedLog.String(), "LEFT ALIVE",
						"the warning must say the carrier outlives the teardown it did not block")
					require.True(t, proctree.AliveSame(candidate),
						"refused means never signalled")
					continue
				}
				if row.wantOwned {
					found := false
					for _, m := range marked {
						if m.PID == candidate.PID {
							found = true
						}
					}
					require.True(t, found, "pid %s must be admitted to the marked set", pid)
					if err != nil {
						require.NotContains(t, err.Error(), "pid "+pid,
							"an admitted carrier must not appear in the refusals")
					}
				} else {
					for _, m := range marked {
						require.NotEqual(t, candidate.PID, m.PID,
							"a refused carrier must never reach the kill set")
					}
					require.Error(t, err, "every exclusion is REPORTED — nothing is silently left")
					require.Contains(t, err.Error(), "pid "+pid+" ",
						"the refusal names the carrier's pid")
					if row.wantErr != "" {
						require.Contains(t, err.Error(), row.wantErr)
					}
					require.True(t, proctree.AliveSame(candidate),
						"refused means never signalled")
				}
			}
		})
	}
}
