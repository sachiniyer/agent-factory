// Package rateindex is the bounded reader of account usage evidence (#6776
// slice 1b): it finds the newest rate_limits record an agent wrote in its own
// rollout store, newest by RECORD TIMESTAMP across files — never by file
// mtime, because a carried file's mtime is copy time while its records are
// old.
//
// The walk is deliberately cheap and bounded, so a refresh can run off the
// daemon's poll goroutine on its own cadence: candidate files are picked by
// stat, each contributes at most its newest rate_limits claim from a capped
// tail window, and per-file results are memoized on (path, size, mtime,
// watermark) so an unchanged file is never re-read. Hard caps bound every
// refresh; hitting them reports unknown — a laggy read must not be mistaken
// for a healthy account.
//
// Carried-prefix watermarks (handoff.State.CarriedPrefixes) scope the poison:
// only the copied prefix of a carried file holds the SOURCE account's
// numbers, so claims whose line begins below a file's watermark are ignored
// while records the target's own runtime appended past it count normally.
package rateindex

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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
// was found, or that the window closed before one was.
type fileMemo struct {
	size       int64
	modTime    time.Time
	watermark  int64
	perFileCap int64
	claim      fileClaim
	bounded    bool
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

	files, boundedEnum, err := candidates(home, opts.MaxFiles, deadline, ix.now)
	if err != nil {
		return AccountReading{Status: StatusUnknown, Reason: "store-unreadable"}
	}
	result := AccountReading{Files: len(files)}
	indeterminate := boundedEnum

	var best *fileClaim
	for _, c := range files {
		if best != nil && c.modTime.Before(best.sortAt) {
			// Sorted mtime-desc: no remaining file can hold a newer claim
			// (a file's newest record cannot postdate its own mtime).
			break
		}
		if ix.now().After(deadline) || result.BytesRead >= opts.MaxTotalBytes {
			// The first unexamined file could still hold the newest claim.
			if best == nil || !c.modTime.Before(best.sortAt) {
				indeterminate = true
			}
			break
		}
		claim, bounded, bytesRead, readErr := ix.fileClaim(c, opts)
		result.BytesRead += bytesRead
		if readErr != nil {
			bounded = true // an unreadable file could hold the newest claim
		}
		if bounded && (best == nil || !c.modTime.Before(best.sortAt)) {
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

// candidates enumerates rollout files under <home>/sessions and keeps the
// newest maxFiles by mtime. Enumeration is stat-only; content is never read
// here. The deadline check inside the walk keeps a huge store from stalling
// the refresh before a single file is read; an expired deadline reports the
// partial list as bounded rather than pretending completeness.
func candidates(home string, maxFiles int, deadline time.Time, now func() time.Time) ([]candidate, bool, error) {
	root := filepath.Join(home, "sessions")
	top := make([]candidate, 0, maxFiles)
	seen := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || !codexRolloutFileRE.MatchString(d.Name()) {
			return nil
		}
		if seen++; seen%256 == 0 && now().After(deadline) {
			return errDeadline
		}
		info, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		top = insertCandidate(top, candidate{path: path, size: info.Size(), modTime: info.ModTime()}, maxFiles)
		return nil
	})
	if err != nil {
		if errors.Is(err, errDeadline) {
			sort.Slice(top, func(i, j int) bool { return top[i].modTime.After(top[j].modTime) })
			return top, true, nil
		}
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	sort.Slice(top, func(i, j int) bool { return top[i].modTime.After(top[j].modTime) })
	return top, false, nil
}

// insertCandidate maintains top as a mtime-desc list of at most maxFiles.
func insertCandidate(top []candidate, c candidate, maxFiles int) []candidate {
	if len(top) == maxFiles && !c.modTime.After(top[len(top)-1].modTime) {
		return top
	}
	pos := sort.Search(len(top), func(i int) bool { return c.modTime.After(top[i].modTime) })
	top = append(top, candidate{})
	copy(top[pos+1:], top[pos:])
	top[pos] = c
	if len(top) > maxFiles {
		top = top[:maxFiles]
	}
	return top
}

// fileClaim returns one file's newest claim inside its eligible region —
// memoized on (path, size, mtime, watermark, window cap): a file that has not
// grown and whose watermark and cap are unchanged cannot hold a different
// claim.
func (ix *Indexer) fileClaim(c candidate, opts Options) (fileClaim, bool, int64, error) {
	watermark := opts.CarriedPrefixes[c.path]
	ix.mu.Lock()
	prev, seen := ix.memo[c.path]
	ix.mu.Unlock()
	if seen && prev.size == c.size && prev.modTime.Equal(c.modTime) &&
		prev.watermark == watermark && prev.perFileCap == opts.MaxPerFileBytes {
		return prev.claim, prev.bounded, 0, nil
	}
	claim, bounded, bytesRead, err := scanTail(c.path, c.size, watermark, opts.MaxPerFileBytes)
	if err == nil {
		ix.mu.Lock()
		ix.memo[c.path] = fileMemo{
			size: c.size, modTime: c.modTime, watermark: watermark,
			perFileCap: opts.MaxPerFileBytes, claim: claim, bounded: bounded,
		}
		ix.mu.Unlock()
	}
	return claim, bounded, bytesRead, err
}

// scanTail reads the eligible tail of one rollout — [watermark, size), capped
// at perFileCap — and reports its newest rate_limits claim. bounded reports
// that the eligible region could not be scanned whole, so an unscanned claim
// may exist in it.
//
// Only lines whose START offset lies at or past the watermark count: the
// watermark is a record boundary by contract, and anything below it is the
// source account's copied bytes.
func scanTail(path string, size, watermark, perFileCap int64) (fileClaim, bool, int64, error) {
	if size <= watermark {
		return fileClaim{verdict: quota.RateLimitsAbsent}, false, 0, nil
	}
	lo := size - perFileCap
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
			// The record's timestamp cannot be trusted — order the claim by
			// the file's mtime, an upper bound on when it was written.
			sortAt = mtime
		default:
			continue
		}
		if !found || !sortAt.Before(claim.sortAt) {
			claim = fileClaim{verdict: verdict, reading: reading, sortAt: sortAt}
			found = true
		}
	}
	// No claim in the window: if the window reached the region's bottom the
	// answer is "none"; if the per-file cap closed first it is "bounded".
	return claim, !found && lo > watermark && lo > 0, int64(read), nil
}
