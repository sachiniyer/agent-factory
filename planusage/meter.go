package planusage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

const (
	MeterFileName      = "meter.json"
	MeterSchemaVersion = 1

	// BucketSpan is the width of one cost bucket. Windows are summed from
	// whole buckets, so a reset time on a bucket edge counts exactly.
	BucketSpan = 5 * time.Minute
	// keepFor is how long buckets and untouched files stay metered: longer
	// than the weekly window.
	keepFor = SevenDaySpan + 24*time.Hour
)

// Bucket is the metered cost, in US dollars at API prices, of the messages in
// one BucketSpan.
type Bucket struct {
	All     float64 `json:"all"`
	Backlog float64 `json:"backlog,omitempty"`
}

// AccountMeter is one account's metering state.
type AccountMeter struct {
	Files map[string]FileMark `json:"files"`
	// Buckets is keyed by the bucket's start in epoch seconds.
	Buckets map[int64]Bucket `json:"buckets"`
	// Seen holds the keys of the messages counted, so a message copied into
	// another transcript counts once.
	Seen map[string]int64 `json:"seen,omitempty"`
	// Unpriced lists the models seen with no price; their messages are not
	// counted.
	Unpriced []string `json:"unpriced,omitempty"`
}

// MeterState is $AF_HOME/plan/meter.json.
type MeterState struct {
	SchemaVersion int                      `json:"schema_version"`
	Accounts      map[string]*AccountMeter `json:"accounts"`
	// RunSeen holds, per item session, the keys of the messages its run meter
	// counted.
	RunSeen map[string]map[string]int64 `json:"run_seen,omitempty"`
	// RunBuckets holds, per item session, its cost per BucketSpan keyed by
	// the bucket's start in epoch seconds, for the last 5-hour window.
	RunBuckets map[string]map[int64]float64 `json:"run_buckets,omitempty"`
}

// AddRunEntries buckets a run's newly metered messages.
func (s *MeterState) AddRunEntries(sessionID string, entries []Entry) {
	if len(entries) == 0 {
		return
	}
	if s.RunBuckets == nil {
		s.RunBuckets = map[string]map[int64]float64{}
	}
	b := s.RunBuckets[sessionID]
	if b == nil {
		b = map[int64]float64{}
		s.RunBuckets[sessionID] = b
	}
	for _, e := range entries {
		b[e.At.Truncate(BucketSpan).Unix()] += e.Cost
	}
}

// RunWindowCost sums a run's buckets in the 5-hour window w; 0 when w has no
// valid reading.
func (s *MeterState) RunWindowCost(sessionID string, w PlanWindow) float64 {
	if !w.Valid() {
		return 0
	}
	from, to := w.ResetsAt.Add(-FiveHourSpan).Unix(), w.ResetsAt.Unix()
	total := 0.0
	for k, c := range s.RunBuckets[sessionID] {
		if k >= from && k < to {
			total += c
		}
	}
	return total
}

// PruneRunBuckets drops buckets that can no longer fall in a 5-hour window.
func (s *MeterState) PruneRunBuckets(now time.Time) {
	cutoff := now.Add(-FiveHourSpan - BucketSpan).Unix()
	for id, b := range s.RunBuckets {
		for k := range b {
			if k < cutoff {
				delete(b, k)
			}
		}
		if len(b) == 0 {
			delete(s.RunBuckets, id)
		}
	}
}

// RunSeenFor returns the session's set of counted message keys.
func (s *MeterState) RunSeenFor(sessionID string) map[string]int64 {
	if s.RunSeen == nil {
		s.RunSeen = map[string]map[string]int64{}
	}
	seen := s.RunSeen[sessionID]
	if seen == nil {
		seen = map[string]int64{}
		s.RunSeen[sessionID] = seen
	}
	return seen
}

// KeepRunSeen drops the sets and buckets of sessions not in keep.
func (s *MeterState) KeepRunSeen(keep map[string]bool) {
	for id := range s.RunSeen {
		if !keep[id] {
			delete(s.RunSeen, id)
		}
	}
	for id := range s.RunBuckets {
		if !keep[id] {
			delete(s.RunBuckets, id)
		}
	}
}

func NewMeterState() *MeterState {
	return &MeterState{SchemaVersion: MeterSchemaVersion, Accounts: map[string]*AccountMeter{}}
}

