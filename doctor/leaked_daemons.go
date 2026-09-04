package doctor

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
)

// This file owns the two probes for the debris a test or debug run leaves
// behind, item 4 of #3842 and section 2 of #3845:
//
//   - af daemons still running from a binary that is not an install — one that
//     lives under the temp dir, or one whose file is gone from disk;
//   - directories under the temp dir holding nothing but a dead
//     daemon-http.sock.
//
// Neither was visible anywhere. checkOrphanedProcesses counts descendants of
// dead tmux sessions, which these are not; checkForeignDaemons only escalates a
// daemon whose home is MISSING, and the leaked daemons kept their homes alive by
// re-creating them (the bug #3845's first half fixes). daemon.StopOrphanDaemons
// cannot see them either, and on purpose: isTestBinaryArgs classifies any argv
// starting /tmp/Test… as a test binary and skips it, so `af reset` walked past
// three daemons that had been running for eleven days.
//
// The census at the time of #3842 was 9,892 /tmp/af-* directories on the
// maintainer's box. The point of these rows is to make that number visible, and
// shrinkable without a hand-written rm.
//
// Both file under "Session & Process Health" rather than the daemon section,
// beside orphaned-processes and stale-temp-homes. A leaked daemon is process
// debris found by the leak sweep, not a statement about the daemon serving this
// home — and collapsedProcessRow renders every collapsed row in that section, so
// filing them anywhere else would put the summary row and the --verbose rows in
// two different places.

const (
	// leakedDaemonMinAge is how old a daemon running from a temp-dir binary must
	// be before --fix will offer to kill it.
	//
	// The window exists for exactly one process: a `go test` run that is still
	// going. Those daemons look identical to the leaked ones — same temp path,
	// same argv — and killing one mid-suite turns this check into a flaky-test
	// generator. An hour is comfortably past any test invocation in this repo
	// (CI's own per-package budget is 20 minutes) and negligible beside the
	// eleven-day leaks it exists to reap. A younger one is still REPORTED; it is
	// only the kill that waits.
	leakedDaemonMinAge = time.Hour

	// deadSocketDialTimeout bounds the probe of one abandoned socket. A dial to a
	// unix socket with no listener fails immediately (ECONNREFUSED); the timeout
	// is for a path that hangs, which is an UNKNOWN and must not stall a sweep
	// over thousands of directories.
	deadSocketDialTimeout = 250 * time.Millisecond

	// deletedExeSuffix is what Linux appends to /proc/<pid>/exe when the binary's
	// inode has been unlinked.
	deletedExeSuffix = " (deleted)"

	// The two check slugs, named once so the collapse list in report.go and the
	// detectors here cannot drift apart.
	checkLeakedDaemon   = "leaked-daemon"
	checkDeadSocketHome = "dead-socket-home"
)

// daemonBinary is what /proc/<pid>/exe says about a daemon's own binary.
//
// known is the honest-unknown channel, not a formality: an unreadable exe link
// is the ordinary state for a process we do not own, and treating it as "no
// binary" would put every foreign daemon into the leaked bucket.
type daemonBinary struct {
	path    string
	deleted bool
	known   bool
}

// The two probes' injection points, package vars for the same reason the rest of
// this package's are: a test cannot spawn a process from a deleted binary, and
// it must never depend on what is running on the machine running it.
var (
	daemonProcessExe = readDaemonProcessExe
	deadSocketProbe  = probeDeadSocket
	// exePathsReadable reports whether this platform exposes a process's own
	// binary path at all. /proc/<pid>/exe is Linux's; darwin has no equivalent
	// proctree exposes, so without this every daemon on a Mac would earn an
	// "unreadable binary path" row on every run — a per-process advisory for a
	// platform fact, which is noise, not a finding.
	exePathsReadable = procExePathsReadable
)

func procExePathsReadable() bool {
	_, err := os.Readlink("/proc/self/exe")
	return err == nil
}

// readDaemonProcessExe resolves pid's running binary through /proc/<pid>/exe.
//
// The " (deleted)" suffix is the kernel's, and it is ambiguous by construction —
// a file genuinely named "af (deleted)" reads the same way. That ambiguity is
// harmless here because deletion alone is never actionable (see
// checkLeakedDaemonBinaries): it is the temp-dir location that arms a kill, and
// the suffix is stripped before that test so a deleted temp binary is still
// recognised as a temp binary.
func readDaemonProcessExe(pid int) daemonBinary {
	target, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil || target == "" {
		return daemonBinary{}
	}
	if strings.HasSuffix(target, deletedExeSuffix) {
		return daemonBinary{path: strings.TrimSuffix(target, deletedExeSuffix), deleted: true, known: true}
	}
	return daemonBinary{path: target, known: true}
}

