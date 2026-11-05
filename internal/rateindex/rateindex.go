// Package rateindex is the bounded reader of account usage evidence (#6776
// slice 1b): it finds the newest rate_limits record an agent wrote in its own
// rollout store, newest by RECORD TIMESTAMP across files — never by file
// mtime, because a carried file's mtime is copy time while its records are
// old.
//
// The walk is deliberately cheap and bounded, so a refresh can run off the
// daemon's poll goroutine on its own cadence: candidate files are picked by
// stat while descending the store's year/month/day directories NEWEST-FIRST,
// so a deadline truncation or the early-stop bound can only leave older
// directories unexamined, never the newest ones. Each file contributes at
// most its newest rate_limits claim from a capped tail window, and per-file
// results are memoized on (path, size, mtime, watermark) so an unchanged file
// is never re-read. Hard caps bound every refresh; hitting one outside a
// certified remainder reports unknown — a laggy read must not be mistaken
// for a healthy account.
//
// Carried-prefix watermarks (handoff.State.CarriedPrefixes) scope the poison:
// only the copied prefix of a carried file holds the SOURCE account's
// numbers, so claims whose line begins below a file's watermark are ignored
// while records the target's own runtime appended past it count normally.
package rateindex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/quota"
)

// Status is the account-level verdict of one refresh.
type Status int

const (
	// StatusNone: no rate_limits claim exists in the eligible store region.
	// This is "no evidence", NOT a healthy account — absence never reports
	// headroom.
	StatusNone Status = iota
	// StatusObserved: the newest claim parsed; Reading carries its windows.
	StatusObserved
	// StatusUnknown: the newest claim is malformed, or the scan could not be
	// completed inside its bounds. Lag reports unknown.
	StatusUnknown
)

func (s Status) String() string {
	switch s {
	case StatusObserved:
		return "observed"
	case StatusUnknown:
		return "unknown"
	default:
		return "none"
	}
}

// AccountReading is one refresh's answer for one agent home.
type AccountReading struct {
	Status Status
	// Reading is the newest well-formed rate_limits snapshot; set only under
	// StatusObserved.
	Reading quota.UsageReading
	// Reason is an af-authored code for StatusUnknown — never file content or
	// transcript text, which could carry anything an agent printed.
	Reason string
	// Files counts the candidate rollouts in scope this refresh (read or
	// memo-hit).
	Files int
	// BytesRead counts bytes actually read this refresh (memo hits read
	// nothing) — the observability half of the cost bound.
	BytesRead int64
}

// Options bounds one Refresh. The zero value applies the defaults; the fields
// exist so tests can exercise the bounds without fixtures of bounded size.
type Options struct {
	// MaxFiles caps candidate rollouts examined per refresh (default 32).
	MaxFiles int
	// MaxTotalBytes caps bytes read across all files (default 4 MiB).
	MaxTotalBytes int64
	// MaxPerFileBytes caps the tail window read per file (default 1 MiB).
	MaxPerFileBytes int64
	// Deadline bounds wall time per refresh (default 250 ms).
	Deadline time.Duration
	// CarriedPrefixes maps rollout path → copied-prefix length: claims whose
	// line begins below the watermark are ignored (the copied bytes hold the
	// source account's numbers). Keys must match the scanned paths exactly —
	// callers derive both from the same absolute home path.
	CarriedPrefixes map[string]int64
}

const (
	defaultMaxFiles        = 32
	defaultMaxTotalBytes   = 4 << 20
	defaultMaxPerFileBytes = 1 << 20
	defaultDeadline        = 250 * time.Millisecond
)

func (o Options) withDefaults() Options {
	if o.MaxFiles <= 0 {
		o.MaxFiles = defaultMaxFiles
	}
	if o.MaxTotalBytes <= 0 {
		o.MaxTotalBytes = defaultMaxTotalBytes
	}
	if o.MaxPerFileBytes <= 0 {
		o.MaxPerFileBytes = defaultMaxPerFileBytes
	}
	if o.Deadline <= 0 {
		o.Deadline = defaultDeadline
	}
	return o
}

