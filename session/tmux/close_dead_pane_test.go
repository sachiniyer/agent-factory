package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/internal/shellquote"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// #4506 review: a process tab runs under remain-on-exit, so a finished command
// leaves a HELD DEAD PANE. tmux keeps reporting that pane's pane_pid, which is
// the pid that already exited and was reaped. Measured on tmux 3.4 with
// `sh -c 'echo hello; exit 7'`:
//
//	list-panes -F '#{pane_pid} #{pane_dead}'  ->  "<pid> 1", and <pid> is not in the process table
//
// The teardown read that pid as a pane process that "disappeared before its
// descendants could be captured" and refused, so archive, kill and the
// account-scope stop all failed on a finished process tab.

// startHeldPane starts a real session on the test's private server with
// remain-on-exit set in the same invocation (as Start does), and waits until
// tmux reports the pane dead. Production writes the pane's identity bundle
// from the pane's own launch shim (the shim IS the pane root, and it stamps
// before it execs the pane command); the fixture records the same options
// through the production panePidStampArgs helper — and the pane command
// waits on a release file so the stamp and the pane-died hook always land
// while the root is alive, exactly the ordering the shim guarantees. Without
// the release an `exit 7` pane can die inside the ~30ms the second client
// takes to connect, the hook never counts the death, and the fixture flakes.
// exec then replaces the launcher image under the same pid, so the stamped
// #{pane_pid} is still the dead root's.
// afPaneLaunch is the pane command an af launch leaves in pane_start_command —
// the exec marker af stamps into every pane it launches and the recorded
// command as the wrapper's last word — which is also what makes it runnable
// here: tmux executes the string through the default shell, so the pane runs
// `sh -c <command>` with the marker present in its environment.
func afPaneLaunch(command string) string {
	return "env " + sessionenv.ExecMarker + "=1 sh -c " + shellquote.Quote(command)
}

func startHeldPane(t *testing.T, name, command string) string {
	t.Helper()
	return startHeldPaneWithHook(t, name, command, nil)
}

// startHeldPaneWithHookLag is startHeldPane with the pane-died hook's
// increment delayed behind a blocking run-shell: pane_dead becomes
// observable while @af_pane_deaths is still unset — the #5335 window,
// measured on tmux 3.8.
func startHeldPaneWithHookLag(t *testing.T, name, command string) string {
	t.Helper()
	return startHeldPaneWithHook(t, name, command,
		[]string{`run-shell "sleep 0.5" ; ` + sessionenv.PaneDeathsHookCommand})
}

// startHeldPaneWithHook is startHeldPane with an optional pane-died hook
// override: nil installs the production hook verbatim through
// panePidStampArgs; anything else replaces sessionenv.PaneDeathsHookName
// after the stamp lands, still before the pane is released to die. It returns
// the pane's recorded program — the wrapped command afPaneLaunch leaves as
// pane_start_command's last word — for the TmuxSession that probes it.
func startHeldPaneWithHook(t *testing.T, name, command string, hookOverride []string) string {
	t.Helper()
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	script := filepath.Join(dir, "run.sh")
	require.NoError(t, os.WriteFile(script, []byte(
		"while [ ! -f "+release+" ]; do sleep 0.01; done\nexec "+command+"\n"), 0o700))
	// The recorded program must exec into the script: `sh -c 'sh run.sh'`
	// forks the script as a child, and the -c shell then WAITING on it keeps
	// the pty slave open — a root that closes its descriptors and keeps
	// running (DeadPaneWithARunningRootStopsTheRoot) would leave the pane
	// undead forever.
	program := "exec sh " + shellquote.Quote(script)
	require.NoError(t, exec.Command("tmux", "new-session", "-d", "-s", name, afPaneLaunch(program),
		";", "set-option", "-w", "-t", ExactTarget(name), "remain-on-exit", "on").Run())
	require.NoError(t, exec.Command("tmux", panePidStampArgs(ExactTarget(name))...).Run())
	if hookOverride != nil {
		require.NoError(t, exec.Command("tmux", append([]string{
			"set-hook", "-p", "-t", ExactTarget(name), sessionenv.PaneDeathsHookName},
			hookOverride...)...).Run())
	}
	require.NoError(t, os.WriteFile(release, nil, 0o600))
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", ExactTarget(name)).Run() })
	require.Eventually(t, func() bool {
		out, err := exec.Command("tmux", "display-message", "-p", "-t", ExactTarget(name), "#{pane_dead}").Output()
		return err == nil && strings.TrimSpace(string(out)) == "1"
	}, 5*time.Second, 20*time.Millisecond, "the pane never died")
	return program
}

// probeHeldExit probes a held dead pane once and requires its exit status. One
// probe is the contract: ProbePaneExit waits for tmux to collect the root and
// nudges a tmux that lost the root's SIGCHLD (#4682), so a status missing here
// is a product failure, not a slow runner. The failure says which of the two
// states tmux was left in.
func probeHeldExit(t *testing.T, s *TmuxSession, name string) (status int, at time.Time) {
	t.Helper()
	dead, status, statusKnown, at, known := s.ProbePaneExit()
	require.True(t, known)
	require.True(t, dead)
	if !statusKnown {
		out, _ := exec.Command("tmux", "display-message", "-p", "-t", ExactTarget(name),
			"#{pane_dead_status}|#{pane_pid}").Output()
		fields := strings.Split(strings.TrimSpace(string(out)), "|")
		state := "unreadable"
		if pid, err := strconv.Atoi(fields[len(fields)-1]); err == nil {
			_, lookupErr := proctree.Lookup(pid)
			switch {
			case errors.Is(lookupErr, proctree.ErrProcessExited):
				state = "still a zombie: tmux never collected it"
			case pidGone(pid):
				state = "gone"
			default:
				state = "running"
			}
		}
		t.Fatalf("no exit status after one probe (tmux now reports %q; root %s)", out, state)
	}
	return status, at
}

func requireSessionGone(t *testing.T, name string) {
	t.Helper()
	require.Error(t, exec.Command("tmux", "has-session", "-t", ExactTarget(name)).Run(),
		"the held dead pane's session must be gone after teardown")
}

// #3699 via #7435 R6/R9/R14 — the re-hosted shape, end to end: a process
// that setsid'd out of the pane leads its own kernel session while
// carrying the pane's exact markers. Once the session vanishes no kernel
// membership tier can place it — not the recorded root's SID, not a
// recorded pair, not capture inside a tree that no longer exists — and
// before tier 2 every trusting close refused it as unproven, which is
// what wedged root-reap forever. The generation-identity admission
// converges it: exact markers, an in-cohort generation, a start no
// earlier than the recorded pane root, a positively clean ancestry — and
// an executable identity the allowlist accepts: the session's own AGENT
// program or its marked subtree, each admitted by its own identity
// (#7435 R14/R15).
//
// The same close holds the boundary's other half: a pane on a SECOND tmux
// server stamped with the vanished session's exact markers — the #6846
// global-env copy — carries our name but holds no kernel proof, so the
// boundary refuses it: REPORTED, never silently left, and never signalled
// (#7435, Codex 4597265500). The refusal blocks the worktree cleanup while
// the member cohort still converges.
func TestCloseAndWaitForPaneExit_VanishedRehostedAgentConverges(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)
	name := fmt.Sprintf("af_test_rehost_%d", time.Now().UnixNano())
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	gen := "gen-rehost"

	// The re-hosted member: the full two-level `claude daemon run ->
	// bg-pty-host -> claude` shape — a marked host shim (this test binary
	// exec'd through a `claude`-named symlink, so its own argv[0] answers
	// "claude" the way the real re-exec'd host does) re-hosting a marked
	// `claude`-named window. BOTH are admitted by their own executable
	// identity — the host never by its child's (#7435 R14). The session's
	// program is the same `claude`-spelled binary so AgentForCommand
	// resolves the agent identity and tier 2 runs at all (#7435 R15).
	agentSpec := agentSleeperSpec(t)
	host, rehosted := spawnMarkedSessionWithRehostedChild(t, name, home, gen, agentSpec[0], agentSpec[1:]...)

	// Record the pane root while tmux can still answer for it — the stamp
	// Start plants in production. Without it the trusting sweep has no anchor
	// and tier 2 cannot run.
	s := NewTmuxSessionFromSanitizedName(name, agentSpec[0])
	root, err := s.PaneRootProcess()
	require.NoError(t, err)
	require.GreaterOrEqual(t, rehosted.StartID, root.StartID,
		"the re-hosted process must postdate the recorded root for tier 2 to admit it")

	// The unrelated server: the vanished session's exact markers copied onto
	// a pane of a DIFFERENT server — #6846's global-env leak shape. A tmux
	// server sits in its ancestry, so the vanished sweep must exclude it as
	// positively foreign.
	sock2 := testguard.SocketPath(t, "rehostforeign.sock")
	out, err := exec.Command("tmux", "-S", sock2, "new-session", "-d", "-s", "foreign",
		"-e", EnvMarkerSession+"="+name,
		"-e", EnvMarkerHome+"="+home,
		"-e", EnvMarkerGeneration+"="+gen,
		"sleep", "300").CombinedOutput()
	require.NoError(t, err, "start the foreign marked session: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", sock2, "kill-server").Run() })
	var foreignPane int
	require.Eventually(t, func() bool {
		lout, err := exec.Command("tmux", "-S", sock2, "list-panes", "-t", "foreign", "-F", "#{pane_pid}").Output()
		if err != nil {
			return false
		}
		foreignPane, err = strconv.Atoi(strings.TrimSpace(string(lout)))
		return err == nil && foreignPane > 0
	}, 5*time.Second, 20*time.Millisecond, "the foreign pane never reported its pid")
	foreign := processIdentity(t, foreignPane)

	// The session vanishes externally — the #3699 shape: tmux forgot the
	// ancestry before af ever closed, so the close below is the trusting
	// vanished-session sweep, not a captured-tree reap.
	vanishSessionWithObservableEscapee(t, name, host)

	state, blind, err := s.CloseAndWaitForPaneExitTrustingOwnGeneration()
	require.True(t, blind, "a session gone before the close ran is the vanished-sweep path")
	require.Error(t, err,
		"the excluded foreign pane is REPORTED — an excluded marked carrier is never a silent keep")
	require.Equal(t, PaneStateUnknown, state,
		"an unplaceable marked survivor blocks the close from proving the cohort dead")
	require.Contains(t, err.Error(), "pid "+strconv.Itoa(foreign.PID)+" ",
		"the refusal names the foreign pane's pid for the operator")

	require.Eventually(t, func() bool {
		return !proctree.AliveSame(host) && !proctree.AliveSame(rehosted)
	}, 10*time.Second, 50*time.Millisecond,
		"the re-hosted pair (host %d, program %d) must be reaped — tier 2 admits the program plus its marked host",
		host.PID, rehosted.PID)

	// The foreign pane on its own server must be untouched: still alive, its
	// server still answering — refused and reported, never signalled.
	require.True(t, proctree.AliveSame(foreign),
		"the marked process behind a foreign tmux server was signalled — the boundary exclusion failed")
	require.NoError(t, exec.Command("tmux", "-S", sock2, "has-session", "-t", "foreign").Run(),
		"the unrelated tmux server must still own its session")
}