// checkLeakedDaemonBinaries reports af daemons running from a binary no install
// owns, and offers to kill the ones that provably are not this machine's.
//
// A kill needs THREE facts, and each one is load-bearing:
//
//   - the binary lives under the temp dir. That is the unfalsifiable half: no
//     install puts af in /tmp, and the three daemons #3842 found had run from
//     /tmp/Test*/001/af for eleven days.
//   - the daemon does not serve the ACTIVE home. A temp-built binary serving the
//     user's real home is somebody's dev daemon and is the one answering this
//     very run — checkDaemonHealth and checkDuplicateDaemons own it. It is
//     reported here, never offered for a kill.
//   - it is older than leakedDaemonMinAge, so a `go test` run in progress is
//     reported rather than reaped out from under itself.
//
// A DELETED binary is deliberately NOT actionable on its own, which is where
// this departs from the issue's "a real install's daemon never does either". It
// does: `af upgrade` replaces the binary in place, so every healthy daemon reads
// as deleted until it restarts — the very reason watchDaemonHome refuses to
// check the binary path (#1093). Deletion outside the temp dir is reported as
// the advisory it is, with the upgrade named so the reader can dismiss it in one
// glance.
func checkLeakedDaemonBinaries(ctx *scanContext, report *Report) {
	if ctx.snap == nil {
		// Blind, not healthy. checkProcessInspection already filed the FAIL row
		// for the unreadable process table, and every daemon check below it stays
		// silent rather than reporting a clean bill it never earned (#1939).
		return
	}
	if !exePathsReadable() {
		// One row for the platform, not one per daemon. Advisory, and only when
		// there is actually a daemon to have said something about — a Mac with no
		// af daemon running has nothing to report either way. Mirrors
		// checkDuplicateDaemons' macOS branch: a diagnostic that cannot see must
		// say so rather than PASS, but the user cannot fix the platform.
		if len(ctx.daemonProcs()) > 0 {
			report.Warn(sectionProcesses, "leaked-daemons",
				"cannot read a process's own binary path on this platform, so daemons left over by a "+
					"test or debug run were not checked",
				"on macOS this check is unavailable (#1939); look for `af --daemon` processes running "+
					"from a temp path by hand", false)
		}
		return
	}
	activeHome := normalizeHome(ctx.opts.ConfigDir)
	tempDir := filepath.Clean(ctx.opts.TempDir)

	for _, d := range ctx.daemonProcs() {
		if d.isSelfAncestor || !d.ownedByUs {
			// Our own ancestor daemon is the one running this command, and another
			// user's is not ours to reap — the same two exclusions
			// checkForeignDaemons makes, for the same reasons.
			continue
		}
		bin := daemonProcessExe(d.proc.PID)
		if !bin.known {
			report.addAdvisoryFinding(Finding{
				Check: checkLeakedDaemon,
				Detail: fmt.Sprintf("%s is an af daemon whose own binary path cannot be read, so it is "+
					"impossible to tell an installed daemon from one left over by a test run",
					describeProc(d.proc)),
				Severity:    StatusWarn,
				Remediation: "inspect `readlink /proc/" + fmt.Sprint(d.proc.PID) + "/exe` yourself, then stop it if it is debris",
			})
			continue
		}
		underTemp := pathutil.IsStrictlyInside(filepath.Clean(bin.path), tempDir)
		if !underTemp && !bin.deleted {
			continue // an ordinary install running its own binary
		}
		if !underTemp {
			// Deleted, but from a real install path. Overwhelmingly an upgrade.
			report.addAdvisoryFinding(Finding{
				Check: checkLeakedDaemon,
				Detail: fmt.Sprintf("%s is running a binary that no longer exists at %s — normal right after "+
					"`af upgrade` replaced it, and stale otherwise", describeProc(d.proc), bin.path),
				Severity:    StatusWarn,
				Remediation: "run `af daemon restart` so it picks up the installed binary, if you did not just upgrade",
			})
			continue
		}

		where := bin.path
		if bin.deleted {
			where += " (since deleted)"
		}
		serves := "an agent-factory home this run could not establish"
		if d.homeKnown {
			serves = "agent-factory home " + d.home
		}
		// describeProc already renders the process's age ("… 0% CPU over 11d"),
		// which is the fact the kill window turns on, so it is not repeated here.
		detail := fmt.Sprintf("%s runs af from %s, which is not an install — it serves %s",
			describeProc(d.proc), where, serves)

		// The kill needs a home we positively read AND that is not this run's.
		// An unreadable home leaves open the possibility that this is the daemon
		// serving the active home, and "I could not tell" must never resolve to
		// "kill it" — the rule daemon.StopOrphanDaemons states for the same scan.
		switch {
		case !d.homeKnown:
			report.addAdvisoryFinding(Finding{
				Check:       checkLeakedDaemon,
				Detail:      detail + "; which home it serves is unreadable, so it is reported rather than stopped",
				Severity:    StatusWarn,
				Remediation: "confirm it is not your working daemon, then stop it by pid",
			})
		case d.home == activeHome:
			report.addAdvisoryFinding(Finding{
				Check:       checkLeakedDaemon,
				Detail:      detail + "; it serves THIS home, so it is left alone — it is the daemon answering this run",
				Severity:    StatusWarn,
				Remediation: "install af properly (`./dev-install.sh`) and run `af daemon restart` if this was a throwaway build",
			})
		case !leakedDaemonOldEnough(ctx, d.proc):
			report.addAdvisoryFinding(Finding{
				Check: checkLeakedDaemon,
				Detail: detail + fmt.Sprintf("; younger than %s, so it may belong to a test run that is still going",
					formatAge(ctx.opts.minLeakedDaemonAge.Seconds())),
				Severity:    StatusWarn,
				Remediation: "re-run `af doctor` once the test run has finished",
			})
		default:
			report.addActionableFinding(Finding{
				Check:       checkLeakedDaemon,
				Detail:      detail,
				FixAction:   fmt.Sprintf("kill pid %d", d.proc.PID),
				fix:         killFix(ctx, d.proc),
				Severity:    StatusFail,
				Remediation: "run `af doctor --fix` to stop it, or kill pid " + fmt.Sprint(d.proc.PID),
			})
		}
	}
}

