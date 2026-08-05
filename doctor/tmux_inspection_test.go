package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// These tests pin the same property #1939 pinned for the process table, on the
// other input `af doctor` reasons about: A FAILED READ IS NOT AN EMPTY RESULT.
//
// The stakes here are higher than #1939's. An unreadable process table made
// doctor report a clean bill of health it had not earned. An unreadable tmux
// SESSION LIST made it report the opposite — every live session looked dead, so
// every process carrying this home's markers looked orphaned, and `--fix`
// executes those findings' kill closures. Blindness rendered as a work order.

// realTmuxExitError produces a genuine *exec.ExitError carrying diagnostic on
// stderr, which is the only shape the classifier can read: an error hand-rolled
// with fmt.Errorf has no Stderr, and tmux's exit status alone cannot separate
// "there is no server" from "I could not reach the server".
func realTmuxExitError(t *testing.T, diagnostic string, code int) error {
	t.Helper()
	c := exec.Command("sh", "-c", `printf '%s\n' "$DIAG" >&2; exit "$CODE"`)
	c.Env = append(os.Environ(), "DIAG="+diagnostic, fmt.Sprintf("CODE=%d", code))
	_, err := c.Output()
	require.Error(t, err, "the fixture must actually fail, or it proves nothing")
	return err
}

// blindTmuxLsExec passes every tmux command through to the real executor EXCEPT
// `tmux ls`, which fails the way an unreachable socket does. Only the listing is
// broken, so the checks below fail for the reason under test rather than because
// nothing tmux-shaped works.
func blindTmuxLsExec(t *testing.T, diagnostic string, code int) cmd.Executor {
	t.Helper()
	real := cmd.MakeExecutor()
	return cmd_test.MockCmdExec{
		RunFunc: real.Run,
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if len(c.Args) > 1 && c.Args[1] == "ls" {
				return nil, realTmuxExitError(t, diagnostic, code)
			}
			return real.Output(c)
		},
	}
}

// TestUnreadableTmuxSessionListDoesNotArmKills is the #2874 regression, and the
// reason the fixture goes to the trouble of standing up a REAL live tmux
// session: without one, the process doctor proposes to kill really would be an
// orphan and killing it would be correct. Here the session is alive, so the kill
// is unambiguously wrong — it is only proposed because the listing that would
// have proved the session live could not be read.
//
// Observed on master: doctor reports the process as a verified orphan with
// `kill pid N` attached, and --fix kills it. On the maintainer's box that is
// every agent in every live session.
func TestUnreadableTmuxSessionListDoesNotArmKills(t *testing.T) {
	testguard.IsolateTmux(t) // private server: nothing here can see or touch a real one
	home := testguard.SocketTempDir(t)

	const name = "af_doctor-live-while-blind"
	out, err := exec.Command("tmux", "new-session", "-d", "-s", name,
		"-e", tmux.EnvMarkerSession+"="+name, "-e", tmux.EnvMarkerHome+"="+home,
		"sleep", "300").CombinedOutput()
	require.NoError(t, err, "tmux new-session: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+name+":").Run() })

	// A process of that LIVE session, carrying this home's markers — the exact
	// shape checkOrphanedProcesses arms a kill for once the session looks dead.
	victim := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: name,
		tmux.EnvMarkerHome:    home,
	})

	opts := testOptionsWithHome(t, home, true, victim.PID) // Fix: true
	opts.Exec = blindTmuxLsExec(t, "error connecting to /tmp/tmux-1000/default (Permission denied)", 1)

	report, err := Run(opts)
	require.NoError(t, err)

	require.True(t, alive(victim),
		"`af doctor --fix` killed a LIVE session's process because it could not list tmux sessions — "+
			"an unreadable list was read as 'no sessions are live' (#2874)")
	require.Empty(t, findByCheck(report, "orphaned-process"),
		"nothing may be classified as orphaned from a session list doctor could not read")
	for _, f := range report.Findings {
		require.Empty(t, f.FixAction,
			"no fix may be armed on a run that could not determine session liveness: %s / %s", f.Check, f.Detail)
	}
}

