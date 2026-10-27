package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
)

// This file owns the stale-temp-home check: finding agent-factory homes
// abandoned under the temp dir, deciding which of them are provably unused, and
// — with --fix — removing those.
//
// Split out of checks.go when the #3466 work pushed that file past the
// 1000-line limit. The cut is along a real seam rather than a convenient line
// number: everything here reads the temp dir and nothing else does, and the
// helpers it needs (newestMtime, timeSince, isAFHome) have no other
// callers in the package. The bounded sweep machinery that enumerates the temp
// dir lives in temp_home_sweep.go; this file is the check and report layer over
// it.
//
// Two properties this file exists to hold on to, both learned the hard way:
//
//   - Removal rests on a POSITIVE proof — the home's daemon lock was takeable,
//     which the kernel guarantees means no live daemon owns it — never on
//     having failed to find a user (#1989). Under-cleaning is cosmetic;
//     over-cleaning deletes someone's work.
//   - A sweep that did not finish must SAY SO. Bounded work is worth having,
//     but a shorter list of findings that reads exactly like a healthier
//     machine is worse than a slow command (#3466).

// afHomeMarkers are the files/dirs whose presence identifies a directory as
// an agent-factory home. Two or more must match before doctor will even
// report a directory, let alone remove it.
var afHomeMarkers = []string{
	config.TomlConfigFileName, config.ConfigFileName, "state.json", "instances", "daemon.sock", "daemon.pid", "agent-factory.log",
}

