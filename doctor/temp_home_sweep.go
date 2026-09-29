// The bounded temp-dir sweep behind the stale-temp-home check: one enumeration
// of the temp dir, capped by a candidate budget AND a per-entry read budget,
// that records how much of the dir it actually got through so a truncated sweep
// can never read as a complete one (#3466). Nothing here knows what an
// agent-factory home is — it sees directories, entries, and budgets; the check
// and report layer in temp_homes.go decides what the candidates mean.
package doctor

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

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
	// roots is every first-level directory NAME the root listing yielded, which
	// the candidate budget never trims: it is complete unless rootPartial or
	// unreadable says otherwise. checkTestResidue reads it, because the dirs it
	// looks for are first-level by construction and must not vanish behind a
	// budget spent on somebody else's nested directories (#4170).
	roots []string
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
	// denied and failed split that count by CAUSE, because the two want
	// opposite advice. A directory another user owns is not a fault and there
	// is nothing to investigate — it is simply not ours to read. An I/O error
	// is a fault, and telling its owner to go clear out /tmp is advice that
	// cannot work. Lumping them together guarantees wrong advice for one of
	// them.
	denied int
	failed int
	// failedErr is the first non-permission child-listing error, so the notice
	// can name what actually went wrong rather than that something did.
	failedErr error
	// listedPartly counts directories whose listing failed AFTER yielding
	// entries. "Could not be listed at all" is false for those.
	listedPartly int
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
	// rootErr holds the error that ended the root listing early, when one did.
	// A listing interrupted by an I/O fault is NOT a listing stopped by a
	// budget, and without the cause the notice describes a failing filesystem
	// as a large one and tells its owner to go clean their temp dir.
	rootErr error
	// hitCandidateLimit and hitReadBudget record WHICH bound stopped the sweep.
	// Both exist because they stop it for different reasons and the notice has
	// to name the right one: a first-level directory holding 500,000 plain
	// files exhausts the read budget while recording a single candidate, and
	// blaming the candidate budget there tells the reader to look at a number
	// that never fired.
	hitCandidateLimit bool
	hitReadBudget     bool
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

// tempHomeProbeSlack is how far past a spent budget the sweep will read purely
// to find out whether the directory had already ended.
//
// A single-entry probe is not enough, and the reason is the same shape as the
// bug it was added for. ReadDir(1) that returns an entry reports io.EOF only on
// the NEXT call, so a directory holding exactly budget+1 entries hands back its
// last one and still looks unfinished — a root of nine entries under an
// eight-entry budget was reported partial having collected all nine. Chasing
// that with one more probe just moves the boundary to budget+2.
//
// So the probe reads up to a bounded slack, and then ONE more entry. Within the
// slack the answer is exact, and the trailing single-entry read is what makes
// that literal rather than nearly true: a directory holding exactly slack
// entries past the budget is consumed by the last batch of the slack, which
// again reports io.EOF only on the following call, so without it the false
// truncation simply moved from budget to budget+slack.
//
// One boundary remains, and it is the one no bounded probe can remove: a
// directory with exactly slack+1 entries left is read to its end and still
// reported truncated, because the read that consumed its final entry is the
// read that proves there was more than a slack to read. Chasing that costs
// another read and hands the same case back one entry further out. It errs
// toward "this run may have missed something", which is the safe direction for
// a report whose counts are already documented as a lower bound.
var tempHomeProbeSlack = 1024

// tempHomeProbeBatch is the read size used while probing. Smaller than the main
// batch so the common case (the directory ended one or two entries ago) costs
// one small read rather than one large one.
var tempHomeProbeBatch = 64

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

// boundedBatch is how many entries the next ordinary read may ask for: a full
// batch, or whatever is left of the read budget if that is smaller.
//
// The budget is checked BEFORE each read, so without this the last read of a
// scan asks for a full batch however little budget remains and overshoots by up
// to batch-1 entries — on top of which the probe then reads a whole slack. The
// slack is the one overrun that is deliberate, and it is the only one the doc
// comments promise; this keeps that promise literal.
func boundedBatch(sweep *tempHomeSweep, readBudget int) int {
	if readBudget <= 0 {
		return tempHomeChildBatch
	}
	if left := readBudget - sweep.entriesRead; left < tempHomeChildBatch {
		// Never zero: every caller checks the budget first and only reaches
		// here with entries still to spend, and ReadDir(0) means "all of it".
		return left
	}
	return tempHomeChildBatch
}

