package planusage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	RunBuckets map[string]map[int64]float64 `json:"run_buckets,omitempty"` // key: session|account
	// RunDirs holds, per item session, the transcript directories seen for it.
	// Claude files a conversation under the directory it was launched in, and a
	// local session's worktree moves when it is archived, so the directory
	// derived from the worktree later is not where the transcripts are.
	RunDirs map[string][]string `json:"run_dirs,omitempty"`
}

// RememberRunDirs adds dirs to the session's remembered transcript directories
// and returns the union.
func (s *MeterState) RememberRunDirs(sessionID string, dirs []string) []string {
	if s.RunDirs == nil {
		s.RunDirs = map[string][]string{}
	}
	have := s.RunDirs[sessionID]
	for _, d := range dirs {
		if d == "" {
			continue
		}
		found := false
		for _, h := range have {
			if h == d {
				found = true
				break
			}
		}
		if !found {
			have = append(have, d)
		}
	}
	s.RunDirs[sessionID] = have
	return have
}

// runBucketKey keys a run's window buckets by the session AND the account the
// cost was incurred under: a session handed from one account to another keeps
// the spend of each in its own series, so a window only sums what was spent
// under its account (Codex P2 on #6299).
func runBucketKey(sessionID, account string) string { return sessionID + "|" + account }

func sessionOfBucketKey(key string) string {
	if i := strings.LastIndex(key, "|"); i >= 0 {
		return key[:i]
	}
	return key
}

// RunSnapshot is a session's run metering state, to put back when the cost it
// produced could not be committed.
type RunSnapshot struct {
	Seen    map[string]int64
	Buckets map[string]map[int64]float64 // by account
}

// SnapshotRun copies the session's counted-message keys and window buckets.
func (s *MeterState) SnapshotRun(sessionID string) RunSnapshot {
	snap := RunSnapshot{Seen: map[string]int64{}, Buckets: map[string]map[int64]float64{}}
	for k, v := range s.RunSeen[sessionID] {
		snap.Seen[k] = v
	}
	for key, b := range s.RunBuckets {
		if sessionOfBucketKey(key) != sessionID {
			continue
		}
		cp := make(map[int64]float64, len(b))
		for k, v := range b {
			cp[k] = v
		}
		snap.Buckets[key] = cp
	}
	return snap
}

// RestoreRun puts a snapshot back: the dedupe keys must not advance past the
// offsets and costs that were actually stored, or the next pass skips messages
// whose cost was never recorded.
func (s *MeterState) RestoreRun(sessionID string, snap RunSnapshot) {
	if s.RunSeen == nil {
		s.RunSeen = map[string]map[string]int64{}
	}
	s.RunSeen[sessionID] = snap.Seen
	for key := range s.RunBuckets {
		if sessionOfBucketKey(key) == sessionID {
			delete(s.RunBuckets, key)
		}
	}
	if len(snap.Buckets) == 0 {
		return
	}
	if s.RunBuckets == nil {
		s.RunBuckets = map[string]map[int64]float64{}
	}
	for key, b := range snap.Buckets {
		s.RunBuckets[key] = b
	}
}

// AddRunEntries buckets a run's newly metered messages under the account that
// incurred them.
func (s *MeterState) AddRunEntries(sessionID, account string, entries []Entry) {
	if len(entries) == 0 {
		return
	}
	if s.RunBuckets == nil {
		s.RunBuckets = map[string]map[int64]float64{}
	}
	key := runBucketKey(sessionID, account)
	b := s.RunBuckets[key]
	if b == nil {
		b = map[int64]float64{}
		s.RunBuckets[key] = b
	}
	for _, e := range entries {
		b[e.At.Truncate(BucketSpan).Unix()] += e.Cost
	}
}