// TestUnreadableTmuxSessionListFailsRatherThanPasses is the honesty half: the
// operator must be told the session-dependent checks could not run. Silence
// reads as "no orphans found", which is exactly the conclusion doctor has not
// earned — the same rule checkProcessInspection applies to the process table.
func TestUnreadableTmuxSessionListFailsRatherThanPasses(t *testing.T) {
	home := testguard.SocketTempDir(t)
	opts := testOptionsWithHome(t, home, false)
	opts.Exec = blindTmuxLsExec(t, "error connecting to /tmp/tmux-1000/default (Permission denied)", 1)

	report, err := Run(opts)
	require.NoError(t, err)

	rows := findCheckRows(report, "tmux-inspection")
	require.Len(t, rows, 1, "an unreadable tmux session list must report exactly one tmux-inspection row")
	require.Equal(t, StatusFail, rows[0].Status,
		"doctor reported %s for a session list it could not read — blindness must never render as health", rows[0].Status)
	require.True(t, rows[0].Problem,
		"the failure must count toward the exit code, or `af doctor` still exits 0 while blind")
	require.NotZero(t, report.UnresolvedCount())
	require.Contains(t, rows[0].Detail, "Permission denied",
		"the row must carry tmux's own diagnostic — `exit status 1` is not actionable")
}

// TestTmuxSessionListBlindnessIsVisibleInRenderedOutput checks what the user
// actually reads. The struct being right is not the product; the page is.
func TestTmuxSessionListBlindnessIsVisibleInRenderedOutput(t *testing.T) {
	home := testguard.SocketTempDir(t)
	opts := testOptionsWithHome(t, home, false)
	opts.Exec = blindTmuxLsExec(t, "error connecting to /tmp/tmux-1000/default (Permission denied)", 1)

	report, err := Run(opts)
	require.NoError(t, err)

	var buf strings.Builder
	Render(&buf, report, false, false)
	require.Contains(t, buf.String(), "tmux-inspection")
	require.Contains(t, buf.String(), "cannot list tmux sessions")
	require.Contains(t, strings.ToUpper(buf.String()), "FAIL")
}

// TestListedTmuxSessionsPass is the other half of the contract: when the list IS
// readable the row passes, so the FAIL above is a signal rather than a row that
// is always red. Both tmux diagnostics that definitively mean "no server" count
// as READ — a machine with no tmux server is not a blind machine, and doctor
// must not go red on one.
func TestListedTmuxSessionsPass(t *testing.T) {
	home := testguard.SocketTempDir(t)

	for _, tc := range []struct {
		name       string
		exec       func(*testing.T) cmd.Executor
		wantDetail string
	}{
		{
			name: "sessions listed",
			exec: func(t *testing.T) cmd.Executor {
				return cmd_test.MockCmdExec{
					RunFunc: func(*exec.Cmd) error { return nil },
					OutputFunc: func(c *exec.Cmd) ([]byte, error) {
						if len(c.Args) > 1 && c.Args[1] == "ls" {
							return []byte("af_one\naf_two\n"), nil
						}
						return []byte(""), nil
					},
				}
			},
			wantDetail: "2",
		},
		{
			// The socket was never created: the ordinary answer on a machine
			// with no tmux server.
			name: "socket absent",
			exec: func(t *testing.T) cmd.Executor {
				return blindTmuxLsExec(t, "error connecting to /tmp/tmux-1000/default (No such file or directory)", 1)
			},
			wantDetail: "0",
		},
		{
			// A server that exited leaves its socket behind, so the connect is
			// refused.
			name: "server exited",
			exec: func(t *testing.T) cmd.Executor {
				return blindTmuxLsExec(t, "no server running on /tmp/tmux-1000/default", 1)
			},
			wantDetail: "0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := testOptionsWithHome(t, home, false)
			opts.Exec = tc.exec(t)
			report, err := Run(opts)
			require.NoError(t, err)

			rows := findCheckRows(report, "tmux-inspection")
			require.Len(t, rows, 1)
			require.Equal(t, StatusPass, rows[0].Status,
				"a definitive answer is a READ answer, and must not be reported as blindness: %s", rows[0].Detail)
			require.Contains(t, rows[0].Detail, tc.wantDetail)
		})
	}
}