// leakedDaemonOldEnough reports whether the process has been running longer than
// the run's kill window, measured against its fixed snapshot instant so a
// slow doctor run cannot age a candidate across the threshold mid-sweep.
//
// An unreadable age is NOT old enough. The window exists to spare a live test
// run, and a guard that cannot be evaluated has not passed.
func leakedDaemonOldEnough(ctx *scanContext, p proctree.Process) bool {
	if p.StartedAt.IsZero() {
		return false
	}
	return ctx.snapAt.Sub(p.StartedAt) >= ctx.opts.minLeakedDaemonAge
}

// checkDeadSocketHomes finds temp directories holding nothing but an
// unanswered daemon-http.sock, and offers to remove them.
//
// This shape is exactly what the #3842 bug produced: an abandoned daemon's
// socket bind re-created the home directory it had just been deleted from, so
// the directory came back containing the one file the bind had made and nothing
// else. isAFHome needs two markers before it will even look at a directory, and
// daemon-http.sock is not one of them, so every one of these was invisible to
// the stale-temp-home sweep.
//
// Removing one cannot destroy data, and that is the argument rather than an
// inference about who might be using it:
//
//   - the directory's ENTIRE content is one entry, and it is a socket;
//   - a dial of that socket was REFUSED — positive evidence that nothing is
//     serving it, not "we failed to find a user";
//   - the fix re-checks the shape and then calls os.Remove, never os.RemoveAll,
//     so anything that appeared in the meantime makes the removal fail instead
//     of being swept up with it.
//
// The reference checks below (a live process's AGENT_FACTORY_HOME, a live tmux
// session, a live process's working directory) can each only ADD a reason to
// keep the directory. None of them authorises the removal, which is the
// asymmetry #1989 was fought over: under-cleaning is cosmetic, over-cleaning is
// not.
func checkDeadSocketHomes(ctx *scanContext, report *Report) {
	tempDir := filepath.Clean(ctx.opts.TempDir)
	activeHome := filepath.Clean(ctx.opts.ConfigDir)

	// A tmux listing that FAILED is not an empty one, and this set is one of the
	// things that can spare a directory. Derived from a failed read it would
	// spare nothing (#2874), so the whole check stands down and says so.
	tmuxHomes, tmuxHomesErr := liveTmuxHomes(ctx)
	if tmuxHomesErr != nil {
		report.markIncomplete(checkDeadSocketHome)
		report.addAdvisoryFinding(Finding{
			Check: "dead-socket-scan",
			Detail: fmt.Sprintf("no dead-socket directory could be assessed: cannot establish which AF homes "+
				"live tmux sessions claim (%v)", tmuxHomesErr),
			Severity:    StatusWarn,
			Remediation: "re-run `af doctor` once every live session answers",
		})
		return
	}
	processHomes := processReferencedHomes(ctx.snap)

	sweep := ctx.tempHomeCandidates()
	if sweep.truncated() {
		// checkStaleTempHomes already filed the temp-home-scan advisory naming the
		// truncation and its cause; this only records that THIS check's numbers
		// are a lower bound too, so the summary line names it.
		report.markIncomplete(checkDeadSocketHome)
	}

	for _, dir := range sweep.candidates {
		dir = filepath.Clean(dir)
		if dir == activeHome || !pathutil.IsStrictlyInside(dir, tempDir) {
			continue
		}
		if !holdsOnlyADaemonSocket(dir) {
			continue
		}
		socket := filepath.Join(dir, daemon.HTTPSocketName())
		if timeSince(fileMtime(socket)) < ctx.opts.MinTempHomeAge {
			continue
		}

		var dead bool
		var cause error
		deadSocketProbe(socket).Match(
			func() {},                   // Yes: something is accepting — in use
			func() { dead = true },      // No: the dial was refused — nothing serves it
			func() {},                   // NotFound: probeDeadSocket never returns this
			func(c error) { cause = c }, // Undetermined: no answer, no licence
		)
		if !dead {
			if cause != nil {
				report.addAdvisoryFinding(Finding{
					Check: checkDeadSocketHome,
					Detail: fmt.Sprintf("%s holds nothing but a daemon socket, but whether anything is serving "+
						"it could not be determined (%v) — inspect it yourself", dir, cause),
					Severity:    StatusWarn,
					Remediation: "verify nothing is using it, then `" + shellsuggest.Command("rm", "-rf", dir) + "`",
				})
			}
			continue
		}

		if reason, held := deadSocketHomeIsReferenced(dir, processHomes, tmuxHomes, ctx.liveWorkingDirs()); held {
			report.Pass(sectionProcesses, "dead-socket home",
				fmt.Sprintf("%s holds only a dead daemon socket but is in use (%s)", dir, reason))
			continue
		}

		report.addActionableFinding(Finding{
			Check: checkDeadSocketHome,
			Detail: fmt.Sprintf("%s holds nothing but a daemon socket no process answers on — the residue an "+
				"abandoned daemon's bind left behind, and there is no data in it to lose", dir),
			FixAction:   "remove " + dir,
			fix:         deadSocketHomeRemoveFix(ctx, dir, tempDir, activeHome),
			Severity:    StatusWarn,
			Remediation: "run `af doctor --fix` to remove it, or `" + shellsuggest.Command("rm", "-rf", dir) + "`",
		})
	}
}