// probeForMoreEntries answers the question a spent read budget cannot: is there
// anything LEFT in this directory, or did it happen to end exactly here?
//
// File.ReadDir returns its final non-empty batch with a nil error and reports
// io.EOF only on the NEXT call. So a directory whose last batch takes the
// counter to the budget has been read in FULL, while a pre-read budget check
// sees the same state as one with a million entries still to go — and reports a
// complete listing as truncated. A root of exactly 500,001 entries, finished by
// its final 1,024-entry batch, was described as "too large to list in full".
//
// One entry is enough to tell them apart, and whatever it reads is handed back
// so the caller keeps it rather than dropping it on the floor.
func probeForMoreEntries(sweep *tempHomeSweep, f *os.File) (entries []os.DirEntry, atEnd bool, err error) {
	read := 0
	for read < tempHomeProbeSlack {
		n := tempHomeProbeBatch
		// Sized to what is left of the slack, so a slack that is not a whole
		// number of batches is still the bound it says it is.
		if left := tempHomeProbeSlack - read; left < n {
			n = left
		}
		batch, readErr := scanDirBatch(f, n)
		sweep.entriesRead += len(batch)
		entries = append(entries, batch...)
		read += len(batch)
		if errors.Is(readErr, io.EOF) {
			return entries, true, nil
		}
		if readErr != nil {
			return entries, false, readErr
		}
		if len(batch) == 0 {
			// No entries and no error. Another call can only tell us the same
			// thing, so stop rather than spin.
			return entries, false, nil
		}
	}

	// The slack is spent and EOF has not been reported. That is precisely the
	// state this function exists to disambiguate, one level up: a directory
	// holding exactly slack entries past the budget was just consumed by the
	// final batch, which returns them with a nil error and reports io.EOF only
	// on the NEXT call. Without this read it would be called truncated having
	// been read in full, which is the same bug at a larger radius.
	//
	// One entry, because it is the ANSWER that is wanted and not the entries:
	// an empty return means the directory ended inside the slack, and a
	// non-empty one means there was genuinely more than the slack to read.
	batch, readErr := scanDirBatch(f, 1)
	sweep.entriesRead += len(batch)
	entries = append(entries, batch...)
	if errors.Is(readErr, io.EOF) {
		return entries, true, nil
	}
	if readErr != nil {
		return entries, false, readErr
	}
	return entries, false, nil
}