// codexRolloutFileRE is the same filename shape session/conversation_capture.go
// recognizes as a Codex rollout: rollout-<stamp>-<uuid>.jsonl, case-insensitive.
var codexRolloutFileRE = regexp.MustCompile(`(?i)^rollout-.+-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.jsonl$`)

var errDeadline = errors.New("rateindex: enumeration deadline")

// stillAppendingHorizon is the documented early-stop bound: how long past the
// end of its day directory a rollout file may still be appended to. A file
// keeps being written only while its session lives, so a parked session
// resumed within a weekly-limit window stretches that furthest — 30 days
// covers stragglers with a wide margin. The residual the bound accepts: a
// resume of a conversation older than the horizon appends outside it, so a
// skipped directory could hide the truly-newest claim — the reading then lags
// one file, which the mover re-verifies around rather than treating as truth.
const stillAppendingHorizon = 30 * 24 * time.Hour

// Indexer memoizes per-file scan results so a refresh costs reads only on
// files that changed since the last one. The memo holds no file content —
// verdicts and timestamps only.
type Indexer struct {
	now  func() time.Time
	mu   sync.Mutex
	memo map[string]fileMemo
}

// fileMemo is the cached outcome of scanning one file's eligible region at one
// (size, mtime, watermark, window cap): the newest claim found, or that none
// was found, or that the window closed before one was. The window key is the
// EFFECTIVE cap applied — the smaller of the per-file bound and the remaining
// total budget — so a truncated-by-budget result is never replayed as a fuller
// scan.
type fileMemo struct {
	size      int64
	modTime   time.Time
	watermark int64
	window    int64
	claim     fileClaim
	bounded   bool
}

// fileClaim is one file's newest rate_limits claim. sortAt is the record's own
// timestamp when it parsed, else the file's mtime — an upper bound on when the
// claim was written, which is the honest ordering an unparseable record gets.
type fileClaim struct {
	verdict quota.RateLimitsVerdict
	reading quota.UsageReading
	sortAt  time.Time
}

// New returns an indexer on wall-clock time.
func New() *Indexer {
	return &Indexer{now: time.Now, memo: map[string]fileMemo{}}
}

func newForTest(now func() time.Time) *Indexer {
	return &Indexer{now: now, memo: map[string]fileMemo{}}
}

// Refresh scans the rollout store under home (the account's own CODEX_HOME)
// and returns the newest rate_limits evidence. A store that does not exist is
// StatusNone — absent rollouts are a real answer, not an error.
func (ix *Indexer) Refresh(home string, opts Options) AccountReading {
	opts = opts.withDefaults()
	deadline := ix.now().Add(opts.Deadline)

	enum, err := candidates(home, opts.MaxFiles, deadline, ix.now)
	if err != nil {
		return AccountReading{Status: StatusUnknown, Reason: "store-unreadable"}
	}
	result := AccountReading{Files: len(enum.files)}
	// Record timestamps are not bounded by a file's mtime — a backdated or
	// skewed record can postdate it — so no unexamined content is ever proven
	// irrelevant: a deadline-truncated walk, a MaxFiles drop, or a certified
	// directory skip all leave the refresh indeterminate.
	indeterminate := enum.bounded || enum.hasMissed

	var best *fileClaim
	for _, c := range enum.files {
		if ix.now().After(deadline) || result.BytesRead >= opts.MaxTotalBytes {
			// An unexamined file can hold the newest claim regardless of its
			// mtime.
			indeterminate = true
			break
		}
		// Clamp the file's window to the remaining total budget so the cap
		// holds exactly; a window that cannot cover the eligible region is a
		// bounded read, not a complete one.
		window := opts.MaxPerFileBytes
		if remaining := opts.MaxTotalBytes - result.BytesRead; remaining < window {
			window = remaining
		}
		claim, bounded, bytesRead, readErr := ix.fileClaim(c, opts, window)
		result.BytesRead += bytesRead
		if readErr != nil || bounded {
			// An unreadable or partially-read file can hold the newest claim.
			indeterminate = true
		}
		if claim.verdict == quota.RateLimitsAbsent {
			continue
		}
		if best == nil || claim.sortAt.After(best.sortAt) {
			best = &claim
		}
	}
	switch {
	case best == nil && indeterminate:
		result.Status, result.Reason = StatusUnknown, "scan-bounded"
	case best == nil:
		result.Status = StatusNone
	case best.verdict == quota.RateLimitsMalformed:
		result.Status, result.Reason = StatusUnknown, "malformed-record"
	case indeterminate:
		// A file that could hold a newer claim went unexamined; the observed
		// reading might not be the newest.
		result.Status, result.Reason = StatusUnknown, "scan-bounded"
	default:
		result.Status, result.Reading = StatusObserved, best.reading
	}
	return result
}