// #7435 R14, on an isolated server — a host is admitted by its OWN
// executable identity, never by its child's. The same trusting close that
// converges the #3699 re-host faces a marked multiplexer pair where one
// window IS the session's program — the `screen` hosting `claude` plus
// `vim` shape. The program-named window admits by its own identity and
// dies; its host does NOT inherit the admission — the one-hop rule that
// let the screen in is gone — so the host, the other window, and a lone
// marked helper are all refused, named in the error, and left alive. A
// refused process's host is never signalled through it.
func TestCloseAndWaitForPaneExit_VanishedTier2RefusesAHostByItsChildsIdentity(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)
	name := fmt.Sprintf("af_test_tier2host_%d", time.Now().UnixNano())
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	gen := "gen-tier2host"

	agentSpec := agentSleeperSpec(t)
	host, rehosted := spawnMarkedSessionWithRehostedChild(t, name, home, gen, agentSpec[0], agentSpec[1:]...)
	s := NewTmuxSessionFromSanitizedName(name, agentSpec[0])
	_, err := s.PaneRootProcess()
	require.NoError(t, err)

	// The screen shape: ONE marked `sh` host carrying two windows — a
	// `claude`-named one that IS the session's program identity and a
	// `tail` vim. The claude window admits by its own identity; the host
	// does not inherit the admission, and the vim window is refused by its
	// own — exactly what the old one-hop rule got wrong.
	screenHost, screenWins := spawnMarkedHostWithWindows(t, name, home, gen,
		agentSleeperSpec(t),
		[]string{"tail", "-f", "/dev/null"})
	claudeWin, vimWin := screenWins[0], screenWins[1]
	// A lone marked orphan that is not the program — the escaped-helper
	// shape: refused, reported, alive.
	helper := spawnMarkedProgram(t, name, home, gen, "tail", "-f", "/dev/null")

	vanishSessionWithObservableEscapee(t, name, host)

	state, blind, err := s.CloseAndWaitForPaneExitTrustingOwnGeneration()
	require.True(t, blind, "a session gone before the close ran is the vanished-sweep path")
	require.Error(t, err,
		"the refused cohort must be REPORTED — an error, never a silent keep and never a signal")
	require.Equal(t, PaneStateUnknown, state,
		"refused survivors mean the teardown cannot prove the cohort dead — "+
			"the close must not claim Known while look-alikes still run")
	for _, p := range []proctree.Process{screenHost, vimWin, helper} {
		require.Contains(t, err.Error(), "pid "+strconv.Itoa(p.PID)+" ",
			"the refusal names pid %d for the operator", p.PID)
	}

	// The session's own re-host pair and the screen's program window die by
	// their own identities; every refused process — including the program
	// window's own host — is still alive.
	require.Eventually(t, func() bool {
		return !proctree.AliveSame(host) && !proctree.AliveSame(rehosted) &&
			!proctree.AliveSame(claudeWin)
	}, 10*time.Second, 50*time.Millisecond,
		"the agent identity is reaped — the re-host pair and the program-named window")
	for _, p := range []proctree.Process{screenHost, vimWin, helper} {
		require.True(t, proctree.AliveSame(p),
			"pid %d (%s) is not the session's program — refused means never signalled", p.PID, p.Comm)
	}
}

// #7435 R15, on an isolated server — tier 2 runs only for an AGENT tab's
// identity. A shell or process tab's program is a general-purpose runtime
// (sh, bash, python, node), so the SAME marked cohort that would converge
// under an agent tab is refused wholesale: a multiplexer pair, a lone
// helper, and — the property's sharp edge — a `claude`-named carrier that
// an agent session WOULD admit. The identity that opens tier 2 is the
// session's own agent, not a name a process happens to answer to.
func TestCloseAndWaitForPaneExit_VanishedTier2RunsOnlyForAgentTabs(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)
	name := fmt.Sprintf("af_test_tier2shell_%d", time.Now().UnixNano())
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	gen := "gen-tier2shell"

	// A shell tab: the recorded program is a general-purpose runtime, so
	// agentProgramExe resolves "" and tier 2 never runs — kernel evidence
	// only.
	escapee := spawnMarkedSessionWithEscapee(t, name, home, gen)
	s := NewTmuxSessionFromSanitizedName(name, "bash pane.sh")
	_, err := s.PaneRootProcess()
	require.NoError(t, err)

	screenHost, screenWin := spawnMarkedHostPair(t, name, home, gen, "tail", "-f", "/dev/null")
	// The sharp edge: a `claude`-named carrier an AGENT session would admit
	// by identity — under a shell tab it is refused like everything else.
	impostorSpec := agentSleeperSpec(t)
	impostor := spawnMarkedProgram(t, name, home, gen, impostorSpec[0], impostorSpec[1:]...)
	// And a carrier named for the tab's OWN runtime: under a gate that
	// resolves ANY program to an identity this would admit — the guard's
	// kill side. Under agent-identity-only it is refused too.
	shellImpostorSpec := namedSleeperSpec(t, "bash")
	shellImpostor := spawnMarkedProgram(t, name, home, gen, shellImpostorSpec[0], shellImpostorSpec[1:]...)

	vanishSessionWithObservableEscapee(t, name, escapee)

	state, blind, err := s.CloseAndWaitForPaneExitTrustingOwnGeneration()
	require.True(t, blind, "a session gone before the close ran is the vanished-sweep path")
	require.Error(t, err,
		"a shell tab's whole marked cohort is refused — tier 2 never opens")
	require.Equal(t, PaneStateUnknown, state)
	for _, p := range []proctree.Process{escapee, screenHost, screenWin, impostor, shellImpostor} {
		require.Contains(t, err.Error(), "pid "+strconv.Itoa(p.PID)+" ",
			"the refusal names pid %d for the operator", p.PID)
		require.True(t, proctree.AliveSame(p),
			"pid %d (%s) is refused — a shell tab's carrier is never signalled", p.PID, p.Comm)
	}
}