// TestUninvokableTmuxClientIsBlindness pins the case that reads most like a
// determinate empty and is not one. `exec.ErrNotFound` proves doctor could not
// invoke the tmux CLIENT — not that no server holds sessions. doctor's PATH is
// not the sessions' PATH: a scan from a cron job or systemd unit with a minimal
// PATH, or one straddling a package upgrade, finds no tmux while the server is
// up. Reading that as "no sessions are live" is the #2874 defect wearing the
// costume of an optimisation.
func TestUninvokableTmuxClientIsBlindness(t *testing.T) {
	home := testguard.SocketTempDir(t)
	opts := testOptionsWithHome(t, home, true) // Fix: true
	opts.Exec = cmd_test.MockCmdExec{
		RunFunc:    func(*exec.Cmd) error { return nil },
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return nil, &exec.Error{Name: "tmux", Err: exec.ErrNotFound} },
	}

	report, err := Run(opts)
	require.NoError(t, err)

	rows := findCheckRows(report, "tmux-inspection")
	require.Len(t, rows, 1)
	require.Equal(t, StatusFail, rows[0].Status,
		"a tmux client doctor could not execute is a session set it could not read, not an empty one")
	require.Contains(t, rows[0].Detail, "executable file not found",
		"the row must name why doctor could not look")
	for _, f := range report.Findings {
		require.Empty(t, f.FixAction,
			"no fix may be armed when the tmux client could not be invoked: %s / %s", f.Check, f.Detail)
	}
}

// TestStaleTempHomeFixRelistsTmux is the TOCTOU half of the recheck, and the
// regression lock for a freshness bug the memo introduced: findings are applied
// AFTER detection, so the session that must veto an rm -rf is exactly the one
// that STARTED in that window — and it cannot appear in a listing taken before
// the window opened. Detection here sees no sessions; by fix time one claims the
// home.
func TestStaleTempHomeFixRelistsTmux(t *testing.T) {
	tempRoot := t.TempDir()
	dir := makeOldTempAFHome(t, tempRoot, "tmp.claimed-after-detection")
	stubTempHomeLockProbe(t, func(string) daemon.ProbeAnswer { return daemon.AnswerNo() })

	const late = tmux.TmuxPrefix + "started-after-detection"
	detected := false
	opts := macLikeTempHomeOptions(t, tempRoot, true) // Fix: true
	opts.Exec = cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return nil },
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if len(c.Args) > 1 && c.Args[1] == "ls" {
				if !detected {
					detected = true
					return []byte(""), nil // detection: nothing claims the home
				}
				return []byte(late + "\n"), nil // fix time: a session has appeared
			}
			if len(c.Args) > 1 && c.Args[1] == "show-environment" {
				return []byte(tmux.EnvMarkerHome + "=" + dir + "\n"), nil
			}
			return []byte(""), nil
		},
	}

	report, err := Run(opts)
	require.NoError(t, err)

	require.DirExists(t, dir,
		"the fix-time recheck reused the run's memoized session list, so a session that started after "+
			"detection was invisible and `--fix` removed a home a live session claims")
	findings := findByCheck(report, "stale-temp-home")
	require.Len(t, findings, 1)
	require.False(t, findings[0].Fixed)
	require.Error(t, findings[0].FixErr, "the refusal must surface as a fix error, not a silent skip")
	require.Contains(t, findings[0].FixErr.Error(), "live tmux session")
}

// TestUnreadableTmuxSessionListRefusesTempHomeRemoval covers the second
// destructive consumer. A temp home with live tmux sessions but a dead daemon
// holds no lock, so "no live tmux session names this home" is the ONLY thing
// standing between it and an rm -rf. Derived from an unreadable list, that is
// not a fact — so neither detection nor the fix-time recheck may accept it.
func TestUnreadableTmuxSessionListRefusesTempHomeRemoval(t *testing.T) {
	tempRoot := t.TempDir()
	dir := makeOldTempAFHome(t, tempRoot, "tmp.blind-listing")
	stubTempHomeLockProbe(t, func(string) daemon.ProbeAnswer { return daemon.AnswerNo() })

	opts := macLikeTempHomeOptions(t, tempRoot, true) // Fix: true
	opts.Exec = blindTmuxLsExec(t, "error connecting to /tmp/tmux-1000/default (Permission denied)", 1)

	report, err := Run(opts)
	require.NoError(t, err)

	require.DirExists(t, dir,
		"`af doctor --fix` removed a temp home while unable to list tmux sessions — the tmux claim it "+
			"relies on was 'absent' only because it could not be read (#2874)")
	for _, f := range findByCheck(report, "stale-temp-home") {
		require.Empty(t, f.FixAction,
			"a home whose tmux claim could not be checked must not be offered for removal: %s", f.Detail)
		require.False(t, f.Fixed)
	}
}