// holdsOnlyADaemonSocket reports whether dir's ENTIRE content is one entry, and
// that entry is the daemon's HTTP socket.
//
// "Entire" is what makes the removal safe, so a directory that cannot be listed
// is false rather than a guess, and the name alone is never enough — only the
// mode proves the entry is a socket and not a file that borrowed the name, the
// same rule checkStaleSockets follows.
func holdsOnlyADaemonSocket(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		return false
	}
	if entries[0].Name() != daemon.HTTPSocketName() {
		return false
	}
	info, err := os.Lstat(filepath.Join(dir, entries[0].Name()))
	return err == nil && info.Mode()&os.ModeSocket != 0
}

// deadSocketHomeIsReferenced returns the reason a directory is in use, if any
// live thing on the box references it. Every branch is a POSITIVE observation:
// finding one spares the directory, and failing to find one authorises nothing.
//
// It takes the run's precomputed working-directory index rather than calling
// proctree.OccupantsOfDir per directory — that helper takes a fresh process-table
// snapshot on every call, and this check runs over every candidate under the
// temp dir. On the box #3845 was filed from that is 9,894 snapshots.
//
// There is deliberately no scan of /proc/<pid>/fd here, and it is worth saying
// why rather than leaving it as an omission. A directory in this shape holds one
// unix socket; a process that has that socket OPEN — the listener or a connected
// client — shows it in /proc as `socket:[inode]`, not as a path, so an fd walk
// cannot see the case it would be there to catch. The dial answers that question
// directly and positively, and the removal's os.Remove (never os.RemoveAll) is
// what covers anything that appears afterwards.
func deadSocketHomeIsReferenced(dir string, processHomes, tmuxHomes map[string]bool, cwds map[int]string) (string, bool) {
	if processHomes[dir] {
		return "a live process names it as AGENT_FACTORY_HOME", true
	}
	if tmuxHomes[dir] {
		return "a live tmux session references it", true
	}
	if pid, ok := processWorkingInside(dir, cwds); ok {
		return fmt.Sprintf("pid %d is working inside it", pid), true
	}
	return "", false
}