// #7435 (Codex 4597265500), on an isolated server — proof is consulted
// BEFORE the ancestry exclusions. A SIGHUP-ignoring pane root whose session
// vanished under a still-live server is still that server's child, so its
// ancestry meets a tmux process — and the tmux-boundary rule, consulted
// ahead of kernel proof, classified this session's own recorded root as a
// foreign pane and left it alive and unreported, where master's sweep
// reaped it. With proof first the recorded (pid, StartID) pair and the pane
// SID it still leads admit it, and the trusting close converges the session
// exactly like the ordinary vanished sweep.
func TestCloseAndWaitForPaneExit_VanishedSighupRootBehindLiveServerConverges(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)
	name := fmt.Sprintf("af_test_sighup_%d", time.Now().UnixNano())
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	gen := "gen-sighup"

	root := spawnSighupIgnoringPaneRoot(t, name, home, gen)
	// The server must outlive the session it is asked to give up — the
	// "shared server lives on" half of the shape.
	testguard.KeepTmuxServerOnEmpty(t)

	s := NewTmuxSessionFromSanitizedName(name, "sleep 300")
	_, err := s.PaneRootProcess()
	require.NoError(t, err, "the recorded root anchor is stamped while tmux can still answer")

	vanishSessionWithSurvivingRoot(t, name, root)

	state, blind, err := s.CloseAndWaitForPaneExitTrustingOwnGeneration()
	require.NoError(t, err,
		"a proven member behind the live server is still a member — the close must converge it")
	require.True(t, blind, "a session gone before the close ran is the vanished-sweep path")
	require.Equal(t, PaneStateKnown, state)

	require.Eventually(t, func() bool {
		return !proctree.AliveSame(root)
	}, 10*time.Second, 50*time.Millisecond,
		"the SIGHUP-ignoring root must be reaped — proof beats the tmux boundary")
}

// The boundary's other half on the same fixture (#7435, Codex 4597265500):
// a pane of a DIFFERENT tmux session on the SAME still-live server, stamped
// with the vanished session's exact markers — the #6846 global-env copy one
// hop over. It carries our session's name but holds no kernel proof, so the
// sweep leaves it ALIVE and REPORTED — an excluded carrier is never
// silently left and never signalled — while the proven root converges.
func TestCloseAndWaitForPaneExit_VanishedSiblingPaneUnderSharedServerLeftAlive(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)
	name := fmt.Sprintf("af_test_sighupsib_%d", time.Now().UnixNano())
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	gen := "gen-sighupsib"
	sibling := "af_sighup_sibling"

	root := spawnSighupIgnoringPaneRoot(t, name, home, gen)
	// The sibling session doubles as the server's reason to stay alive
	// after the victim is killed — no exit-empty pin needed.
	out, err := exec.Command("tmux", "new-session", "-d", "-s", sibling,
		"-e", EnvMarkerSession+"="+name,
		"-e", EnvMarkerHome+"="+home,
		"-e", EnvMarkerGeneration+"="+gen,
		"sleep", "300").CombinedOutput()
	require.NoError(t, err, "start the marked sibling session: %s", out)
	var siblingPane int
	require.Eventually(t, func() bool {
		lout, lerr := exec.Command("tmux", "list-panes", "-t", ExactTarget(sibling),
			"-F", "#{pane_pid}").Output()
		if lerr != nil {
			return false
		}
		siblingPane, lerr = strconv.Atoi(strings.TrimSpace(string(lout)))
		return lerr == nil && siblingPane > 0
	}, 5*time.Second, 20*time.Millisecond, "the sibling pane never reported its pid")
	foreign := processIdentity(t, siblingPane)
	t.Cleanup(func() {
		_ = exec.Command("tmux", "kill-session", "-t", ExactTarget(sibling)).Run()
		_ = proctree.Signal(foreign, syscall.SIGKILL)
	})

	s := NewTmuxSessionFromSanitizedName(name, "sleep 300")
	_, err = s.PaneRootProcess()
	require.NoError(t, err)

	vanishSessionWithSurvivingRoot(t, name, root)

	state, blind, err := s.CloseAndWaitForPaneExitTrustingOwnGeneration()
	require.True(t, blind, "a session gone before the close ran is the vanished-sweep path")
	require.Error(t, err,
		"the excluded sibling is REPORTED — an excluded marked carrier is never a silent keep")
	require.Equal(t, PaneStateUnknown, state)
	require.Contains(t, err.Error(), "pid "+strconv.Itoa(foreign.PID)+" ",
		"the refusal names the sibling's pid for the operator")

	// The proven root still converges — proof is consulted before the
	// boundary — while the unproven sibling pane is left alive and its own
	// tmux session untouched.
	require.Eventually(t, func() bool {
		return !proctree.AliveSame(root)
	}, 10*time.Second, 50*time.Millisecond,
		"the recorded root is reaped by proof even behind the boundary")
	require.True(t, proctree.AliveSame(foreign),
		"the marked sibling under the same server was signalled — an unproven carrier is never ours to kill")
	require.NoError(t, exec.Command("tmux", "has-session", "-t", ExactTarget(sibling)).Run(),
		"the sibling tmux session must be untouched")
}

func TestCloseAndWaitForPaneExit_TearsDownAHeldDeadPane(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)
	name := fmt.Sprintf("af_test_dead_pane_%d", time.Now().UnixNano())
	before := time.Now().Truncate(time.Second)
	program := startHeldPane(t, name, "sh -c 'echo hello; exit 7'")

	s := NewTmuxSessionFromSanitizedName(name, program)
	status, at := probeHeldExit(t, s, name)
	require.Equal(t, 7, status)
	require.False(t, at.Before(before), "death time %v predates the pane", at)

	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state,
		"a pane whose command already exited has nothing left to wait for")
	requireSessionGone(t, name)
	require.True(t, s.ClosedConclusively(), "the observed close must latch its proof")
}

// #4682, on real tmux: Ubuntu's tmux 3.4 leaves about one in sixteen of these
// roots uncollected (25 of 400 measured), so a single pane passes by luck. Many
// panes on one server make the lost SIGCHLD all but certain to occur, and every
// probe must still recover its status. On a tmux that loses nothing (3.6+, or a
// build without utempter) this passes trivially.
//
// An anchor session holds the server for the whole loop. Without it, killing
// each pane's session left the server with none, so it exited (exit-empty) and
// every pane got a fresh server; the next new-session could also reach a server
// already on its way out and fail with "server exited unexpectedly" (#4217).
func TestProbePaneExitRecoversTheStatusOfEveryHeldExit(t *testing.T) {
	testguard.IsolateTmux(t)
	const anchor = "af_test_held_exit_anchor"
	require.NoError(t, exec.Command("tmux", "new-session", "-d", "-s", anchor, "sleep 3600").Run())
	serverPID := func() string {
		out, err := exec.Command("tmux", "display-message", "-p", "-t", ExactTarget(anchor), "#{pid}").Output()
		require.NoError(t, err)
		return strings.TrimSpace(string(out))
	}
	server := serverPID()
	const panes = 60
	for i := range panes {
		name := fmt.Sprintf("af_test_held_exit_%d_%d", time.Now().UnixNano(), i)
		program := startHeldPane(t, name, "sh -c 'echo hello; exit 7'")
		status, _ := probeHeldExit(t, NewTmuxSessionFromSanitizedName(name, program), name)
		require.Equal(t, 7, status, "pane %d", i)
		require.NoError(t, exec.Command("tmux", "kill-session", "-t", ExactTarget(name)).Run())
	}
	require.Equal(t, server, serverPID(), "every pane must be probed on the same tmux server")
}

// The root exited, but a child it backgrounded is still running: it left the
// pane's ppid tree when its parent died, and only its kernel session still ties
// it to the pane. The teardown must find it through that session and stop it.
func TestCloseAndWaitForPaneExit_HeldDeadPaneReapsItsDetachedSurvivor(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)
	name := fmt.Sprintf("af_test_dead_pane_bg_%d", time.Now().UnixNano())
	dir := t.TempDir()
	childFile := filepath.Join(dir, "child.pid")
	// The child writes its pid only after it ignores SIGHUP, and the root exits
	// only after that: the root's exit hangs up the terminal, and a child still
	// starting would die of it and leave nothing to reap.
	childScript := filepath.Join(dir, "child.sh")
	require.NoError(t, os.WriteFile(childScript, []byte(
		"trap '' HUP\necho $$ > \"$1\"\nexec sleep 300 </dev/null >/dev/null 2>&1\n"), 0o600))
	program := startHeldPane(t, name, fmt.Sprintf(
		`sh -c 'sh %[1]s %[2]s & while [ ! -s %[2]s ]; do sleep 0.02; done; exit 3'`,
		strconv.Quote(childScript), strconv.Quote(childFile)))

	childPID := readPIDFile(t, childFile)
	child := processIdentity(t, childPID)
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	require.NotEqual(t, child.SID, childPID, "the fixture must keep the survivor in the dead root's session")

	s := NewTmuxSessionFromSanitizedName(name, program)
	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	requireSessionGone(t, name)
	require.False(t, proctree.AliveSame(child),
		"a detached child of a finished command survived the teardown")
}