// checkStaleTempHomes finds abandoned agent-factory homes under the temp
// dir (leaked by tests/debug runs — the #1093 immortal-daemon fuel). A home
// is stale only when nothing references it: no live process has it as
// AGENT_FACTORY_HOME, no live tmux session marks it as AF_HOME, its
// daemon.pid (if any) is verified absent/dead/stale rather than merely
// unreadable, and it has not been touched for MinTempHomeAge.
func checkStaleTempHomes(ctx *scanContext, report *Report) {
	// normalizeHome (filepath.EvalSymlinks) rather than filepath.Clean: a
	// symlinked AGENT_FACTORY_HOME whose target is a differently-named sibling
	// under the temp dir must compare equal to the sweep's real-target
	// candidate, or the active-home guard misses and --fix deletes it. The
	// sibling checkLeakedDaemonBinaries already canonicalises this way; the
	// temp dir is resolved too so macOS /tmp -> /private/tmp does not split the
	// candidate from the home it is inside.
	tempDir := normalizeHome(ctx.opts.TempDir)
	activeHome := normalizeHome(ctx.opts.ConfigDir)
	processHomes := processReferencedHomes(ctx.snap)
	// An unavailable tmux claim set is not an empty one. A temp home with live
	// tmux sessions and a dead daemon holds no lock, so this set is the only
	// thing that can spare it — derived from a read that failed, it would spare
	// nothing and the rm -rf would go ahead (#2874). Detection stops here; the
	// blindness is already a FAIL row from checkTmuxInspection (or, for an
	// unreadable ownership marker, reported below).
	tmuxHomes, tmuxHomesErr := liveTmuxHomes(ctx)
	if tmuxHomesErr != nil {
		// Advisory, not FAIL: an unreadable session LIST is already a FAIL row
		// from checkTmuxInspection, and the narrower case left here — one live
		// session that would not answer, often one that vanished mid-scan — is
		// an ordinary unknown, not a broken machine. Either way nothing is
		// assessed and nothing is removed, which is the part that matters.
		// temp-home-scan, NOT stale-temp-home: this is one statement about the
		// SCAN, and stale-temp-home findings are per-home and collapse into a
		// counted row. Filed under that slug the collapse rewrote this advisory
		// as "1 agent-factory home abandoned under the temp dir" — inventing a
		// home that was never found and deleting the reason the scan did no
		// work. A fabricated positive on the report that gates an rm -rf.
		report.markIncomplete("stale-temp-home")
		report.addAdvisoryFinding(Finding{
			Check: "temp-home-scan",
			Detail: fmt.Sprintf("no temp home could be assessed: cannot establish which AF homes live tmux "+
				"sessions claim (%v), and a home a live session claims must never be removed", tmuxHomesErr),
			Severity:    StatusWarn,
			Remediation: "re-run `af doctor` once every live session answers, or inspect the temp dir yourself",
		})
		return
	}

	sweep := ctx.tempHomeCandidates()
	// Reported BEFORE the per-home findings, deliberately. This row is the
	// caveat on every count below it, and a caveat printed after the thing it
	// qualifies is a caveat most readers never reach.
	reportTempHomeSweepTruncation(report, tempDir, sweep, ctx.opts.MaxTempHomeCandidates)

	for _, dir := range sweep.candidates {
		dir = normalizeHome(dir)
		if dir == activeHome || !isAFHome(dir) {
			continue
		}
		// POSITIVE proofs of use come first. Each can only ADD a reason to keep
		// the home; none authorises a delete, so an unread or unavailable surface
		// here costs at worst an unreported keep — never a wrong removal.
		if processHomes[dir] {
			report.Pass(sectionProcesses, "temp home", fmt.Sprintf("%s is in use (a live process references it)", dir))
			continue
		}
		if tmuxHomes[dir] {
			report.Pass(sectionProcesses, "temp home", fmt.Sprintf("%s is in use (a live tmux session references it)", dir))
			continue
		}
		// The AUTHORITATIVE signal: does a live daemon hold the home's lock? This
		// replaces the old "did I see a process referencing this home?" — a
		// negative four consecutive P1 reviews each found a fresh way to falsify,
		// every one ending at an rm -rf. The lock cannot be falsified that way:
		// the kernel releases it on the daemon's death, so a takeable lock is
		// PROOF (not inference) that no live daemon owns the home (#1989).
		lock := tempHomeLockProbe(dir)
		var daemonHoldsLock, provablyFree bool
		var lockCause error
		lock.Match(
			func() { daemonHoldsLock = true }, // Yes: a live daemon owns it
			func() { provablyFree = true },    // No: we took the lock; no daemon owns it
			func() {},                         // NotFound: ProbeHomeLock never returns this; treat as unknown
			func(cause error) { lockCause = cause },
		)
		if daemonHoldsLock {
			report.Pass(sectionProcesses, "temp home", fmt.Sprintf("%s is in use (an af daemon holds its lock)", dir))
			continue
		}

		age := timeSince(newestMtime(dir))
		if age < ctx.opts.MinTempHomeAge {
			continue
		}
		if !pathutil.IsStrictlyInside(dir, tempDir) {
			continue
		}

		if provablyFree {
			// The teeth, restored on a FACT. The lock is takeable (no live daemon)
			// and no live tmux session names it, so the home is provably unused.
			// The fix re-verifies every precondition at fix time (TOCTOU): a
			// daemon may have started, or a tmux session appeared, since detection.
			report.addActionableFinding(Finding{
				Check: "stale-temp-home",
				Detail: fmt.Sprintf("agent-factory home %s is abandoned (untouched for %s) and no live daemon "+
					"holds its lock, so it is safe to remove", dir, formatAge(age.Seconds())),
				FixAction:   "remove " + dir,
				fix:         staleTempHomeRemoveFix(ctx, dir, tempDir, activeHome),
				Severity:    StatusWarn,
				Remediation: "run `af doctor --fix` to remove it, or `" + shellsuggest.Command("rm", "-rf", dir) + "`",
			})
			continue
		}

		// UNKNOWN: no lock file at all (absence of a lock is not proof of
		// non-use), a filesystem whose flock cannot be trusted (NFS), or an I/O
		// error. Report it — reporting is safe — but never authorise the delete
		// on a proof we do not have. The operator decides.
		report.addAdvisoryFinding(Finding{
			Check: "stale-temp-home",
			Detail: fmt.Sprintf("agent-factory home %s looks abandoned (untouched for %s), but nothing here can "+
				"PROVE it is unused (%s) — inspect it and remove it yourself if it is dead",
				dir, formatAge(age.Seconds()), lockUnknownReason(lockCause)),
			Severity:    StatusWarn,
			Remediation: "verify nothing is using it, then `" + shellsuggest.Command("rm", "-rf", dir) + "`",
		})
	}
}