// RunWindowCost sums a run's buckets incurred under account in the 5-hour window
// w; 0 when w has no valid reading.
func (s *MeterState) RunWindowCost(sessionID, account string, w PlanWindow) float64 {
	if !w.Valid() {
		return 0
	}
	from, to := w.ResetsAt.Add(-FiveHourSpan).Unix(), w.ResetsAt.Unix()
	total := 0.0
	for k, c := range s.RunBuckets[runBucketKey(sessionID, account)] {
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
	for key := range s.RunBuckets {
		if !keep[sessionOfBucketKey(key)] {
			delete(s.RunBuckets, key)
		}
	}
	for id := range s.RunDirs {
		if !keep[id] {
			delete(s.RunDirs, id)
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
	return config.AtomicWriteFile(path, append(data, '\n'), 0o600)
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

// Scan meters the transcripts of one account; see ScanAll.
func (s *MeterState) Scan(account, configDir string, backlogDirs []string, overrides map[string]Price, now time.Time) bool {
	return s.ScanAll(map[string][]string{account: {configDir}}, backlogDirs, overrides, now)
}

// ScanAll meters every Claude Code transcript under each account's config dirs.
// A file under one of backlogDirs also counts as backlog cost. A file untouched
// for longer than the weekly window is not read and keeps no mark, and a file
// whose size and modification time match its mark is not opened.
//
// Files are read oldest first across ALL accounts, and a message already counted
// for another account is not counted again: an account handoff copies the
// conversation into the destination's config dir, and the source's (older)
// transcript is the one that consumed the source's plan. A file that skipped a
// model with no price is read again from the start once that model has a price.
// It returns whether anything changed.
func (s *MeterState) ScanAll(sources map[string][]string, backlogDirs []string, overrides map[string]Price, now time.Time) bool {
	type candidate struct {
		account, path string
		info          os.FileInfo
	}
	var files []candidate
	present := map[string]map[string]bool{}
	for account, dirs := range sources {
		s.account(account)
		present[account] = map[string]bool{}
		for _, configDir := range dirs {
			if configDir == "" {
				continue
			}
			projects, _ := filepath.Glob(filepath.Join(configDir, "projects", "*"))
			for _, f := range TranscriptFiles(projects) {
				info, err := os.Stat(f)
				if err != nil || now.Sub(info.ModTime()) > keepFor {
					continue
				}
				present[account][f] = true
				files = append(files, candidate{account, f, info})
			}
		}
	}
	sort.SliceStable(files, func(i, j int) bool {
		if !files[i].info.ModTime().Equal(files[j].info.ModTime()) {
			return files[i].info.ModTime().Before(files[j].info.ModTime())
		}
		if files[i].account != files[j].account {
			return files[i].account < files[j].account
		}
		return files[i].path < files[j].path
	})
	changed := false
	for _, c := range files {
		a := s.Accounts[c.account]
		mark, known := a.Files[c.path]
		if known && mark.Skipped != "" && anyPriced(mark.Skipped, overrides) {
			mark, known = FileMark{}, false // price the messages that were read past
		}
		if known && c.info.Size() >= mark.Offset && (c.info.Size() == mark.Offset || c.info.ModTime().UnixNano() == mark.ModTime) {
			continue
		}
		foreign := func(key string) bool {
			for name, other := range s.Accounts {
				if name == c.account {
					continue
				}
				if _, ok := other.Seen[key]; ok {
					return true
				}
			}
			return false
		}
		res, err := ReadTranscript(c.path, mark, ReadOptions{Overrides: overrides, SkipBad: true, SkipUnpriced: true, Seen: a.Seen, Foreign: foreign})
		if err != nil && res.Mark == mark {
			continue
		}
		unpriced := map[string]bool{}
		for _, m := range a.Unpriced {
			unpriced[m] = true
		}
		for _, m := range res.Skipped {
			if !unpriced[m] {
				a.Unpriced = append(a.Unpriced, m)
			}
		}
		backlog := underAny(c.path, backlogDirs)
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
		if res.Mark != a.Files[c.path] || !known {
			a.Files[c.path] = res.Mark
			changed = true
		}
	}
	cutoff := now.Add(-keepFor).Unix()
	for account := range sources {
		a := s.Accounts[account]
		for f := range a.Files {
			if !present[account][f] {
				delete(a.Files, f)
				changed = true
			}
		}
		for k := range a.Buckets {
			if k < cutoff {
				delete(a.Buckets, k)
				changed = true
			}
		}
		if PruneSeen(a.Seen, now) {
			changed = true
		}
	}
	return changed
}

// anyPriced reports whether any of the comma-joined models now has a price.
func anyPriced(models string, overrides map[string]Price) bool {
	for _, m := range strings.Split(models, ",") {
		if m == "" {
			continue
		}
		if _, ok := PriceFor(m, overrides); ok {
			return true
		}
	}
	return false
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