// TestDefinitiveNoTmuxServerStillAllowsTempHomeRemoval is the anti-regression
// direction for the removal path, and it is not redundant with the fixtures that
// model an empty SUCCESSFUL listing: this drives tmux's exit-1 no-server
// DIAGNOSTIC, the shape a real box with no tmux server returns. Refusing here
// would leave abandoned temp homes uncollectable forever on exactly the machines
// where they accumulate.
//
// Idea taken from #2906, a sibling attempt at this fix that is otherwise
// superseded by #2877.
func TestDefinitiveNoTmuxServerStillAllowsTempHomeRemoval(t *testing.T) {
	tempRoot := t.TempDir()
	dir := makeOldTempAFHome(t, tempRoot, "tmp.no-server-at-all")
	stubTempHomeLockProbe(t, func(string) daemon.ProbeAnswer { return daemon.AnswerNo() })

	opts := macLikeTempHomeOptions(t, tempRoot, true) // Fix: true
	opts.Exec = blindTmuxLsExec(t, "no server running on /tmp/tmux-1000/default", 1)

	report, err := Run(opts)
	require.NoError(t, err)

	rows := findCheckRows(report, "tmux-inspection")
	require.Len(t, rows, 1)
	require.Equal(t, StatusPass, rows[0].Status,
		"tmux answered — there is no server, so this is a real empty session set, not blindness")
	findings := findByCheck(report, "stale-temp-home")
	require.Len(t, findings, 1)
	require.True(t, findings[0].Fixed, "fix outcome: %v", findings[0].FixErr)
	require.NoDirExists(t, dir,
		"a provably-unused home must still be removable on a box with no tmux server, or the guard "+
			"has traded one bug for an uncollectable temp dir")
}

// TestUnreadableSessionRecordsDoNotMakeSessionsLookLeaked is the third read in
// this family. checkLeakedTmuxSessions calls a session leaked when it is not in
// the stored records — so a record store it could not read makes EVERY live
// session look leaked, and each row hands the operator a `tmux kill-session`
// command for a session that is perfectly healthy. No --fix closure is needed
// for that to destroy work; the printed command is the weapon.
func TestUnreadableSessionRecordsDoNotMakeSessionsLookLeaked(t *testing.T) {
	home := testguard.SocketTempDir(t)
	// A records path that cannot be enumerated: ReadDir fails with ENOTDIR for
	// every user, root included, so this does not depend on file modes.
	require.NoError(t, os.WriteFile(filepath.Join(home, "instances"), []byte(""), 0o600))

	opts := testOptionsWithHome(t, home, true)
	opts.Exec = cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return nil },
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if len(c.Args) > 1 && c.Args[1] == "ls" {
				return []byte(tmux.TmuxPrefix + "recorded-and-healthy\n"), nil
			}
			return []byte(""), nil
		},
	}

	report, err := Run(opts)
	require.NoError(t, err)

	require.Empty(t, findByCheck(report, "leaked-tmux-session"),
		"a session was called leaked because the record store could not be read — the row tells the "+
			"operator to kill a live session (#2874)")
	rows := findCheckRows(report, "session-records")
	require.Len(t, rows, 1, "unreadable session records must be reported, not silently treated as none")
	require.Equal(t, StatusFail, rows[0].Status)
}

// TestDoctorSharesOneSessionListing guards the memo: the session list feeds four
// checks, and re-shelling out per check would both cost four tmux round trips and
// let one DETECTION pass see two different worlds.
//
// Scoped to a report-only run on purpose, and read the scope before "fixing" a
// count that exceeds one. A `--fix` run lists again, deliberately:
// staleTempHomeRemoveFix must see sessions that started AFTER detection, so
// tightening this assertion to cover the fix path would mean deleting exactly
// the re-list that keeps an rm -rf off a live home
// (TestStaleTempHomeFixRelistsTmux is the one that would catch it).
func TestDoctorSharesOneSessionListing(t *testing.T) {
	home := testguard.SocketTempDir(t)
	calls := 0
	opts := testOptionsWithHome(t, home, false)
	opts.Exec = cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return nil },
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if len(c.Args) > 1 && c.Args[1] == "ls" {
				calls++
				return []byte("af_only\n"), nil
			}
			return []byte(""), nil
		},
	}
	opts.snapshot = func() (map[int]proctree.Process, error) {
		return map[int]proctree.Process{1: {PID: 1, PPID: 0, Comm: "init"}}, nil
	}
	opts.MinTempHomeAge = time.Hour

	_, err := Run(opts)
	require.NoError(t, err)
	require.Equal(t, 1, calls,
		"the run must list tmux sessions exactly once: repeated listings can disagree, and a check that "+
			"disagrees with the row reporting the listing is worse than either alone")
}