// tempHomeCandidates returns the run's ONE listing of the temp dir, computing it
// at most once.
//
// Memoized for the reason ctx.tempHomeCandidates' siblings are (daemonProcs, the
// tmux listing): two checks now read this sweep — the abandoned-home one and the
// dead-socket one (#3845) — and a temp dir with 9,892 entries is not a directory
// to list twice. More than cost: two listings are two different worlds, and a
// run whose two temp-dir rows disagree about what is there is worse than either
// answer alone.
func (c *scanContext) tempHomeCandidates() tempHomeSweep {
	if !c.tempSweepDone {
		c.tempSweep = candidateTempHomes(filepath.Clean(c.opts.TempDir), c.opts.MaxTempHomeCandidates)
		c.tempSweepDone = true
	}
	return c.tempSweep
}

// reportTempHomeSweepTruncation says out loud that the temp-home sweep did not
// see the whole temp dir.
//
// This is the half of #3466 that actually misleads. A bounded sweep finds fewer
// stale homes than an unbounded one, and without this row the two runs differ
// only in a number nobody can calibrate — the shorter list reads as the
// healthier box. The issue was filed by someone who misread exactly that while
// looking straight at it.
//
// It carries its OWN check slug rather than "stale-temp-home", which matters
// more than it looks: stale-temp-home findings collapse into a single summary
// row, so filing this under that slug would fold the warning that the scan was
// incomplete into the very count it is warning about, and it would vanish.
//
// Advisory, not actionable. "I did not finish looking" is an unknown, and this
// package never lets an unknown assert that a machine is unhealthy — see the
// Finding.Actionable contract. It reaches the reader through the summary line
// and summary.incomplete, which independently makes the command exit 1.
func reportTempHomeSweepTruncation(report *Report, tempDir string, sweep tempHomeSweep, budget int) {
	if !sweep.truncated() {
		return
	}
	report.markIncomplete("stale-temp-home")
	if sweep.unreadable {
		report.addAdvisoryFinding(Finding{
			Check: "temp-home-scan",
			Detail: fmt.Sprintf("no temp home was assessed: %s could not be listed at all, so this run has "+
				"nothing to say about abandoned homes — which is not the same as saying there are none", tempDir),
			Severity:    StatusWarn,
			Remediation: "check that " + tempDir + " exists and is readable, then re-run `af doctor`",
		})
		return
	}
	report.addAdvisoryFinding(Finding{
		Check:       "temp-home-scan",
		Detail:      tempHomeSweepTruncationDetail(tempDir, sweep, budget),
		Severity:    StatusWarn,
		Remediation: tempHomeSweepTruncationRemediation(tempDir, sweep),
	})
}

// tempHomeSweepTruncationRemediation says what to DO about an incomplete sweep,
// which depends on why it was incomplete. Telling someone whose filesystem is
// returning I/O errors to clear out their temp directory is advice that cannot
// work, aimed at a cause that is not theirs.
func tempHomeSweepTruncationRemediation(tempDir string, sweep tempHomeSweep) string {
	if sweep.rootErr != nil || sweep.failedErr != nil {
		return "investigate the listing failure under " + tempDir + ", then re-run `af doctor`; the temp-home " +
			"rows below are a lower bound until it can be read to the end"
	}
	return "clear out " + tempDir + " so the sweep can finish, and treat the temp-home rows below as a " +
		"lower bound until it does"
}