// processWorkingInside reports the first pid whose working directory is at or
// inside dir. Pids in ascending order so the reported one is stable across runs.
func processWorkingInside(dir string, cwds map[int]string) (int, bool) {
	pids := make([]int, 0, len(cwds))
	for pid := range cwds {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	for _, pid := range pids {
		if pathutil.IsAtOrInside(cwds[pid], dir) {
			return pid, true
		}
	}
	return 0, false
}

// liveWorkingDirs indexes the working directory of every process in the run's
// snapshot whose cwd could be read, computed at most once.
//
// A pid whose cwd is unreadable contributes nothing, which can only omit a
// reason to KEEP a directory — never invent one to remove it. That direction is
// tolerable here, and only here, because this signal does not authorise the
// removal: what authorises it is that the directory's entire content is one
// socket nothing answers on, so there is no data for a missed occupant to lose,
// and os.Remove refuses the moment anything else appears. The abandoned-HOME
// sweep next door can delete a session's whole state, which is why IT rests on
// the kernel's lock answer instead (#1989).
func (c *scanContext) liveWorkingDirs() map[int]string {
	if c.cwdsScanned {
		return c.cwds
	}
	c.cwdsScanned = true
	c.cwds = map[int]string{}
	for pid := range c.snap {
		if dir, ok := daemonProcessCwd(pid); ok && dir != "" {
			c.cwds[pid] = filepath.Clean(dir)
		}
	}
	return c.cwds
}

// deadSocketHomeRemoveFix removes one dead-socket directory, re-establishing
// every precondition first — findings are applied after detection, so a daemon
// could have bound the socket, or a shell could have cd'd in, since.
//
// It removes the socket and then the DIRECTORY, with os.Remove both times. That
// is the guarantee, not a stylistic preference: os.Remove on a non-empty
// directory fails, so if anything at all appeared in the window the removal
// reports an error instead of taking it with it. There is no rm -rf on this
// path, which is why it needs no lock proof of the kind an abandoned temp HOME
// does (staleTempHomeRemoveFix) — that one can delete a session's whole state,
// and this one cannot delete anything but a socket nobody answers on.
func deadSocketHomeRemoveFix(ctx *scanContext, dir, tempDir, activeHome string) func() error {
	return func() error {
		dir = filepath.Clean(dir)
		if dir == activeHome {
			return fmt.Errorf("refusing to remove the active home %s", dir)
		}
		if !pathutil.IsStrictlyInside(dir, tempDir) {
			return fmt.Errorf("refusing to remove %s: it is not inside the temp dir %s", dir, tempDir)
		}
		if !holdsOnlyADaemonSocket(dir) {
			return fmt.Errorf("refusing to remove %s: it no longer holds only a daemon socket", dir)
		}
		socket := filepath.Join(dir, daemon.HTTPSocketName())
		dead := false
		var cause error
		deadSocketProbe(socket).Match(
			func() {}, func() { dead = true }, func() {}, func(c error) { cause = c })
		if !dead {
			return fmt.Errorf("refusing to remove %s: %s", dir, deadSocketRefusal(cause))
		}
		// Both rechecks read state captured AFTER detection, which is the property
		// they exist for: the session or shell that must veto a removal is
		// precisely the one that appeared in that window, and it cannot be in a
		// listing taken before it opened. A recheck that cannot RUN has not passed.
		claimed, err := ctx.fixTimeTmuxHomes()
		if err != nil {
			return fmt.Errorf("refusing to remove %s: cannot check whether a live tmux session references it: %w", dir, err)
		}
		if claimed[dir] {
			return fmt.Errorf("refusing to remove %s: a live tmux session now references it", dir)
		}
		if pid, inside := processWorkingInside(dir, ctx.fixTimeWorkingDirs()); inside {
			return fmt.Errorf("refusing to remove %s: pid %d is now working inside it", dir, pid)
		}
		if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove the dead socket in %s: %w", dir, err)
		}
		// os.Remove, never os.RemoveAll: a directory that has gained an entry
		// since the shape check fails here rather than being swept away with
		// whatever it gained.
		if err := os.Remove(dir); err != nil {
			return fmt.Errorf("remove %s: %w", dir, err)
		}
		return nil
	}
}

