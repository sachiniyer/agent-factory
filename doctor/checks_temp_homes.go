package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Temp-home liveness: which abandoned agent-factory homes under the temp dir are
// safe to remove, and the --fix that removes them. Split out of checks.go to keep
// both files under the length limit (#1145); the rules live together because
// every one of them is a KEEP-reason guarding the same rm -rf.

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
	tmuxHomes, err := liveTmuxHomes(ctx)
	if err != nil {
		// Without the tmux signal a home whose daemon is dead but whose sessions
		// are live has NO keep-reason left, and the lock probe cannot see them.
		// Classify nothing rather than arm a removal on a surface we could not read.
		report.Fail(sectionProcesses, "stale-temp-home",
			fmt.Sprintf("could not list tmux sessions, so no temp home was checked for a live session "+
				"referencing it: %v", err),
			"this check is UNKNOWN, not clean — resolve the tmux listing failure and re-run `af doctor`")
		return
	}

	for _, dir := range candidateTempHomes(tempDir) {
		dir = filepath.Clean(dir)
		if dir == activeHome || !isAFHome(dir) {
			continue
		}
		// POSITIVE proofs of use come first. Each can only ADD a reason to keep
		// the home; none authorises a delete, so an unread surface here costs at
		// worst an unreported keep — never a wrong removal.
		//
		// That holds only because a FAILED tmux listing is refused above rather
		// than read as "no session references this home". Collapsing it would
		// withdraw the one keep-reason a home with live sessions and a dead
		// daemon has, and the removal below would proceed on the lock alone.
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
		if !pathInside(tempDir, dir) {
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
				Remediation: "run `af doctor --fix` to remove it, or `rm -rf " + dir + "`",
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
			Remediation: "verify nothing is using it, then `rm -rf " + dir + "`",
		})
	}
}

// candidateTempHomes lists directories one and two levels below tempDir —
// Go tests produce /tmp/TestName123/001-style homes, manual runs
// /tmp/tmp.XXXX ones.
func candidateTempHomes(tempDir string) []string {
	var out []string
	level1, err := os.ReadDir(tempDir)
	if err != nil {
		return nil
	}
	for _, e := range level1 {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(tempDir, e.Name())
		out = append(out, dir)
		level2, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e2 := range level2 {
			if e2.IsDir() {
				out = append(out, filepath.Join(dir, e2.Name()))
			}
		}
	}
	return out
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

// liveTmuxHomes is a PROTECTIVE set: a home named by a live tmux session must
// not be removed. An unreadable listing therefore cannot be reported as an empty
// one — that silently withdraws the keep-reason and lets the rm -rf proceed
// (#2874's class). The error is returned so both the detection and the fix
// refuse rather than guess.
func liveTmuxHomes(ctx *scanContext) (map[string]bool, error) {
	names, err := listTmuxSessions(ctx)
	if err != nil {
		return nil, err
	}
	homes := map[string]bool{}
	for _, name := range names {
		if !strings.HasPrefix(name, tmux.TmuxPrefix) {
			continue
		}
		if home, ok := tmuxSessionHomeMarker(ctx, name); ok && home != "" {
			homes[filepath.Clean(home)] = true
		}
	}
	return homes, nil
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
		if !pathInside(tempDir, dir) {
			return fmt.Errorf("refusing to remove %s: it is not inside the temp dir %s", dir, tempDir)
		}
		if !isAFHome(dir) {
			return fmt.Errorf("refusing to remove %s: it no longer looks like an agent-factory home", dir)
		}
		// A live tmux session naming the home is the second, sound signal the
		// lock cannot see (a home with live tmux sessions but a dead daemon holds
		// no lock). Re-check it fresh.
		tmuxHomes, err := liveTmuxHomes(ctx)
		if err != nil {
			return fmt.Errorf("refusing to remove %s: could not list tmux sessions, so af cannot tell "+
				"whether a live session references it: %w", dir, err)
		}
		if tmuxHomes[dir] {
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

func pathInside(base, path string) bool {
	rel, err := filepath.Rel(base, path)
	if err != nil || rel == "." || rel == ".." {
		return false
	}
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// timeSince is time.Since, indirected so tests can pin the clock if needed.
var timeSince = time.Since