// pane_dead is not proof the command exited. A root that closes its terminal
// makes tmux mark the pane dead while the process keeps running, and tmux has
// not reaped it, so no status, signal or death time is reported. The teardown
// must still treat that root as a live pane process.
//
// Linux only: that state is how a Linux pty answers the root closing its last
// terminal descriptor. On macOS (tmux 3.7c in CI) the pane is never marked dead
// while the root runs, so there is no such pane to tear down there.
func TestCloseAndWaitForPaneExit_DeadPaneWithARunningRootStopsTheRoot(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("a pane is held dead with its root running only on Linux ptys, not on %s", runtime.GOOS)
	}
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)
	name := fmt.Sprintf("af_test_dead_pane_eof_%d", time.Now().UnixNano())
	// HUP is ignored before the terminal is closed: closing it is what makes tmux
	// hang the pane up.
	program := startHeldPane(t, name, `sh -c 'trap "" HUP; exec </dev/null >/dev/null 2>&1; exec sleep 300'`)

	out, err := exec.Command("tmux", "display-message", "-p", "-t", ExactTarget(name),
		"#{pane_pid}|#{pane_dead_status}#{pane_dead_signal}#{pane_dead_time}").Output()
	require.NoError(t, err)
	fields := strings.Split(strings.TrimSpace(string(out)), "|")
	require.Len(t, fields, 2)
	require.Empty(t, fields[1], "the fixture must hold a dead pane whose root tmux has not reaped")
	rootPID, err := strconv.Atoi(fields[0])
	require.NoError(t, err)
	root := processIdentity(t, rootPID)
	t.Cleanup(func() { _ = syscall.Kill(rootPID, syscall.SIGKILL) })

	s := NewTmuxSessionFromSanitizedName(name, program)
	dead, _, _, _, known := s.ProbePaneExit()
	require.True(t, known)
	require.False(t, dead, "a pane whose root is still running has not finished")

	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	require.False(t, proctree.AliveSame(root), "the running root of a dead pane survived the teardown")
}

// tmux marks a pane dead when its pty hits EOF, and the kernel closes a dying
// process's descriptors before the process becomes a zombie. A root that closes
// its terminal and exits a moment later is therefore, for that moment, a dead
// pane over a root that is neither collected nor a zombie; one probe must not
// answer "alive" for it (#6439). The fixture stretches the moment to 300ms so it
// is deterministic rather than a 1-in-60 race.
func TestProbePaneExit_ARootThatIsExitingIsNotReportedAlive(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("a pane is held dead with its root running only on Linux ptys, not on %s", runtime.GOOS)
	}
	testguard.IsolateTmux(t)
	prev := paneRootGrace
	paneRootGrace = 5 * time.Second
	t.Cleanup(func() { paneRootGrace = prev })
	name := fmt.Sprintf("af_test_dead_pane_exiting_%d", time.Now().UnixNano())
	program := startHeldPane(t, name, `sh -c 'trap "" HUP; exec </dev/null >/dev/null 2>&1; sleep 0.3; exit 7'`)

	out, err := exec.Command("tmux", "display-message", "-p", "-t", ExactTarget(name),
		"#{pane_dead}|#{pane_dead_status}#{pane_dead_signal}#{pane_dead_time}").Output()
	require.NoError(t, err)
	require.Equal(t, "1|", strings.TrimSpace(string(out)),
		"the fixture must hold a dead pane whose root is still running and uncollected")

	status, _ := probeHeldExit(t, NewTmuxSessionFromSanitizedName(name, program), name)
	require.Equal(t, 7, status)
}

// The probe's status fields and its liveness check are two reads. If tmux collects
// the root between them, the check answers "gone" while the fields it just read
// still had no status; the probe must read again rather than report a finished
// command with an unknown status (#6439). The ordering is forced, not timed: the
// root exits only when the liveness check releases it, and the check is held until
// tmux has recorded the status, which is exactly "root collected after the format
// read, before the check".
func TestProbePaneExit_AStatusRecordedBetweenTheReadAndTheLivenessCheckIsNotLost(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("a pane is held dead with its root running only on Linux ptys, not on %s", runtime.GOOS)
	}
	testguard.IsolateTmux(t)
	name := fmt.Sprintf("af_test_dead_pane_reap_order_%d", time.Now().UnixNano())
	// The root closes its terminal (so tmux holds the pane dead) and then stays up
	// until the injected liveness check lets it go: it cannot exit, and so cannot be
	// collected, before the probe has taken its format read.
	gate := filepath.Join(t.TempDir(), "exit")
	program := startHeldPane(t, name, fmt.Sprintf(
		`sh -c 'trap "" HUP; exec </dev/null >/dev/null 2>&1; while [ ! -f %s ]; do sleep 0.01; done; exit 7'`, gate))

	var raced atomic.Bool
	prev := paneRootGone
	paneRootGone = func(pid int) bool {
		// The first check is the one that follows the format read that saw no
		// status. Let the root exit now, and hold the check until tmux has
		// collected it and recorded 7.
		if raced.CompareAndSwap(false, true) {
			require.NoError(t, os.WriteFile(gate, nil, 0o600))
			require.Eventually(t, func() bool {
				out, err := exec.Command("tmux", "display-message", "-p", "-t", ExactTarget(name), "#{pane_dead_status}").Output()
				return err == nil && strings.TrimSpace(string(out)) == "7"
			}, 5*time.Second, 5*time.Millisecond, "tmux never recorded the root's status")
			return true
		}
		return prev(pid)
	}
	t.Cleanup(func() { paneRootGone = prev })

	status, _ := probeHeldExit(t, NewTmuxSessionFromSanitizedName(name, program), name)
	require.Equal(t, 7, status)
	require.True(t, raced.Load(), "the fixture never reached the liveness check")
}

// shimStartCommand is what #{pane_start_command} answers for a pane af
// launched through its exec shim: the sessionenv marker and the recorded
// command as the wrapper's last word are what paneLaunchIsRecorded reads as
// launch provenance — the fixture sessions run program "true", so that is the
// command the string must end with — and as stampCmd it is what the stamp's
// command pin (@af_pane_cmd) byte-matches against.
const shimStartCommand = "/usr/bin/af " + sessionenv.ExecMarker + " sh 0 true"

// paneState is the part of a dead pane's row the stamp's freshness is judged
// against: the deaths counter the pane-died hook maintains, the pane's
// reported pty name, the command the stamp pinned (@af_pane_cmd), and the
// start command tmux records now. stampCmd == startCmd is what a commandless
// respawn — which re-ran the shim — reports.
type paneState struct {
	deaths, tty, stampCmd, startCmd string
}

// freshPane is the state a shim-stamped pane reports after its first death:
// its own exit is the counter's first increment, so the stamp's deaths field
// is exactly one behind.
func freshPane() paneState {
	return paneState{deaths: "1", tty: "/dev/pts/0", stampCmd: shimStartCommand, startCmd: shimStartCommand}
}

func (s paneState) cmdMatch() string {
	if s.stampCmd == s.startCmd {
		return "1"
	}
	return "0"
}

// heldPaneExec answers every pane query as tmux would for one held dead pane:
// it expands whatever format was asked for, so the fixture tracks the query
// instead of a canned shape. Every command is accepted, including kill-session.
func heldPaneExec(t *testing.T, pid int, status, signal, deadTime string, killed *bool) cmd_test.MockCmdExec {
	return deadPaneExec(t, strconv.Itoa(pid), strconv.Itoa(pid)+":0:/dev/pts/0", freshPane(), status, signal, deadTime, killed)
}

// deadPaneExpand builds the row expander deadPaneExec answers with.
func deadPaneExpand(panePID, stamp string, state paneState, status, signal, deadTime string) *strings.Replacer {
	return strings.NewReplacer(
		"#{pane_pid}", panePID,
		"#{pane_dead}", "1",
		"#{pane_dead_status}", status,
		"#{pane_dead_signal}", signal,
		"#{pane_dead_time}", deadTime,
		"#{"+PanePidMarker+"}", stamp,
		"#{"+paneDeathsMarker+"}", state.deaths,
		"#{pane_tty}", state.tty,
		paneCommandMatchField, state.cmdMatch(),
		// After paneCommandMatchField so the == token is consumed whole: the
		// raw start command is the probe's last field.
		"#{pane_start_command}", state.startCmd,
	)
}