// MeterPath returns $AF_HOME/plan/meter.json.
func MeterPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, MeterFileName), nil
}

// LoadMeter reads the meter state. A missing file is an empty state; a file
// from a newer af is refused so it is not overwritten.
func LoadMeter() (*MeterState, error) {
	path, err := MeterPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return NewMeterState(), nil
	}
	if err != nil {
		return nil, err
	}
	s := NewMeterState()
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if s.SchemaVersion > MeterSchemaVersion {
		return nil, fmt.Errorf("%s was written by a newer af (schema %d); upgrade af", path, s.SchemaVersion)
	}
	if s.Accounts == nil {
		s.Accounts = map[string]*AccountMeter{}
	}
	s.SchemaVersion = MeterSchemaVersion
	return s, nil
}

// Save writes the meter state atomically.
func (s *MeterState) Save() error {
	path, err := MeterPath()
	if err != nil {
		return err
	}
	if err := config.MkdirAllUnderAFHome(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return config.AtomicWriteFile(path, append(data, '\n'), 0o644)
}

func (s *MeterState) account(name string) *AccountMeter {
	a := s.Accounts[name]
	if a == nil {
		a = &AccountMeter{}
		s.Accounts[name] = a
	}
	if a.Files == nil {
		a.Files = map[string]FileMark{}
	}
	if a.Buckets == nil {
		a.Buckets = map[int64]Bucket{}
	}
	if a.Seen == nil {
		a.Seen = map[string]int64{}
	}
	return a
}

// Scan meters every Claude Code transcript under configDir for account. A
// file under one of backlogDirs also counts as backlog cost. A file untouched
// for longer than the weekly window is not read and keeps no mark, and a file
// whose size and modification time match its mark is not opened. It returns
// whether anything changed.
func (s *MeterState) Scan(account, configDir string, backlogDirs []string, overrides map[string]Price, now time.Time) bool {
	a := s.account(account)
	projects, _ := filepath.Glob(filepath.Join(configDir, "projects", "*"))
	files := TranscriptFiles(projects)
	present := make(map[string]bool, len(files))
	changed := false
	unpriced := map[string]bool{}
	for _, m := range a.Unpriced {
		unpriced[m] = true
	}
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil || now.Sub(info.ModTime()) > keepFor {
			continue
		}
		present[f] = true
		mark, known := a.Files[f]
		if known && info.Size() >= mark.Offset && (info.Size() == mark.Offset || info.ModTime().UnixNano() == mark.ModTime) {
			continue
		}
		res, err := ReadTranscript(f, mark, ReadOptions{Overrides: overrides, SkipBad: true, SkipUnpriced: true, Seen: a.Seen})
		if err != nil && res.Mark == mark {
			continue
		}
		for _, m := range res.Skipped {
			if !unpriced[m] {
				unpriced[m] = true
				a.Unpriced = append(a.Unpriced, m)
			}
		}
		backlog := underAny(f, backlogDirs)
		for _, e := range res.Entries {
			if now.Sub(e.At) > keepFor {
				continue
			}
			key := e.At.Truncate(BucketSpan).Unix()
			b := a.Buckets[key]
			b.All += e.Cost
			if backlog {
				b.Backlog += e.Cost
			}
			a.Buckets[key] = b
		}
		if res.Mark != mark || !known {
			a.Files[f] = res.Mark
			changed = true
		}
	}
	for f := range a.Files {
		if !present[f] {
			delete(a.Files, f)
			changed = true
		}
	}
	cutoff := now.Add(-keepFor).Unix()
	for k := range a.Buckets {
		if k < cutoff {
			delete(a.Buckets, k)
			changed = true
		}
	}
	if PruneSeen(a.Seen, now) {
		changed = true
	}
	return changed
}

func underAny(path string, dirs []string) bool {
	for _, d := range dirs {
		if d != "" && strings.HasPrefix(path, filepath.Clean(d)+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// costIn sums the buckets that start in [from, to).
func (a *AccountMeter) costIn(from, to time.Time) (all, backlog float64) {
	if a == nil {
		return 0, 0
	}
	f, t := from.Unix(), to.Unix()
	for k, b := range a.Buckets {
		if k >= f && k < t {
			all += b.All
			backlog += b.Backlog
		}
	}
	return all, backlog
}