func deadSocketRefusal(cause error) string {
	if cause != nil {
		return fmt.Sprintf("cannot establish that nothing is serving its socket: %v", cause)
	}
	return "something is now accepting connections on its socket"
}

// fixTimeTmuxHomes and fixTimeWorkingDirs are the fix pass's rechecks, taken
// ONCE for the whole pass rather than once per removal.
//
// Once per REMOVAL is what the sibling temp-home fix does, and it is wrong at
// this scale. A tmux recheck costs one listing plus one marker read per af_
// session — about sixteen shell-outs on the maintainer's box — and a working-dir
// recheck walks the whole process table. This check finds 626 removable
// directories there, so per-removal would be ~10,000 shell-outs at the tmux
// server every live agent session depends on, and 626 process-table walks.
//
// Memoizing keeps what the recheck is FOR. Its purpose is not sub-second
// freshness; it is that the observation happens after DETECTION, so a session or
// shell that appeared in that window is visible at all. Run applies every fix
// synchronously in one pass, so the first removal's read is post-detection and
// every later one shares it. What is given up is a session that starts partway
// through the pass — and losing that costs nothing, because the only thing this
// fix can delete is a socket nobody answers on and an otherwise-empty directory.
// The dial, which IS re-run per removal, is the recheck that guards the case
// that matters: a daemon binding that socket back.
func (c *scanContext) fixTimeTmuxHomes() (map[string]bool, error) {
	if !c.fixTmuxScanned {
		c.fixTmuxHomes, c.fixTmuxErr = liveTmuxHomesNow(c)
		c.fixTmuxScanned = true
	}
	return c.fixTmuxHomes, c.fixTmuxErr
}

// fixTimeWorkingDirs indexes live working directories from a process-table read
// taken during the fix pass. An unreadable table yields an empty index, which
// spares nothing — see liveWorkingDirs for why that direction is acceptable on
// this signal alone.
func (c *scanContext) fixTimeWorkingDirs() map[int]string {
	if c.fixCwdsScanned {
		return c.fixCwds
	}
	c.fixCwdsScanned = true
	c.fixCwds = map[int]string{}
	snap, err := proctree.Snapshot()
	if err != nil {
		return c.fixCwds
	}
	for pid := range snap {
		if dir, ok := daemonProcessCwd(pid); ok && dir != "" {
			c.fixCwds[pid] = filepath.Clean(dir)
		}
	}
	return c.fixCwds
}

// probeDeadSocket asks whether anything is accepting on a unix socket.
//
// AnswerNo — the outcome that authorises anything — is reached only from a dial
// that COMPLETED and was refused, or from a socket that is simply gone. A
// timeout, a permission error or any other failure is Undetermined: "the dial
// did not succeed" is not "nothing is there", and this package has a standing
// rule against turning the first into the second (see daemon.ProbeAnswer).
func probeDeadSocket(path string) daemon.ProbeAnswer {
	conn, err := net.DialTimeout("unix", path, deadSocketDialTimeout)
	if err == nil {
		_ = conn.Close()
		return daemon.AnswerYes()
	}
	if errors.Is(err, os.ErrNotExist) {
		return daemon.AnswerNo()
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Timeout() {
		return daemon.Undetermined(err)
	}
	if isConnectionRefused(err) {
		return daemon.AnswerNo()
	}
	return daemon.Undetermined(err)
}

// isConnectionRefused reports whether the dial failed because nothing is
// listening — the one syscall-level outcome that is a positive NO rather than a
// failure to ask.
func isConnectionRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}

// fileMtime returns path's modification time, or the zero time when it cannot be
// read — which timeSince turns into a very large age, so an unreadable socket is
// never spared by the age gate alone. Every other guard still applies.
func fileMtime(path string) time.Time {
	info, err := os.Lstat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}