// deadPaneExec is heldPaneExec with independent control over the pid fields
// and the pane's incarnation state: panePID is what #{pane_pid} expands to —
// "" models tmux ≥3.8's held dead pane (#5319) — stamp is what the pane's
// @af_pane_pid bundle answers ("" for a pane that was never recorded), and
// state carries the freshness inputs: the deaths counter, the reported pty
// name, and the stamped/recorded commands the server-side equality reads.
func deadPaneExec(t *testing.T, panePID, stamp string, state paneState, status, signal, deadTime string, killed *bool) cmd_test.MockCmdExec {
	expand := deadPaneExpand(panePID, stamp, state, status, signal, deadTime)
	return cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			if strings.Contains(c.String(), "kill-session") && killed != nil {
				*killed = true
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			s := c.String()
			if strings.Contains(s, "list-panes") || strings.Contains(s, "display-message") {
				return []byte(expand.Replace(c.Args[len(c.Args)-1]) + "\n"), nil
			}
			return nil, nil
		},
	}
}

// laggingDeathsExec is deadPaneExec whose deaths counter stays empty for the
// first 150ms of row reads — pane_dead already visible while the queued
// pane-died hook has not incremented @af_pane_deaths yet (#5335) — then
// counts the death. The lag is wall-clock, not read-counted: the race is a
// server-queue delay, and both the panePID read AND the later list-panes
// capture have to see it for the window to matter.
func laggingDeathsExec(t *testing.T, panePID, stamp string, state paneState, status, signal, deadTime string, killed *bool) cmd_test.MockCmdExec {
	lagging := state
	lagging.deaths = ""
	lagExpand := deadPaneExpand(panePID, stamp, lagging, status, signal, deadTime)
	liveExpand := deadPaneExpand(panePID, stamp, state, status, signal, deadTime)
	var lagUntil time.Time
	return cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			if strings.Contains(c.String(), "kill-session") && killed != nil {
				*killed = true
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			s := c.String()
			if strings.Contains(s, "list-panes") || strings.Contains(s, "display-message") {
				if lagUntil.IsZero() {
					lagUntil = time.Now().Add(150 * time.Millisecond)
				}
				expand := liveExpand
				if time.Now().Before(lagUntil) {
					expand = lagExpand
				}
				return []byte(expand.Replace(c.Args[len(c.Args)-1]) + "\n"), nil
			}
			return nil, nil
		},
	}
}

// uncollectedDeathsExec models the Ubuntu utempter failure mode the deaths
// wait must survive: tmux marks pane_dead but never collects the root's exit
// status — SIGCHLD lost in utempter's SIG_DFL window (#4682) — and pane-died,
// which fires on collection rather than on pane_dead, never increments the
// counter. The first run-shell nudge delivers the SIGCHLD that collects the
// root; the status bundle and the deaths counter then land together.
func uncollectedDeathsExec(t *testing.T, panePID, stamp string, state paneState, status, signal, deadTime string, killed *bool) cmd_test.MockCmdExec {
	before := deadPaneExpand(panePID, stamp,
		paneState{deaths: "", tty: state.tty, stampCmd: state.stampCmd, startCmd: state.startCmd}, "", "", "")
	after := deadPaneExpand(panePID, stamp, state, status, signal, deadTime)
	var collected bool
	return cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			s := c.String()
			if strings.Contains(s, "kill-session") && killed != nil {
				*killed = true
			}
			if strings.Contains(s, "run-shell") {
				collected = true
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			s := c.String()
			if strings.Contains(s, "list-panes") || strings.Contains(s, "display-message") {
				expand := before
				if collected {
					expand = after
				}
				return []byte(expand.Replace(c.Args[len(c.Args)-1]) + "\n"), nil
			}
			return nil, nil
		},
	}
}

// The #4682 shape through the #5335 wait: pane_dead is up, the status bundle
// is empty, and @af_pane_deaths stays unset — not a lagging hook, a death the
// server never collected, so pane-died can never fire. panePID's nudge
// delivers the SIGCHLD that collects the root; the hook then counts the death
// and the stamp resolves fresh.
func TestCloseAndWaitForPaneExit_NudgesTheServerToCountTheDeath(t *testing.T) {
	shrinkReapWaits(t)
	killed := false
	s := newTmuxSession(toTmuxName("dead-pane-uncollected", ""), "sh", NewMockPtyFactory(t),
		uncollectedDeathsExec(t, "", "456:0:/dev/pts/0", freshPane(), "7", "", "1726000000", &killed))

	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	require.True(t, killed, "kill-session still runs — the nudge heals the stamp, not the refusal")
}

// The mock form of the defect: a held dead pane reports a pid that is absent
// from the process table. Refusing it made a finished process tab impossible to
// archive or kill.
func TestCloseAndWaitForPaneExit_HeldPaneWithAnAbsentPidIsConclusive(t *testing.T) {
	gone := exitedProcess(t)
	killed := false
	s := newTmuxSession(toTmuxName("dead-pane-absent", ""), "sh", NewMockPtyFactory(t),
		heldPaneExec(t, gone.PID, "7", "", "1726000000", &killed))

	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	require.True(t, killed, "the dead pane's session must still be killed")
}

// tmux reaped the dead root long ago, and the kernel has since handed its pid to
// an unrelated process. That process is not the pane's: nothing may wait on it,
// and nothing may signal it.
func TestCloseAndWaitForPaneExit_HeldPaneNeverTouchesARecycledPid(t *testing.T) {
	shrinkReapWaits(t)
	stranger := startStranger(t, nil)
	s := newTmuxSession(toTmuxName("dead-pane-recycled", ""), "sh", NewMockPtyFactory(t),
		heldPaneExec(t, stranger.PID, "0", "", "1726000000", nil))

	start := time.Now()
	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	require.Less(t, time.Since(start), paneExitWait, "the teardown waited on a process that is not the pane's")
	require.True(t, proctree.AliveSame(stranger), "the teardown signalled a process that merely reuses the dead pane's pid")
}

// Without a reported status the root may not be reaped yet, and tmux older than
// 3.3 never reports a signal death either, so a live process behind the pid is
// ambiguous. A launch marker naming ANOTHER session settles it: the pid now
// belongs to a different pane.
func TestCloseAndWaitForPaneExit_UnreapedDeadPaneSkipsAPidMarkedForAnotherSession(t *testing.T) {
	shrinkReapWaits(t)
	stranger := startStranger(t, []string{EnvMarkerSession + "=af_someone_else"})
	s := newTmuxSession(toTmuxName("dead-pane-marked", ""), "sh", NewMockPtyFactory(t),
		heldPaneExec(t, stranger.PID, "", "", "", nil))

	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	require.True(t, proctree.AliveSame(stranger), "the teardown signalled another session's process")
}

// The #5319 shape on tmux ≥3.8: a held dead pane whose pane_pid is EMPTY,
// carrying only the @af_pane_pid stamp set at spawn. The stamp names the dead
// root, so its kernel session members are attributed exactly as they were
// while pane_pid still reported it — the detached child gets reaped and the
// teardown completes instead of refusing on an empty pid.
func TestCloseAndWaitForPaneExit_StampedDeadPaneReapsItsDetachedSurvivor(t *testing.T) {
	shrinkReapWaits(t)
	leaderPID, child := startDeadLeaderWithSessionChild(t)
	killed := false
	s := newTmuxSession(toTmuxName("dead-pane-stamped", ""), "sh", NewMockPtyFactory(t),
		deadPaneExec(t, "", strconv.Itoa(leaderPID)+":0:/dev/pts/0", freshPane(), "7", "", "1726000000", &killed))

	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	require.True(t, killed, "the stamped dead pane's session must still be killed")
	require.False(t, proctree.AliveSame(child),
		"the dead root's kernel-session survivor must be reaped through the stamped pid")
}

// The respawn-pane trap the stamp must not fall into (#5328 review): pane
// options survive respawn-pane, so a pane respawned into a command af did
// not launch still carries the OLD root's stamp — and the recorded start
// command changes to the replacement, so even argv containing a launch
// marker fails the pinned-command equality. The deaths counter saw the first
// death and the respawn's, leaving the stamped count two behind.
func TestCloseAndWaitForPaneExit_RespawnedDeadPaneDistrustsItsStaleStamp(t *testing.T) {
	shrinkReapWaits(t)
	leaderPID, child := startDeadLeaderWithSessionChild(t)
	killed := false
	stale := paneState{deaths: "2", tty: "/dev/pts/1", stampCmd: shimStartCommand,
		startCmd: `/bin/sh -c 'echo ` + sessionenv.ExecMarker + `; sleep 5'`}
	s := newTmuxSession(toTmuxName("dead-pane-respawned", ""), "sh", NewMockPtyFactory(t),
		deadPaneExec(t, "", strconv.Itoa(leaderPID)+":0:/dev/pts/0", stale, "7", "", "1726000000", &killed))

	state, err := s.CloseAndWaitForPaneExit()
	require.Error(t, err)
	require.Equal(t, PaneStateUnknown, state)
	require.True(t, killed, "kill-session still runs — refusal is about the process tree, not the session")
	require.Contains(t, err.Error(), "respawn",
		"the refusal must say why the recorded stamp cannot be trusted")
	require.True(t, proctree.AliveSame(child),
		"an unattributed survivor must not be reaped on a stale stamp's say-so")
}

