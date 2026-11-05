package handoff

// events.go owns the shadow decision log:
// <af-home>/instances/auto-handoff-events-YYYYMMDD.jsonl — one JSON row per
// emitted edge, rotated daily and pruned past seven days (#6776 design R14).
//
// Rows hold only af-owned structured fields: the session's stable key, its
// (capped, control-stripped) title, account names, the reason code, and
// numeric/code parameters. Pane, transcript, composer, and banner text can
// never appear here — a row is the record an operator diffs against the
// external watcher's, not a capture.
//
// Writes go through the same discipline the account-limit ledger uses: the
// sibling file lock serializes every writer, and each row lands as one
// append-mode write so a crash can tear at most the final partial line, which
// readers skip.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sachiniyer/agent-factory/config"
)

// EventFilePrefix names the day-rotated files under <af-home>/instances/.
const EventFilePrefix = "auto-handoff-events-"

// eventRetentionDays bounds how long a day's file is kept (design R14).
const eventRetentionDays = 7

// eventDayMaxBytes caps one day's file (#6817 review): a degraded caller that
// emits rows every tick cannot grow the log without bound. Rows past the cap
// are dropped for that day — the file is an operator-diff surface, not a
// transport, and an unbounded one breaks the AF-home guard the same way a
// bare MkdirAll did.
const eventDayMaxBytes = 4 << 20

// eventLabelCap bounds the title field carried on a row — titles are data.
const eventLabelCap = 120

// EventRow is one JSONL line. Field names match the design's row shape.
type EventRow struct {
	TS         time.Time         `json:"ts"`
	Kind       EventKind         `json:"kind"`
	SessionKey string            `json:"session_key,omitempty"`
	Agent      string            `json:"agent,omitempty"`
	Title      string            `json:"title,omitempty"`
	From       string            `json:"from,omitempty"`
	To         string            `json:"to,omitempty"`
	ReasonCode string            `json:"reason_code"`
	Params     map[string]string `json:"params,omitempty"`
	Mode       string            `json:"mode"`
}

// EventLog appends edge rows to the day's file under the ledger's lock
// discipline and prunes the directory past the retention window.
type EventLog struct {
	dir string
	now func() time.Time
}

// NewEventLog returns a log rooted at dir (the instances directory).
func NewEventLog(dir string) *EventLog {
	return &EventLog{dir: dir, now: time.Now}
}

// DefaultEventLog resolves <af-home>/instances/.
func DefaultEventLog() (*EventLog, error) {
	home, err := config.GetConfigDir()
	if err != nil {
		return nil, err
	}
	return NewEventLog(filepath.Join(home, "instances")), nil
}

// eventDayFile names the file holding rows stamped on day.
func (l *EventLog) eventDayFile(day time.Time) string {
	return filepath.Join(l.dir, fmt.Sprintf("%s%s.jsonl", EventFilePrefix, day.UTC().Format("20060102")))
}

// Append writes rows under the day's file lock, then prunes expired files.
// Rows are grouped by their own timestamp's day so a tick spanning midnight
// lands each event on its own day's file.
func (l *EventLog) Append(rows []EventRow) error {
	if len(rows) == 0 {
		return nil
	}
	// MkdirAllUnderAFHome, not os.MkdirAll: this is <af-home>/instances, so a
	// bare MkdirAll could re-create a deleted AF home as an ancestor (#3850;
	// TestNoBareMkdirAllUnderTheAFHome).
	if err := config.MkdirAllUnderAFHome(l.dir, 0o755); err != nil {
		return err
	}
	byDay := make(map[string][]EventRow)
	for _, row := range rows {
		day := l.eventDayFile(row.TS)
		byDay[day] = append(byDay[day], row)
	}
	files := make([]string, 0, len(byDay))
	for file := range byDay {
		files = append(files, file)
	}
	sort.Strings(files)
	for _, file := range files {
		var payload strings.Builder
		for _, row := range byDay[file] {
			row.Title = sanitizeEventLabel(row.Title)
			row.SessionKey = sanitizeEventLabel(row.SessionKey)
			raw, err := json.Marshal(row)
			if err != nil {
				return fmt.Errorf("encode %s row: %w", file, err)
			}
			payload.Write(raw)
			payload.WriteByte('\n')
		}
		if err := config.WithFileLockTimeout(file, config.RepoInstancesLockTimeout, func() error {
			// A symlinked day file is never ours to write through (#6817
			// review): Lstat refuses it outright, O_NOFOLLOW pins the open to
			// a real file, and the SameFile recheck after open closes the
			// window where the path is swapped between the check and the
			// append.
			before, statErr := os.Lstat(file)
			if statErr == nil && before.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("refusing to append through symlink %s", file)
			}
			if statErr == nil && before.Size() >= eventDayMaxBytes {
				// An error, not a silent drop (#6817 round-2 review): the
				// caller's edge must stay uncommitted so the suppressed row
				// re-announces on a later tick instead of vanishing.
				return fmt.Errorf("event file %s at %d-byte day cap: row held for retry", file, eventDayMaxBytes)
			}
			f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
			if err != nil {
				return err
			}
			if statErr == nil {
				after, serr := f.Stat()
				if serr != nil {
					f.Close()
					return serr
				}
				if !os.SameFile(before, after) {
					f.Close()
					return fmt.Errorf("event file %s was replaced during open", file)
				}
			}
			_, werr := f.WriteString(payload.String())
			cerr := f.Close()
			if werr != nil {
				return werr
			}
			return cerr
		}); err != nil {
			return fmt.Errorf("append %s: %w", file, err)
		}
	}
	return l.prune()
}

// prune removes event files older than the retention window. It runs inside
// today's file lock so a racing appender cannot lose a row mid-prune.
func (l *EventLog) prune() error {
	today := l.eventDayFile(l.now())
	return config.WithFileLockTimeout(today, config.RepoInstancesLockTimeout, func() error {
		entries, err := os.ReadDir(l.dir)
		if err != nil {
			return err
		}
		cutoff := l.now().AddDate(0, 0, -eventRetentionDays)
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasPrefix(name, EventFilePrefix) {
				continue
			}
			// The file AND its sibling lock prune on the same stamp (#6817
			// review): a stale <day>.jsonl.lock outlives the rotation it
			// guarded otherwise.
			suffix := ".jsonl"
			if strings.HasSuffix(name, ".jsonl.lock") {
				suffix = ".jsonl.lock"
			} else if !strings.HasSuffix(name, suffix) {
				continue
			}
			stamp := strings.TrimSuffix(strings.TrimPrefix(name, EventFilePrefix), suffix)
			day, err := time.Parse("20060102", stamp)
			if err != nil {
				continue // an unrelated look-alike file is never ours to remove
			}
			if day.Before(cutoff) {
				if err := os.Remove(filepath.Join(l.dir, name)); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// sanitizeEventLabel caps and strips a data field (title, key). Stripped:
// control characters, Unicode FORMAT characters (bidi overrides, zero-width
// spaces, joiners — category Cf, which IsControl misses), and the U+2028/
// U+2029 line separators a JSONL consumer treats as a line break (#6818
// round-2 review). The cap cuts on a RUNE boundary — a mid-rune byte slice
// would smuggle invalid UTF-8 into the row.
func sanitizeEventLabel(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			return -1
		}
		return r
	}, s)
	if len(s) > eventLabelCap {
		cut := eventLabelCap
		for cut > 0 && !utf8.ValidString(s[:cut]) {
			cut--
		}
		s = s[:cut]
	}
	return s
}
