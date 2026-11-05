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
	"time"
	"unicode"

	"github.com/sachiniyer/agent-factory/config"
)

// EventFilePrefix names the day-rotated files under <af-home>/instances/.
const EventFilePrefix = "auto-handoff-events-"

// eventRetentionDays bounds how long a day's file is kept (design R14).
const eventRetentionDays = 7

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
	if err := os.MkdirAll(l.dir, 0o755); err != nil {
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
			f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
			if err != nil {
				return err
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
			if !strings.HasPrefix(name, EventFilePrefix) || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			stamp := strings.TrimSuffix(strings.TrimPrefix(name, EventFilePrefix), ".jsonl")
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

// sanitizeEventLabel caps and control-strips a data field (title, key).
func sanitizeEventLabel(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	if len(s) > eventLabelCap {
		s = s[:eventLabelCap]
	}
	return s
}