// The -k respawn shape Codex P1'd (#5328 review): `respawn-pane -k` on a live
// pane produces NO pane-died for the kill, and the devpts index freed by the
// kill can be handed straight back to the replacement — so a respawn that
// never restamped can leave a row that looks fresh on every field the row
// itself carries: deaths gap of exactly one (only the new run's exit counted),
// the recorded command unchanged, and the very same /dev/pts/N name. No
// post-mortem proof can see it — the pty's node is gone the moment the master
// closes — so the guarantee lives one layer down, in the shim: a respawn
// re-executes the launch shim, which refuses to run the command when its
// bookkeeping write fails over an existing stamp (sessionenv). When that
// refusal holds there is no second incarnation to hide behind the record, so
// the row the teardown reads here is INC1's own — the only run the trusted
// stamp can ever name, and the only session this resolution may touch.
func TestCloseAndWaitForPaneExit_RecycledPtyNameTrustOnlyNamesTheDeadRun(t *testing.T) {
	shrinkReapWaits(t)
	leaderPID, child := startDeadLeaderWithSessionChild(t)
	killed := false
	stranger := startStranger(t, nil)
	// deaths "1" vs the stamp's "0" — the -k kill produced no counter move —
	// same tty NAME at a different allocation, same command: indistinguishable
	// from a first-death row, and trusted as one. What the trust resolves is
	// bounded to the stamped pid's session: the dead run's own survivor is
	// reaped, a process in any other session is not touched.
	fresh := paneState{deaths: "1", tty: "/dev/pts/0", stampCmd: shimStartCommand, startCmd: shimStartCommand}
	s := newTmuxSession(toTmuxName("dead-pane-pty-recycled", ""), "sh", NewMockPtyFactory(t),
		deadPaneExec(t, "", strconv.Itoa(leaderPID)+":0:/dev/pts/0", fresh, "7", "", "1726000000", &killed))

	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	require.True(t, killed)
	require.False(t, proctree.AliveSame(child),
		"the dead run's own kernel-session survivor must be reaped")
	require.True(t, proctree.AliveSame(stranger),
		"a process outside the stamped run's session must not be touched")
}

// The positive half of the same respawn: a commandless respawn re-runs af's
// launch shim, which restamps with the NEW root's pid, the deaths count it
// saw at birth, and the fresh pty — the second death then reads as fresh and
// the teardown completes through the new stamp.
func TestCloseAndWaitForPaneExit_RespawnedShimPaneStampStaysFresh(t *testing.T) {
	shrinkReapWaits(t)
	leaderPID, child := startDeadLeaderWithSessionChild(t)
	killed := false
	restamped := paneState{deaths: "2", tty: "/dev/pts/1", stampCmd: shimStartCommand, startCmd: shimStartCommand}
	s := newTmuxSession(toTmuxName("dead-pane-respawned-fresh", ""), "sh", NewMockPtyFactory(t),
		deadPaneExec(t, "", strconv.Itoa(leaderPID)+":1:/dev/pts/1", restamped, "7", "", "1726000000", &killed))

	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	require.True(t, killed)
	require.False(t, proctree.AliveSame(child),
		"a survivor of a shim-respawned pane's dead root must be reaped through the restamped pid")
}

// #5335: tmux exposes pane_dead=1 before the queued pane-died hook has
// incremented @af_pane_deaths, so a row read in that window shows a deaths
// gap of 0 with every other pin intact — a hook still running, not a stale
// record. The teardown must re-read the row until the counter lands rather
// than refuse the fresh stamp.
func TestCloseAndWaitForPaneExit_WaitsForTheDeathsHook(t *testing.T) {
	shrinkReapWaits(t)
	leaderPID, child := startDeadLeaderWithSessionChild(t)
	killed := false
	s := newTmuxSession(toTmuxName("dead-pane-hook-lag", ""), "sh", NewMockPtyFactory(t),
		laggingDeathsExec(t, "", strconv.Itoa(leaderPID)+":0:/dev/pts/0", freshPane(), "7", "", "1726000000", &killed))

	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	require.True(t, killed, "the stamped dead pane's session must still be killed")
	require.False(t, proctree.AliveSame(child),
		"the dead root's kernel-session survivor must be reaped through the stamp once the hook lands")
}

// The same window held open forever — the counter never counts the death —
// must still fail closed inside a bounded wait: a missing or clobbered
// pane-died hook is exactly what a gap of 0 cannot distinguish from lag, so
// the teardown waits, then refuses with the hook named.
func TestCloseAndWaitForPaneExit_PendingStampRefusesWhenTheHookNeverLands(t *testing.T) {
	shrinkReapWaits(t)
	old := paneDeathsWait
	paneDeathsWait = 200 * time.Millisecond
	t.Cleanup(func() { paneDeathsWait = old })
	killed := false
	pending := paneState{deaths: "0", tty: "/dev/pts/0", stampCmd: shimStartCommand, startCmd: shimStartCommand}
	s := newTmuxSession(toTmuxName("dead-pane-hook-missing", ""), "sh", NewMockPtyFactory(t),
		deadPaneExec(t, "", "456:0:/dev/pts/0", pending, "7", "", "1726000000", &killed))

	state, err := s.CloseAndWaitForPaneExit()
	require.Error(t, err)
	require.Equal(t, PaneStateUnknown, state)
	require.True(t, killed, "kill-session still runs — refusal is about the process tree, not the session")
	require.ErrorIs(t, err, errPaneStampPending,
		"the wait expiring must keep the pending verdict, not degrade it into a tmux timeout")
	require.Contains(t, err.Error(), "pane-died",
		"the refusal must name the counter that never counted the death")
}

// The #5335 race on real tmux, held open by the fixture: the pane-died hook
// is delayed behind a blocking run-shell, so pane_dead=1 is observable while
// @af_pane_deaths is still unset — the window CI hit (measured on tmux 3.8).
// On tmux ≥3.8 the stamp is what the teardown consults, so this is the
// end-to-end proof; on older tmux pane_pid still reports the root and the
// stamp is unused, which makes the same run a no-op pass.
func TestCloseAndWaitForPaneExit_DeathsHookLagOnRealTmux(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)
	name := fmt.Sprintf("af_test_deaths_lag_%d", time.Now().UnixNano())
	program := startHeldPaneWithHookLag(t, name, "sh -c 'exit 7'")

	s := NewTmuxSessionFromSanitizedName(name, program)
	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	requireSessionGone(t, name)
}

// A held dead pane that was never stamped — created by a pre-#5319 af, or not
// created by af at all — reports nothing usable on tmux ≥3.8: pane_pid is
// empty and @af_pane_pid is unset. Teardown must fail closed exactly as
// master does on any unprovable capture: kill-session still runs, but the
// answer is an error that leaves the workspace intact and says how to finish
// manually. There is no marker-attribution fallback for it in this PR.
func TestCloseAndWaitForPaneExit_UnstampedDeadPaneRefuses(t *testing.T) {
	shrinkReapWaits(t)
	killed := false
	s := newTmuxSession(toTmuxName("dead-pane-unstamped", ""), "sh", NewMockPtyFactory(t),
		deadPaneExec(t, "", "", paneState{}, "7", "", "1726000000", &killed))

	state, err := s.CloseAndWaitForPaneExit()
	require.Error(t, err)
	require.Equal(t, PaneStateUnknown, state)
	require.True(t, killed, "kill-session still runs — refusal is about the process tree, not the session")
	require.Contains(t, err.Error(), PanePidMarker,
		"the refusal must name what the pane lacks")
	require.Contains(t, err.Error(), "af sessions kill",
		"the refusal must name the step that actually finishes cleanup — the recorded kill retries automatically, and a re-run drives it (measured on tmux 3.8: the tombstoned kill converged via the vanished-session sweep once the session was gone)")
}