type candidate struct {
	path    string
	size    int64
	modTime time.Time
}

// candidateWalk carries one enumeration's mutable state.
type candidateWalk struct {
	top       []candidate
	maxFiles  int
	deadline  time.Time
	now       func() time.Time
	seen      int
	stopped   bool // horizon-bound stop: no skipped dir can join the mtime slate
	hasMissed bool // unexamined content exists: cap drops or the early-stop remainder
}

// enumeration is candidates' result: the candidate slate plus what the scan
// must know about content the walk left unexamined. Skipped content is
// reported, not ranked — a record timestamp is not bounded by file mtime, so
// an unexamined file can hold the newest claim no matter how old it looks.
type enumeration struct {
	files     []candidate
	bounded   bool // the deadline truncated the walk in uncertified territory
	hasMissed bool // content was skipped unexamined (drops or the early stop)
}

// candidates enumerates rollout files under <home>/sessions NEWEST-FIRST and
// keeps the newest maxFiles by mtime. Codex lays the store out as
// sessions/YYYY/MM/DD/rollout-*.jsonl (UTC); descending those zero-padded
// directory names is chronological order, so any cut — the deadline, or the
// early-stop bound below — leaves only OLDER directories unexamined.
// Enumeration is stat-only; content is never read here.
//
// The walk stops early once the candidate slate is full and every remaining
// day directory is past stillAppendingHorizon relative to the weakest held
// mtime: a file under such a directory last wrote no later than
// dayEnd+horizon, so it cannot displace a held candidate. A slate that is not
// yet full keeps walking — an old directory can still hold a newer claim when
// there is no held evidence to beat. An expired deadline in uncertified
// territory reports the partial list as bounded rather than pretending
// completeness.
func candidates(home string, maxFiles int, deadline time.Time, now func() time.Time) (enumeration, error) {
	walk := &candidateWalk{maxFiles: maxFiles, deadline: deadline, now: now}
	err := walk.dir(filepath.Join(home, "sessions"), 0, "", "")
	if err != nil && !errors.Is(err, errDeadline) {
		if os.IsNotExist(err) {
			return enumeration{}, nil
		}
		return enumeration{}, err
	}
	sort.Slice(walk.top, func(i, j int) bool { return walk.top[i].modTime.After(walk.top[j].modTime) })
	return enumeration{
		files:     walk.top,
		bounded:   errors.Is(err, errDeadline),
		hasMissed: walk.hasMissed,
	}, nil
}