// tempHomeSweepTruncationDetail renders what the sweep did and did not get to.
//
// It reports two DIFFERENT quantities rather than one, because collapsing them
// misdescribes the very case this notice exists for. visited counts first-level
// directories expanded in full; when the budget runs out partway through a
// single huge directory, that is 0 — so a message phrased only in those terms
// says the run "inspected 0 of the 1 directories" immediately after inspecting
// fifty thousand paths inside it. Both numbers are true and neither is the
// whole story, so both are stated.
func tempHomeSweepTruncationDetail(tempDir string, sweep tempHomeSweep, budget int) string {
	// "at least" when the root's own listing was cut short: we do not know how
	// many first-level directories exist, only how many we got as far as
	// naming. Printing that count as a total would be a fabricated denominator
	// — the same error as a fabricated finding, one level up.
	total := fmt.Sprintf("the %d directories", sweep.offered)
	switch {
	case sweep.rootPartial && sweep.rootErr != nil:
		total = fmt.Sprintf("at least %d directories (listing %s failed partway through: %v)",
			sweep.offered, tempDir, sweep.rootErr)
	case sweep.rootPartial:
		total = fmt.Sprintf("at least %d directories (%s is too large to list in full)",
			sweep.offered, tempDir)
	}
	detail := fmt.Sprintf("this run did NOT assess every temp home: it finished %d of %s under "+
		"%s, having inspected %s", sweep.visited, total, tempDir,
		plural(len(sweep.candidates), "candidate directory", "candidate directories"))
	// Name the bound that ACTUALLY fired. They stop the sweep for different
	// reasons and a reader sent to the wrong number is worse off than one sent
	// to none: a first-level directory holding 500,000 plain files exhausts the
	// read budget while recording a single candidate, and "against a
	// 50000-candidate budget" points at a limit that never came near firing.
	var stops []string
	if sweep.hitCandidateLimit {
		stops = append(stops, fmt.Sprintf("a %d-candidate budget", budget))
	}
	if sweep.hitReadBudget {
		stops = append(stops, fmt.Sprintf("a %d-entry read budget (it read %d)",
			tempHomeReadBudget, sweep.entriesRead))
	}
	if len(stops) > 0 {
		detail += " against " + strings.Join(stops, " and ")
	}
	// Split by cause. "Could not be listed at all" is false for a directory
	// that failed partway, and a permission-denied directory is not a fault
	// worth naming an error for — it is simply not ours to read.
	if sweep.denied > 0 {
		detail += fmt.Sprintf("; %s could not be read (they belong to another user)",
			plural(sweep.denied, "directory", "directories"))
	}
	if sweep.failed > 0 {
		detail += fmt.Sprintf("; listing %s failed (%v)",
			plural(sweep.failed, "directory", "directories"), sweep.failedErr)
	}
	if sweep.unlistable > 0 {
		how := "so anything nested inside them was never seen"
		if sweep.listedPartly > 0 {
			how = "so some of what is nested inside them was never seen"
		}
		detail += ", " + how
	}
	return detail + ". Any stale home it did not reach is missing from the counts below"
}

// lockUnknownReason renders the cause of an undetermined lock probe for the
// report, defaulting to a plain phrase when the probe carried none.
func lockUnknownReason(cause error) string {
	if cause == nil {
		return "no lock to take proves nothing"
	}
	return cause.Error()
}

// timeSince is time.Since, indirected so tests can pin the clock if needed.
var timeSince = time.Since

func isAFHome(dir string) bool {
	found := 0
	for _, marker := range afHomeMarkers {
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
			found++
		}
	}
	return found >= 2
}

// processReferencedHomes returns the AF homes live processes name in their
// AGENT_FACTORY_HOME environment. It is a POSITIVE signal only: finding a
// process that names a home proves the home is in use, which spares it. The
// converse — "no process names it" — is NOT proof of non-use and is deliberately
// not derived here: that inference was the unsound predicate behind the
// temp-home rm -rf, and the delete now rests on the daemon lock instead (#1989,
// staleTempHomeRemoveFix). So an unreadable environment simply contributes no
// home, which can only under-spare — never authorise a removal.
//
// Only EnvFound attributes a home. A redacted or denied read (EnvUnknown) must
// add nothing to the set in either direction — "I could not read its home" is
// not "its home is X", and it is not "it names no home" either.
func processReferencedHomes(snap map[int]proctree.Process) map[string]bool {
	homes := map[string]bool{}
	for pid := range snap {
		if home, status := proctree.LookupEnv(pid, "AGENT_FACTORY_HOME"); status == proctree.EnvFound && home != "" {
			homes[normalizeHome(home)] = true
		}
	}
	return homes
}