// pid-bearing rows stay pane_pid's rows: the stamp is a fallback authority
// consulted only when tmux reports none, and only while the stamp's
// incarnation proof holds — pane options outlive respawn-pane, so a stamp on
// a pane respawned or never properly stamped is stale.
func TestParsePaneRowStampedPid(t *testing.T) {
	for _, tc := range []struct {
		field   string
		wantPID int
		dead    bool
		reaped  bool
		wantErr string
	}{
		{"123|0||123:0:/dev/pts/0|0|/dev/pts/0|1", 123, false, false, ""},                  // live, stamped, agrees
		{"123|0||", 123, false, false, ""},                                                 // live, unstamped — pane_pid rules
		{"|1|71726000000|456:0:/dev/pts/0|1|/dev/pts/0|1", 456, true, true, ""},            // 3.8 dead row: fresh stamp answers
		{"|1|71726000000|456:0:/dev/pts/0|1|/dev/pts/0|0", 0, true, true, "respawn"},       // command pin moved
		{"|1|71726000000|456:0:/dev/pts/0|2|/dev/pts/0|1", 0, true, true, "respawn"},       // deaths gap > 1
		{"|1|71726000000|456:0:/dev/pts/0|1|/dev/pts/9|1", 0, true, true, "respawn"},       // pty moved
		{"|1|71726000000|456:0:/dev/pts/0|0|/dev/pts/0|1", 0, true, true, "pane-died"},     // no death counted — hook still queued
		{"|1|71726000000|456:0|1|/dev/pts/0|1", 0, true, true, "respawn"},                  // two-part stamp: no pty pin
		{"|1|71726000000|456|1|/dev/pts/0|1", 0, true, true, "predates"},                   // pre-proof bare-pid stamp
		{"|1|71726000000|456", 0, true, true, "predates"},                                  // no freshness fields at all
		{"|1|71726000000|:0:/dev/pts/0|1|/dev/pts/0|1", 0, true, true, "af sessions kill"}, // torn stamp names nobody — reads as absent
		{"123|1|71726000000|456:0:/dev/pts/0|9|/dev/pts/9|0", 123, true, true, ""},         // populated pane_pid wins over any stamp
		{"|1|71726000000|", 0, true, true, "af sessions kill"},
		{"|0||", 0, false, false, "invalid pane pid"},
		{"abc|1|71726000000|", 0, true, true, "invalid pane pid"},
	} {
		row, err := parsePaneRow(tc.field)
		if tc.wantErr != "" {
			require.ErrorContains(t, err, tc.wantErr, tc.field)
			continue
		}
		require.NoError(t, err, tc.field)
		require.Equal(t, tc.wantPID, row.pid, tc.field)
		require.Equal(t, tc.dead, row.dead, tc.field)
		require.Equal(t, tc.reaped, row.reaped, tc.field)
	}
}

// The production stamp on real tmux: a held dead pane still answers
// @af_pane_pid, the bundle's first field is exactly the pane's live root pid —
// equal to pane_pid wherever tmux still reports it — and the freshness inputs
// the dead row must satisfy are all present: one death counted, the pty
// unchanged, the start command pinned.
//
// pane_dead is observable before the queued pane-died hook increments
// @af_pane_deaths (#5335), so the row is re-read until the stamp's verdict is
// fresh — the same condition teardown waits on — instead of expecting one
// read to see the hook already landed.
func TestStartHeldPaneStampsItsRootPid(t *testing.T) {
	testguard.IsolateTmux(t)
	name := fmt.Sprintf("af_test_dead_pane_stamp_%d", time.Now().UnixNano())
	startHeldPane(t, name, "sh -c 'exit 7'")

	var fields []string
	var lastErr error
	verdict := stampUnusable
	nudges := 0
	// pane_dead itself lands quickly; the hook is a queued tmux command and a
	// loaded box can hold it for seconds, so this wait is deliberately longer
	// than the production paneDeathsWait: the test is measuring that the hook
	// lands at all, not how fast a teardown should give up waiting for it.
	// assert.Eventually reports its message eagerly, so the diagnosis is
	// asserted separately below with the values the callback last saw.
	gotFresh := assert.Eventually(t, func() bool {
		out, err := exec.Command("tmux", "display-message", "-p", "-t", ExactTarget(name), paneRowFormat).Output()
		if err != nil {
			lastErr = err
			return false
		}
		lastErr = nil
		fields = strings.Split(strings.TrimSpace(string(out)), "|")
		// paneRowFormat appends fields (the server identity rides the tail,
		// #7469): read the stable prefix positions, not the count.
		if len(fields) < 7 {
			return false
		}
		// pane-died fires when the server collects the exit status, not on
		// pane_dead: an Ubuntu-class tmux can lose the root's SIGCHLD in
		// utempter's SIG_DFL window (#4682), which leaves dead with no
		// status bundle and never runs the hook — ~1/4 of held deaths on a
		// loaded 3.4. Teardown recovers by nudging the server (nudgeReap);
		// the test does the same so it measures the counter, not the kernel
		// race.
		if fields[1] == "1" && fields[2] == "" && nudges < maxReapNudges {
			nudges++
			_ = exec.Command("tmux", "run-shell", "-b", "true").Run()
		}
		_, verdict = paneStampPID(fields[3], fields[4], fields[5], fields[6])
		return verdict == stampFresh
	}, 30*time.Second, 25*time.Millisecond)
	require.True(t, gotFresh,
		"the pane-died hook never counted this death (last row %q, verdict %d, err %v)", fields, verdict, lastErr)
	require.Equal(t, "1", fields[1])
	stampParts := strings.SplitN(fields[3], ":", 3)
	require.Len(t, stampParts, 3, "the record must carry pid:deaths:tty, got %q", fields[3])
	require.NotEmpty(t, stampParts[0], "the spawn chain must record %s", PanePidMarker)
	require.Equal(t, "1", fields[4], "the pane-died hook must have counted this death")
	require.Equal(t, fields[5], stampParts[2], "the stamped tty must be the pane's own pty")
	require.Equal(t, "1", fields[6], "the pinned start command must be the pane's own")
	if fields[0] != "" {
		require.Equal(t, fields[0], stampParts[0],
			"where tmux still reports pane_pid, the stamp must record the same pid")
	}
}

// Codex 4597265491 — every successful fresh spawn records its OWN pane
// root's evidence. A respawn of an existing TmuxSession (restored shell
// tabs) reaches Start with the previous launch's paneRootEvidence already
// populated; the nil-gate skipped the restamp, so the recorded anchor
// stayed a different pane incarnation's pair forever and tier 2's
// no-earlier-than bound anchored on the wrong root. Seeding a stale pair
// and spawning once exercises the same gate: the stamp must name the pane
// tmux just answered for.
func TestStartRestampsPaneRootEvidenceOnEverySpawn(t *testing.T) {
	testguard.IsolateTmux(t)
	session := NewTmuxSession("af-restamp-anchor", "sleep 300")
	session.SeedPaneRootEvidence(424242, 1)

	require.NoError(t, session.Start(t.TempDir()))
	t.Cleanup(func() { _, _ = session.Close() })

	pid, startID, ok := session.PaneRootEvidence()
	require.True(t, ok, "a successful spawn must leave pane-root evidence")
	require.NotEqual(t, 424242, pid,
		"the previous launch's anchor must be replaced by THIS spawn's root")
	require.NotEqual(t, uint64(1), startID,
		"the previous launch's pair is replaced, not kept")
	matches, err := session.PaneRootProcessMatches(pid, startID)
	require.NoError(t, err)
	require.True(t, matches,
		"the restamped pair must name a pane tmux still answers for")
}