// dir processes one directory level: files are statted into the top-k slate,
// subdirectories are descended in descending name order. depth counts levels
// below sessions/ — 0 is the year level, 1 month, 2 the level whose children
// are day directories. year and month carry the ancestor directory names the
// day-date check needs at depth 2.
func (w *candidateWalk) dir(path string, depth int, year, month string) error {
	if w.stopped {
		return nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	// Descending name order is newest-first for the zero-padded date
	// directories; rollout filenames embed their creation timestamp, so it is
	// approximately newest-first inside a day directory too.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() })
	for _, entry := range entries {
		// Every visited entry advances the cadence counter BEFORE the
		// eligibility filter: a directory full of compressed rollouts or a
		// tree of empty day directories must trip the deadline just like one
		// full of candidates — a walk that outlives its budget without
		// finding claims is not "no evidence".
		if w.seen++; w.seen%256 == 0 && w.now().After(w.deadline) {
			return errDeadline
		}
		if entry.IsDir() || !codexRolloutFileRE.MatchString(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		var dropped *candidate
		w.top, dropped = insertCandidate(w.top, candidate{
			path:    filepath.Join(path, entry.Name()),
			size:    info.Size(),
			modTime: info.ModTime(),
		}, w.maxFiles)
		if dropped != nil {
			// A file that lost the mtime race still counts as unexamined
			// content — its records are not bounded by that mtime.
			w.hasMissed = true
		}
	}
	for _, entry := range entries {
		if w.stopped {
			// A descendant hit the early-stop bound: unwind without further
			// deadline checks so a completed walk is never mislabeled
			// truncated.
			return nil
		}
		if !entry.IsDir() {
			continue
		}
		// The early-stop bound runs before the deadline check on purpose: it
		// certifies that no skipped directory can join the mtime slate, so
		// stopping there bounds enumeration cost rather than truncating it.
		// The skipped content is still reported — unexamined records can hold
		// the newest claim.
		if depth == 2 && w.pastHorizon(year, month, entry.Name()) {
			w.hasMissed = true
			w.stopped = true
			return nil
		}
		if w.now().After(w.deadline) {
			return errDeadline
		}
		nextYear, nextMonth := year, month
		switch depth {
		case 0:
			nextYear = entry.Name()
		case 1:
			nextMonth = entry.Name()
		}
		if err := w.dir(filepath.Join(path, entry.Name()), depth+1, nextYear, nextMonth); err != nil {
			return err
		}
	}
	return nil
}

// pastHorizon reports whether the day directory named day under year/month is
// old enough — relative to the weakest candidate already held — that no file
// in it, or in any still-older directory (the walk descends), can join the
// mtime-ordered candidate slate. The bound certifies SLATE membership only:
// a skipped file can still hold a newer record timestamp, which is why its
// content counts as missed. Certification requires a full candidate slate and
// a parseable YYYY/MM/DD date; anything else keeps walking.
func (w *candidateWalk) pastHorizon(year, month, day string) bool {
	if len(w.top) < w.maxFiles {
		return false
	}
	yy, errY := strconv.Atoi(year)
	mm, errM := strconv.Atoi(month)
	dd, errD := strconv.Atoi(day)
	if errY != nil || errM != nil || errD != nil ||
		len(year) != 4 || len(month) != 2 || len(day) != 2 ||
		mm < 1 || mm > 12 || dd < 1 || dd > 31 {
		return false
	}
	// The latest possible mtime of a file under this day is the end of its day
	// plus the append horizon. If even that precedes the weakest held mtime,
	// nothing here can join the slate — and every still-older directory fails
	// harder.
	cutoff := time.Date(yy, time.Month(mm), dd, 0, 0, 0, 0, time.UTC).
		AddDate(0, 0, 1).Add(stillAppendingHorizon)
	return cutoff.Before(w.top[w.maxFiles-1].modTime)
}

// insertCandidate maintains top as a mtime-desc list of at most maxFiles,
// reporting the entry the insertion displaced — the rejected candidate or the
// evicted weakest — which counts as unexamined content: a file that lost the
// mtime race can still hold the newest record timestamp.
func insertCandidate(top []candidate, c candidate, maxFiles int) ([]candidate, *candidate) {
	if len(top) == maxFiles && !c.modTime.After(top[len(top)-1].modTime) {
		return top, &c
	}
	pos := sort.Search(len(top), func(i int) bool { return c.modTime.After(top[i].modTime) })
	top = append(top, candidate{})
	copy(top[pos+1:], top[pos:])
	top[pos] = c
	var dropped *candidate
	if len(top) > maxFiles {
		evicted := top[maxFiles]
		dropped = &evicted
		top = top[:maxFiles]
	}
	return top, dropped
}

// fileClaim returns one file's newest claim inside its eligible region —
// memoized on (path, size, mtime, watermark, window cap): a file that has not
// grown and whose watermark and cap are unchanged cannot hold a different
// claim. window is the effective cap applied this refresh — the remaining
// total budget can tighten it below Options.MaxPerFileBytes.
func (ix *Indexer) fileClaim(c candidate, opts Options, window int64) (fileClaim, bool, int64, error) {
	watermark := opts.CarriedPrefixes[c.path]
	ix.mu.Lock()
	prev, seen := ix.memo[c.path]
	ix.mu.Unlock()
	if seen && prev.size == c.size && prev.modTime.Equal(c.modTime) &&
		prev.watermark == watermark && prev.window == window {
		return prev.claim, prev.bounded, 0, nil
	}
	claim, bounded, bytesRead, err := scanTail(c.path, c.size, watermark, window)
	if err == nil {
		ix.mu.Lock()
		ix.memo[c.path] = fileMemo{
			size: c.size, modTime: c.modTime, watermark: watermark,
			window: window, claim: claim, bounded: bounded,
		}
		ix.mu.Unlock()
	}
	return claim, bounded, bytesRead, err
}

// scanTail reads the eligible tail of one rollout — [watermark, size), capped
// at window — and reports its newest rate_limits claim. bounded reports that
// the eligible region could not be scanned whole: the unscanned prefix may
// still hold a newer record timestamp even when the tail yielded a claim,
// because record order is not timestamp order.
//
// Only lines whose START offset lies at or past the watermark count: the
// watermark is a record boundary by contract, and anything below it is the
// source account's copied bytes.
func scanTail(path string, size, watermark, window int64) (fileClaim, bool, int64, error) {
	if size <= watermark {
		return fileClaim{verdict: quota.RateLimitsAbsent}, false, 0, nil
	}
	lo := size - window
	if lo < watermark {
		lo = watermark
	}
	if lo < 0 {
		lo = 0
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fileClaim{}, false, 0, fmt.Errorf("open rollout %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fileClaim{}, false, 0, fmt.Errorf("inspect opened rollout %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fileClaim{}, false, 0, fmt.Errorf("read rollout %s: not a regular file (%s)", path, info.Mode().Type())
	}
	buf := make([]byte, size-lo)
	read, err := file.ReadAt(buf, lo)
	if err != nil && err != io.EOF {
		return fileClaim{}, false, 0, fmt.Errorf("read rollout %s: %w", path, err)
	}
	buf = buf[:read]

	lines := bytes.Split(buf, []byte{'\n'})
	// A window opening mid-file begins inside a line — the first fragment is
	// not a record start and is dropped. Every retained line then begins at
	// or past lo ≥ watermark, so no per-line offset check is needed: the
	// window's lower bound IS the watermark (a record boundary by contract).
	if lo > 0 && lo != watermark {
		lines = lines[1:]
	}
	// The file's claim is its newest claim by RECORD TIMESTAMP, not the last
	// claim line: a carried prefix can hold records whose timestamps postdate
	// the target's own appends, so position order and record order differ
	// exactly where it matters.
	claim := fileClaim{verdict: quota.RateLimitsAbsent}
	found := false
	mtime := info.ModTime()
	for _, raw := range lines {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}
		verdict, reading := quota.ParseCodexRateLimitsLine(line)
		var sortAt time.Time
		switch verdict {
		case quota.RateLimitsParsed:
			sortAt = reading.RecordedAt
		case quota.RateLimitsMalformed:
			// A malformed snapshot can still carry a valid record timestamp —
			// order by it so an old malformed claim cannot shadow the newer
			// valid claims its file keeps accruing. The file's mtime is the
			// fallback only when the timestamp itself is invalid.
			var ok bool
			if sortAt, ok = recordTimestamp(line); !ok {
				sortAt = mtime
			}
		default:
			continue
		}
		if !found || !sortAt.Before(claim.sortAt) {
			claim = fileClaim{verdict: verdict, reading: reading, sortAt: sortAt}
			found = true
		}
	}
	// If the window did not reach the eligible region's bottom, the unscanned
	// prefix could still hold a newer record timestamp — bounded whether or
	// not the tail yielded a claim.
	return claim, lo > watermark, int64(read), nil
}

// recordTimestamp extracts a rollout line's envelope timestamp for ordering a
// malformed claim — the snapshot shape may be wrong while the record's own
// timestamp is still a usable ordering key.
func recordTimestamp(line []byte) (time.Time, bool) {
	var envelope struct {
		Timestamp json.RawMessage `json:"timestamp"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return time.Time{}, false
	}
	var text string
	if err := json.Unmarshal(envelope.Timestamp, &text); err != nil {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}