// staleTempHomeRemoveFix rm -rf's an abandoned temp home — the teeth #1969 took
// out, restored on a predicate the kernel guarantees rather than an inference we
// assemble (#1989).
//
// The old closure was careful — containment, active-home and isAFHome checks, a
// fresh snapshot at fix time — and none of that was the problem. Its PREDICATE
// was: "did I see any process referencing this home? no → delete." Four
// consecutive P1 reviews each found a fresh way for that "no" to be false, every
// one ending here in an rm -rf. You cannot make an unsound question safe by
// validating its inputs harder.
//
// So the gate is now a FACT: re-probe the home's daemon lock, and delete only on
// AnswerNo — the lock existed on a trusted filesystem and we took it, proving no
// live daemon owns the home. Everything is re-checked at fix time because the
// findings are applied after detection, leaving a window in which a daemon could
// start (its lock would then be held → refuse) or a tmux session could claim the
// home (refuse). Undetermined (no lock file, untrusted filesystem) never reaches
// here: only a provably-free home carries this closure.
func staleTempHomeRemoveFix(ctx *scanContext, dir, tempDir, activeHome string) func() error {
	return func() error {
		dir = filepath.Clean(dir)
		// Re-assert containment and identity at fix time — cheap, and the home
		// may have changed under us since detection.
		if dir == activeHome {
			return fmt.Errorf("refusing to remove the active home %s", dir)
		}
		if !pathutil.IsStrictlyInside(dir, tempDir) {
			return fmt.Errorf("refusing to remove %s: it is not inside the temp dir %s", dir, tempDir)
		}
		if !isAFHome(dir) {
			return fmt.Errorf("refusing to remove %s: it no longer looks like an agent-factory home", dir)
		}
		// A live tmux session naming the home is the second, sound signal the
		// lock cannot see (a home with live tmux sessions but a dead daemon holds
		// no lock). Re-check it fresh — and refuse if the recheck cannot be
		// PERFORMED, because a guard that cannot run has not passed.
		//
		// liveTmuxHomesNow, not liveTmuxHomes: the run's memoized listing was
		// taken before this window opened, so a session started since detection
		// is absent from it and would be missed by exactly the guard meant to
		// catch it.
		claimed, err := liveTmuxHomesNow(ctx)
		if err != nil {
			return fmt.Errorf("refusing to remove %s: cannot check whether a live tmux session references it: %w", dir, err)
		}
		if claimed[dir] {
			return fmt.Errorf("refusing to remove %s: a live tmux session now references it", dir)
		}
		// The authoritative gate: delete ONLY on a proven "no live daemon owns
		// this" (AnswerNo). A daemon that appeared since detection now holds the
		// lock (Yes → refuse); an unprovable answer (Undetermined → refuse) is not
		// a licence to os.RemoveAll.
		answer := tempHomeLockProbe(dir)
		proven := false
		answer.Match(func() {}, func() { proven = true }, func() {}, func(error) {})
		if !proven {
			return fmt.Errorf("refusing to remove %s: cannot prove no daemon owns it (lock answer: %s)", dir, answer.String())
		}
		return os.RemoveAll(dir)
	}
}

// newestMtime returns the most recent mtime among the dir itself and its
// marker files — a fair "last touched" signal without a full tree walk.
func newestMtime(dir string) time.Time {
	newest := time.Time{}
	consider := func(path string) {
		if info, err := os.Stat(path); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	consider(dir)
	for _, marker := range afHomeMarkers {
		consider(filepath.Join(dir, marker))
	}
	return newest
}