// The pane-root anchor rules share ONE table — paneRootEvidenceAfter — and
// ONE writer that applies it (#7484 item 3): an answered mismatch retires
// the seeded pair only (R11), an unanswered query keeps whatever stands
// (R17), a root that exited before capture keeps the previous anchor (Codex
// 4596311717), a fresh spawn records its own root (Codex 4597265491), and a
// persisted restore seed fills only an empty cell. Exercised as a pure
// function so every rule and the races between them are decided without a
// tmux server — the two keeps are DISTINCT inputs even though they produce
// the same verdict, because the answer a site reports is its own evidence.
func TestPaneRootEvidenceTableIsTheOnlyRule(t *testing.T) {
	old := &proctree.Process{PID: 4242, StartID: 99}
	newer := &proctree.Process{PID: 7777, StartID: 55}
	for _, tc := range []struct {
		name   string
		cur    *proctree.Process
		answer paneRootAnswer
		want   *proctree.Process
		// wantVerdict names the rule the row must fire — the observable
		// that keeps the two keep inputs DISTINCT rows: a mutation that
		// drops one keep kind from the answer's fields returns
		// verdictKept here instead of the kind the row asserts (#7491
		// item 4).
		wantVerdict paneRootVerdict
	}{
		{"answered mismatch retires the claim's seeded pair (R11)", old,
			paneRootAnswer{disproven: *old}, nil, paneRootVerdictRetired},
		{"answered mismatch leaves a different claim's pair standing", newer,
			paneRootAnswer{disproven: *old}, newer, paneRootVerdictKept},
		{"unanswered query keeps the anchor (R17)", old,
			paneRootAnswer{keep: paneRootKeepUnanswered}, old, paneRootVerdictKeptUnanswered},
		{"root exited before capture keeps the previous anchor", old,
			paneRootAnswer{keep: paneRootKeepExited}, old, paneRootVerdictKeptExited},
		{"a fresh spawn records its own root", old,
			paneRootAnswer{resolved: *newer}, newer, paneRootVerdictResolved},
		{"a fresh spawn supersedes even when the re-stamp cannot answer", old,
			paneRootAnswer{supersede: true}, nil, paneRootVerdictSuperseded},
		{"supersede then resolve lands the new root", old,
			paneRootAnswer{supersede: true, resolved: *newer}, newer, paneRootVerdictResolved},
		{"a live resolution outranks a racing disproof", newer,
			paneRootAnswer{resolved: *newer, disproven: *old}, newer, paneRootVerdictResolved},
		{"no anchor to retire", nil,
			paneRootAnswer{disproven: *old}, nil, paneRootVerdictKept},
		{"a restore seed fills an empty cell", nil,
			paneRootAnswer{seed: old}, old, paneRootVerdictSeeded},
		{"a restore seed never displaces a standing anchor", newer,
			paneRootAnswer{seed: old}, newer, paneRootVerdictKept},
		{"supersede clears, then the seed fills", old,
			paneRootAnswer{supersede: true, seed: newer}, newer, paneRootVerdictSeeded},
		{"a disproof in the same answer outranks a seed", old,
			paneRootAnswer{disproven: *old, seed: newer}, nil, paneRootVerdictRetired},
		{"a keep keeps an empty cell empty", nil,
			paneRootAnswer{keep: paneRootKeepUnanswered}, nil, paneRootVerdictKeptUnanswered},
		{"an exited keep keeps an empty cell empty", nil,
			paneRootAnswer{keep: paneRootKeepExited}, nil, paneRootVerdictKeptExited},
		{"an answer that names nothing keeps", old,
			paneRootAnswer{}, old, paneRootVerdictKept},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, verdict := paneRootEvidenceAfter(tc.cur, tc.answer)
			require.Equal(t, tc.wantVerdict, verdict,
				"the rule that fired must name THIS input's kind — two keep kinds are not interchangeable")
			if tc.want == nil {
				require.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			require.Equal(t, *tc.want, *got)
		})
	}
}

// The reconcile writer applies the table to the cell atomically. In sequence
// it composes like the pure table; under a racing storm every goroutine's
// verdict is computed against a value that actually won a CAS, so the cell
// only ever holds a pair some input produced — never a write built on a cell
// the caller already lost.
func TestReconcilePaneRootEvidenceAppliesTheTableAtomically(t *testing.T) {
	old := proctree.Process{PID: 4242, StartID: 99}
	newer := proctree.Process{PID: 7777, StartID: 55}
	third := proctree.Process{PID: 9001, StartID: 12}

	t.Run("serial answers compose like the pure table", func(t *testing.T) {
		s := &TmuxSession{}
		s.reconcilePaneRootEvidence(paneRootAnswer{seed: &old})
		pid, startID, ok := s.PaneRootEvidence()
		require.True(t, ok)
		require.Equal(t, proctree.Process{PID: 4242, StartID: 99},
			proctree.Process{PID: pid, StartID: startID},
			"the seed fills the empty cell")
		s.reconcilePaneRootEvidence(paneRootAnswer{seed: &newer})
		pid, _, _ = s.PaneRootEvidence()
		require.Equal(t, 4242, pid, "a second seed loses to the standing anchor")
		s.reconcilePaneRootEvidence(paneRootAnswer{disproven: old})
		_, _, ok = s.PaneRootEvidence()
		require.False(t, ok, "the disproof retires the seeded pair")
		s.reconcilePaneRootEvidence(paneRootAnswer{resolved: newer})
		pid, startID, ok = s.PaneRootEvidence()
		require.True(t, ok)
		require.Equal(t, newer, proctree.Process{PID: pid, StartID: startID})
	})

	t.Run("a racing storm lands only pairs an input produced", func(t *testing.T) {
		s := &TmuxSession{}
		answers := []paneRootAnswer{
			{seed: &old},
			{resolved: newer},
			{supersede: true, resolved: third},
			{disproven: old},
			{keep: paneRootKeepUnanswered},
			{keep: paneRootKeepExited},
		}
		var wg sync.WaitGroup
		for _, a := range answers {
			wg.Add(1)
			go func(a paneRootAnswer) {
				defer wg.Done()
				s.reconcilePaneRootEvidence(a)
			}(a)
		}
		wg.Wait()
		// Every table output names one of the input pairs, or the empty
		// cell — whichever interleaving won, the cell is coherent.
		legal := map[proctree.Process]bool{old: true, newer: true, third: true}
		if cur := s.paneRootEvidence.Load(); cur != nil {
			require.True(t, legal[*cur],
				"the cell holds %v — a pair no answer ever produced", *cur)
		}
	})

	t.Run("a racing storm converges to the serial order's winner", func(t *testing.T) {
		// The oracle: seed and resolved racing — EVERY serial order of
		// the two inputs converges on the resolved pair (seed fills only
		// an empty cell; resolved replaces anything). A reconcile built
		// as Load-then-Store would let the seed's write land AFTER the
		// resolved write it lost to, so this goes red on any lost update
		// (#7491 item 4).
		s := &TmuxSession{}
		var start, wg sync.WaitGroup
		start.Add(2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			start.Done()
			start.Wait()
			for range 300 {
				s.reconcilePaneRootEvidence(paneRootAnswer{seed: &old})
			}
		}()
		go func() {
			defer wg.Done()
			start.Done()
			start.Wait()
			for range 300 {
				s.reconcilePaneRootEvidence(paneRootAnswer{resolved: newer})
			}
		}()
		wg.Wait()
		cur := s.paneRootEvidence.Load()
		require.NotNil(t, cur)
		require.Equal(t, newer, *cur,
			"every serial order converges on the resolved root — the seed surviving is a lost update")
	})

	t.Run("a racer parked in the Load-CAS gap cannot resurrect its verdict", func(t *testing.T) {
		// The deterministic lost update (#7494 item 4): park the seed's
		// reconcile between Load and CAS, let resolved win the cell, then
		// release. Under the real CAS loop the seed's swap fails on the
		// newer cell and its retry keeps it; under Load-then-Store the
		// parked write lands on top — the cell ends at `old`, red.
		s := &TmuxSession{}
		seated := make(chan struct{})
		release := make(chan struct{})
		var parked atomic.Bool
		oldGap := paneRootReconcileGap
		paneRootReconcileGap = func() {
			if parked.CompareAndSwap(false, true) {
				close(seated)
				<-release
			}
		}
		t.Cleanup(func() { paneRootReconcileGap = oldGap })

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.reconcilePaneRootEvidence(paneRootAnswer{seed: &old})
		}()
		<-seated
		s.reconcilePaneRootEvidence(paneRootAnswer{resolved: newer})
		close(release)
		wg.Wait()

		cur := s.paneRootEvidence.Load()
		require.NotNil(t, cur)
		require.Equal(t, newer, *cur,
			"the verdict the parked load lost to must stand — a stale Store resurrects the seed")
	})
}

// startDeadLeaderWithSessionChild spawns a kernel-session leader with one
// long-lived member, then kills the leader: the child is orphaned but keeps
// the leader's pid as its SID, which is exactly how a held dead pane's
// backgrounded survivor still names its pane. Returns (leaderPID, child).
func startDeadLeaderWithSessionChild(t *testing.T) (leaderPID int, child proctree.Process) {
	t.Helper()
	childFile := filepath.Join(t.TempDir(), "child.pid")
	cmd := exec.Command("sh", "-c",
		"sleep 300 & echo $! > \"$1\"; kill -9 $$", "sh", childFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, cmd.Start())
	_ = cmd.Wait()
	childPID := readPIDFile(t, childFile)
	child = processIdentity(t, childPID)
	require.Equal(t, cmd.Process.Pid, child.SID,
		"the fixture must leave the survivor in the dead leader's session")
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	return cmd.Process.Pid, child
}

// startStranger starts a process the teardown under test must leave alone.
//
// It leads its own kernel session, and that is a safety property rather than
// realism. This package's test binary is named tmux.test, so its direct children
// pass the "child of a tmux server" check. A teardown that wrongly adopts the
// stranger as a pane root also reaps the stranger's whole session. Without
// Setsid, that session is the one `go test` runs in, and the unfixed code killed
// the shell that launched the test.
func startStranger(t *testing.T, extraEnv []string) proctree.Process {
	t.Helper()
	c := exec.Command("sleep", "300")
	c.Env = append(os.Environ(), extraEnv...)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, c.Start())
	t.Cleanup(func() {
		_ = c.Process.Kill()
		_, _ = c.Process.Wait()
	})
	return processIdentity(t, c.Process.Pid)
}

func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	var pid int
	require.Eventually(t, func() bool {
		raw, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(raw)))
		return err == nil && pid > 1
	}, 3*time.Second, 20*time.Millisecond)
	return pid
}