// expandTempHomeChildren appends dir's subdirectories to the sweep, stopping at
// the budget. It reports whether the directory was expanded in FULL, and any
// error that ended the listing early.
func expandTempHomeChildren(sweep *tempHomeSweep, dir string, limit, readBudget int) (expanded, sawEntries bool, err error) {
	f, openErr := os.Open(dir)
	if openErr != nil {
		return false, false, openErr
	}
	defer func() { _ = f.Close() }()

	// Collected, sorted, and only then appended. os.ReadDir sorted; File.ReadDir
	// does not, so appending straight from the batches made the candidate order
	// of an ORDINARY, complete scan depend on filesystem order — changing
	// --verbose output and verbose JSON between runs on machines where nothing
	// was truncated at all. Buffering is bounded by the same budgets as the
	// read, so this cannot reintroduce the unbounded memory the streaming fixed.
	var children []string
	defer func() {
		sort.Strings(children)
		sweep.candidates = append(sweep.candidates, children...)
	}()

	for {
		if limit > 0 && len(sweep.candidates)+len(children) >= limit {
			// Same boundary as the read budget, and it was missed there first:
			// reaching the limit exactly as the directory ends is a COMPLETE
			// expansion, not a truncated one. One parent with four children and
			// a limit of five recorded all five candidates and still reported
			// visited == 0, so the summary and summary.incomplete said the check
			// had not finished when it had.
			probe, atEnd, probeErr := probeForMoreEntries(sweep, f)
			unrecordable := 0
			for _, entry := range probe {
				if entry.IsDir() {
					unrecordable++
				}
			}
			if probeErr != nil {
				return false, sawEntries, probeErr
			}
			// atEnd alone is not enough: the probe may have turned up
			// directories there is no budget left to record, and an expansion
			// missing children is not a complete one.
			if atEnd && unrecordable == 0 {
				return true, sawEntries, nil
			}
			sweep.hitCandidateLimit = true
			return false, sawEntries, nil
		}
		if readBudget > 0 && sweep.entriesRead >= readBudget {
			// Charged per ENTRY, not per candidate. Without this a directory of
			// a million files — which contributes no candidates — is read to
			// EOF however small the candidate budget is.
			//
			// Probe first: a directory that ended exactly on the budget is
			// COMPLETE, and calling it truncated would report a finished scan
			// as a partial one. See probeForMoreEntries.
			probe, atEnd, probeErr := probeForMoreEntries(sweep, f)
			recordedAll := true
			for _, entry := range probe {
				if !entry.IsDir() {
					continue
				}
				if limit > 0 && len(sweep.candidates)+len(children) >= limit {
					// The probe reads PAST the budget on purpose, to learn
					// whether the directory had already ended. What it hands
					// back is still subject to the candidate limit: appending
					// it unchecked let one probe put a whole slack's worth of
					// extra candidates over the bound, and every one of them
					// then costs seven isAFHome stats — the work the bound
					// exists to cap.
					recordedAll = false
					sweep.hitCandidateLimit = true
					break
				}
				children = append(children, filepath.Join(dir, entry.Name()))
			}
			if probeErr != nil {
				return false, sawEntries, probeErr
			}
			// A directory whose children we saw and could not record is not a
			// finished expansion, whatever the probe found after them.
			if !recordedAll {
				return false, sawEntries, nil
			}
			if atEnd {
				return true, sawEntries, nil
			}
			sweep.hitReadBudget = true
			return false, sawEntries, nil
		}
		batch, readErr := scanDirBatch(f, boundedBatch(sweep, readBudget))
		sweep.entriesRead += len(batch)
		if len(batch) > 0 {
			sawEntries = true
		}
		for _, entry := range batch {
			if !entry.IsDir() {
				continue
			}
			if limit > 0 && len(sweep.candidates)+len(children) >= limit {
				// Mid-batch, so a directory we cannot record is in our hand
				// already. No probe needed and none would help: this is a
				// genuinely truncated expansion.
				sweep.hitCandidateLimit = true
				return false, sawEntries, nil
			}
			children = append(children, filepath.Join(dir, entry.Name()))
		}
		if errors.Is(readErr, io.EOF) {
			return true, sawEntries, nil
		}
		if readErr != nil {
			return false, sawEntries, readErr
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
func readTempRoot(sweep *tempHomeSweep, tempDir string, readBudget int) ([]string, bool) {
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
		if readBudget > 0 && sweep.entriesRead >= readBudget {
			// Probe before declaring the root partial: a root that ended
			// exactly on the budget was read in full. See probeForMoreEntries.
			probe, atEnd, probeErr := probeForMoreEntries(sweep, f)
			for _, entry := range probe {
				if entry.IsDir() {
					names = append(names, entry.Name())
					sweep.offered++
				}
			}
			if atEnd {
				break
			}
			if probeErr != nil {
				sweep.rootPartial = true
				sweep.rootErr = probeErr
				break
			}
			sweep.rootPartial = true
			sweep.hitReadBudget = true
			break
		}
		batch, readErr := scanDirBatch(f, boundedBatch(sweep, readBudget))
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
			if sweep.entriesRead == 0 {
				// The FIRST read failed, so nothing was listed at all. Opening
				// succeeded and reading did not, which happens when TempDir
				// names a regular file (ENOTDIR) or the filesystem refuses the
				// listing outright. That is an unreadable temp dir, not a large
				// one — reporting it as "too large to list in full" would hide
				// a configuration or I/O fault behind advice to clean /tmp.
				sweep.unreadable = true
				return nil, false
			}
			// Some of the root was read. That is a partial listing, not an
			// absent one: offered becomes a lower bound and the run is reported
			// incomplete rather than unreadable. The CAUSE is kept, because
			// "the listing failed partway" and "the listing hit its budget"
			// call for different things from the reader.
			sweep.rootPartial = true
			sweep.rootErr = readErr
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
	// A caller that disables the candidate bound is asking for a COMPLETE
	// sweep, so the read budget has to come off with it. Leaving it armed made
	// the documented escape hatch a lie: the caller opted out of one bound and
	// silently got truncated by another it was never told about.
	readBudget := tempHomeReadBudget
	if limit <= 0 {
		readBudget = 0
	}

	var sweep tempHomeSweep
	level1, ok := readTempRoot(&sweep, tempDir, readBudget)
	if !ok {
		return sweep
	}
	sweep.roots = level1
	for _, name := range level1 {
		if limit > 0 && len(sweep.candidates) >= limit {
			sweep.hitCandidateLimit = true
			break
		}
		dir := filepath.Join(tempDir, name)
		sweep.candidates = append(sweep.candidates, dir)
		if readBudget > 0 && sweep.entriesRead >= readBudget {
			// No budget left to look INSIDE this one — but its name is already
			// in hand and costs nothing more to record, and the directory
			// itself may BE an abandoned home. Previously this broke out of the
			// loop, so a temp root big enough to exhaust the read budget
			// assessed NOTHING: it reported an incomplete scan and then failed
			// to check even the names it had already paid to read. Expansion
			// stops; assessment does not. Not counted as visited, because its
			// children were never looked at.
			sweep.hitReadBudget = true
			continue
		}
		expanded, sawEntries, listErr := expandTempHomeChildren(&sweep, dir, limit, readBudget)
		if listErr != nil {
			// A directory we could not list may hold homes we will never see.
			// NOT counted as visited: that would make it indistinguishable from
			// one assessed in full, and if every entry failed this way the sweep
			// would report a complete run having looked at nothing.
			sweep.unlistable++
			if sawEntries {
				sweep.listedPartly++
			}
			if errors.Is(listErr, fs.ErrPermission) {
				sweep.denied++
			} else {
				sweep.failed++
				if sweep.failedErr == nil {
					sweep.failedErr = listErr
				}
			}
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
