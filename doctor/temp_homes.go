package doctor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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
// callers in the package.
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
	tempDir := filepath.Clean(ctx.opts.TempDir)
	activeHome := filepath.Clean(ctx.opts.ConfigDir)
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

	sweep := candidateTempHomes(tempDir, ctx.opts.MaxTempHomeCandidates)
	// Reported BEFORE the per-home findings, deliberately. This row is the
	// caveat on every count below it, and a caveat printed after the thing it
	// qualifies is a caveat most readers never reach.
	reportTempHomeSweepTruncation(report, tempDir, sweep, ctx.opts.MaxTempHomeCandidates)

	for _, dir := range sweep.candidates {
		dir = filepath.Clean(dir)
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
// and summary.incomplete instead of through the exit code.
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
		Check:    "temp-home-scan",
		Detail:   tempHomeSweepTruncationDetail(tempDir, sweep, budget),
		Severity: StatusWarn,
		Remediation: "clear out " + tempDir + " so the sweep can finish, and treat the temp-home rows below as a " +
			"lower bound until it does",
	})
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
	if sweep.rootPartial {
		total = fmt.Sprintf("at least %d directories (%s is too large to list in full)",
			sweep.offered, tempDir)
	}
	detail := fmt.Sprintf("this run did NOT assess every temp home: it finished %d of %s under "+
		"%s, having inspected %s", sweep.visited, total, tempDir,
		plural(len(sweep.candidates), "candidate directory", "candidate directories"))
	if sweep.visited+sweep.unlistable < sweep.offered {
		detail += fmt.Sprintf(" against a %d-candidate budget", budget)
	}
	if sweep.unlistable > 0 {
		detail += fmt.Sprintf("; %s could not be listed at all, so anything nested inside them was never seen",
			plural(sweep.unlistable, "directory", "directories"))
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

// defaultMaxTempHomeCandidates bounds how many candidate directories one
// temp-home sweep will inspect.
//
// The cost of this check is dominated by IDENTIFYING candidates, not by
// assessing them. isAFHome stats up to seven marker paths per candidate, so on
// the box that prompted #3466 a temp dir holding 48,169 candidate directories
// cost ~337,000 stat syscalls to find 53 real homes. None of that work is
// proportional to anything af did — it scales with whatever else has been
// dropped in /tmp — and on a cold dentry cache under load it is what turned
// `af doctor` into a ten-minute command.
//
// Cheaper identification was tried and REJECTED on measurement, not taste.
// Pre-filtering each candidate from its own directory listing (one ReadDir
// instead of seven stats, sound because a stat-able marker must appear in its
// parent's listing) found the identical 53 homes and ran 4x SLOWER: 14.8s
// against 3.6s, because a ReadDir per second-level directory costs far more
// than the seven stats it replaces. The stat path is already the cheap one, so
// the cost is BOUNDED rather than optimized.
//
// The default is a SAFETY VALVE, not routine behavior, and the number is chosen
// against measurement rather than taste. The reporting box offers 48,169
// candidates; 50,000 clears it with headroom, so a machine that works today
// keeps finding exactly what it found before. A tighter bound was tried first
// and rejected on evidence: at 20,000 that same box truncated at 15,194 of its
// 21,762 directories and reported 28 abandoned homes instead of 49 — loudly,
// but it still means a real machine quietly assesses less of itself on every
// run, forever, because nothing about clearing /tmp is on anyone's list.
//
// Bounding growth is the goal; reducing what a working box sees is not. A temp
// dir that blows past this is pathological, and for that case truncating and
// SAYING SO beats a ten-minute command that finishes.
const defaultMaxTempHomeCandidates = 50000

// tempHomeSweep is one enumeration of the temp dir: the candidates to inspect,
// and how much of the temp dir the sweep actually got through.
//
// The second half is not bookkeeping. A sweep that stopped early yields FEWER
// findings than one that finished, and without these counts the two are
// indistinguishable in the output — which is exactly how a truncated run reads
// as the healthier machine (#3466).
type tempHomeSweep struct {
	candidates []string
	// offered and visited count FIRST-LEVEL entries of the temp dir, because
	// that is both the number an operator can act on ("/tmp holds 48,000
	// directories") and a number known EXACTLY: one ReadDir yields it before
	// any bound applies. Counting candidates instead would mean reporting a
	// total the sweep stopped before it could compute.
	offered int
	visited int
	// unreadable records that the temp dir itself could not be listed.
	unreadable bool
	// unlistable counts first-level directories whose OWN listing failed — one
	// owned by another user at mode 0700, say. Their children were never seen,
	// so they are not visited entries; without this the sweep would count them
	// as fully assessed and report a complete run while nested homes went
	// unlooked-at.
	unlistable int
	// entriesRead counts every directory entry the sweep LOOKED AT, across the
	// temp root and every directory it expanded.
	//
	// This is the quantity that actually bounds the work, and it is not the
	// candidate count. A directory holding a million plain files yields no
	// candidates at all, so a candidate budget never stops reading it; and the
	// temp root is read before a single candidate exists to be counted. Both
	// shapes re-created the unbounded read this check is supposed to have
	// stopped.
	entriesRead int
	// rootPartial records that the temp root's own listing was cut short, which
	// makes offered a LOWER BOUND rather than a total — there may be more
	// first-level directories we never saw the names of.
	rootPartial bool
}

// truncated reports whether the sweep gave up before seeing the whole temp dir.
//
// Three ways it can, and all of them count: it never started (the temp dir
// itself would not list), it stopped early (the budget), or it skipped past
// something it could not read (a first-level directory that would not list).
// Only entries expanded IN FULL increment visited, so the last two both surface
// as visited < offered.
func (s tempHomeSweep) truncated() bool {
	return s.unreadable || s.rootPartial || s.visited < s.offered
}

// tempHomeChildBatch is how many directory entries the sweep reads at a time
// while expanding one first-level directory.
//
// Batched, rather than os.ReadDir, because os.ReadDir reads EVERY entry and
// filename-sorts them before it returns. On the million-child directory this
// budget exists for, that is memory and runtime proportional to all million,
// incurred before the budget can stop anything — so the sweep could hang or be
// OOM-killed before it ever emitted the notice saying it had given up. Bounding
// the candidates we RECORD is not enough; the READ has to be bounded too.
//
// The cost is ordering. File.ReadDir does not sort, so when the budget
// truncates a directory, WHICH of its children were reached is filesystem order
// rather than lexical, and two runs on the same box may reach different ones.
// Sorting would mean reading all of them, which is the cost being avoided; a
// truncated sweep is explicitly a partial view either way, and it says so.
var tempHomeChildBatch = 1024

// tempHomeReadBudget bounds how many directory entries ONE sweep will read, across
// the temp root and every directory it expands.
//
// This exists because the candidate budget bounds the wrong quantity for two
// real shapes. A first-level directory holding a million ordinary files and no
// subdirectories produces no candidates, so a candidate limit never fires and
// the sweep reads all million. And the temp ROOT is read before any candidate
// exists, so a root with millions of entries is read in full no matter what the
// candidate limit says. Both re-create the hang-or-OOM this check is meant to
// have stopped, and both do it before any incompleteness notice can be emitted.
//
// 500,000 is against measurement: a full sweep on the box in #3466 reads 238,382
// entries (80,031 at the root plus 158,351 across 21,785 first-level
// directories), so this clears it roughly 2x over and only a genuinely
// pathological temp dir trips it. As everywhere else here, tripping it is not
// silent.
var tempHomeReadBudget = 500000

// scanDirBatch reads the next batch of entries from an open directory.
//
// Indirected so a test can count how much of a directory the sweep actually
// READS. That is the property the bound exists for, and the candidate count
// alone cannot demonstrate it: from the outside, a sweep that reads a million
// entries and records fifty thousand looks identical to one that read fifty
// thousand.
var scanDirBatch = func(f *os.File, n int) ([]os.DirEntry, error) { return f.ReadDir(n) }

// expandTempHomeChildren appends dir's subdirectories to the sweep, stopping at
// the budget. It reports whether the directory was expanded in FULL, and any
// error that ended the listing early.
func expandTempHomeChildren(sweep *tempHomeSweep, dir string, limit int) (expanded bool, err error) {
	f, openErr := os.Open(dir)
	if openErr != nil {
		return false, openErr
	}
	defer func() { _ = f.Close() }()
	for {
		if limit > 0 && len(sweep.candidates) >= limit {
			return false, nil
		}
		if sweep.entriesRead >= tempHomeReadBudget {
			// Charged per ENTRY, not per candidate. Without this a directory of
			// a million files — which contributes no candidates — is read to
			// EOF however small the candidate budget is.
			return false, nil
		}
		batch, readErr := scanDirBatch(f, tempHomeChildBatch)
		sweep.entriesRead += len(batch)
		for _, entry := range batch {
			if !entry.IsDir() {
				continue
			}
			if limit > 0 && len(sweep.candidates) >= limit {
				return false, nil
			}
			sweep.candidates = append(sweep.candidates, filepath.Join(dir, entry.Name()))
		}
		if errors.Is(readErr, io.EOF) {
			return true, nil
		}
		if readErr != nil {
			return false, readErr
		}
	}
}

// readTempRoot streams the temp dir's own listing, returning the names of its
// subdirectories. It reports false only when the directory could not be opened
// at all — a sweep that saw nothing rather than one that found nothing.
//
// Streamed for the same reason child expansion is: os.ReadDir reads and sorts
// EVERY entry before returning, and the root is read before a single candidate
// exists, so no candidate budget can stop it. A temp root with millions of
// entries could therefore hang or OOM the process before it emitted the notice
// saying it had given up.
//
// The names ARE sorted before being returned, which matters more than it looks.
// Any root under the read budget — every non-pathological machine — is read in
// full, so sorting restores exactly the deterministic, reproducible order
// os.ReadDir used to give: the same box scanned twice truncates at the same
// place. Only a root that exceeds the budget loses that, and there the PREFIX
// that was read is filesystem-ordered anyway, so sorting could not have made it
// reproducible.
func readTempRoot(sweep *tempHomeSweep, tempDir string) ([]string, bool) {
	f, err := os.Open(tempDir)
	if err != nil {
		// A temp dir that cannot be opened has told us NOTHING. That is not the
		// same as telling us it holds no homes, and reporting the second is this
		// package's signature failure (#1939, #2874).
		sweep.unreadable = true
		return nil, false
	}
	defer func() { _ = f.Close() }()

	var names []string
	for {
		if sweep.entriesRead >= tempHomeReadBudget {
			sweep.rootPartial = true
			break
		}
		batch, readErr := scanDirBatch(f, tempHomeChildBatch)
		sweep.entriesRead += len(batch)
		for _, entry := range batch {
			if entry.IsDir() {
				names = append(names, entry.Name())
				sweep.offered++
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			// Some of the root was read. That is a partial listing, not an
			// absent one: offered becomes a lower bound and the run is reported
			// incomplete rather than unreadable.
			sweep.rootPartial = true
			break
		}
	}
	sort.Strings(names)
	return names, true
}

// candidateTempHomes lists directories one and two levels below tempDir —
// Go tests produce /tmp/TestName123/001-style homes, manual runs
// /tmp/tmp.XXXX ones.
//
// It stops once it has produced limit candidates, at a first-level BOUNDARY so
// the counts it returns describe whole entries rather than a partially expanded
// one. A limit <= 0 disables the bound.
func candidateTempHomes(tempDir string, limit int) tempHomeSweep {
	var sweep tempHomeSweep
	level1, ok := readTempRoot(&sweep, tempDir)
	if !ok {
		return sweep
	}
	for _, name := range level1 {
		if limit > 0 && len(sweep.candidates) >= limit {
			break
		}
		if sweep.entriesRead >= tempHomeReadBudget {
			break
		}
		dir := filepath.Join(tempDir, name)
		sweep.candidates = append(sweep.candidates, dir)
		expanded, listErr := expandTempHomeChildren(&sweep, dir, limit)
		if listErr != nil {
			// A directory we could not list may hold homes we will never see.
			// NOT counted as visited: that would make it indistinguishable from
			// one assessed in full, and if every entry failed this way the sweep
			// would report a complete run having looked at nothing.
			sweep.unlistable++
			continue
		}
		if !expanded {
			// Deliberately NOT counted as visited. A partly expanded entry is
			// one we did not finish, and counting it would make visited equal
			// offered on a single-directory temp dir — hiding the truncation
			// behind the very entry that caused it.
			break
		}
		sweep.visited++
	}
	return sweep
}

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
			homes[filepath.Clean(home)] = true
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
