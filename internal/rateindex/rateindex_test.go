package rateindex

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Rollout contents are HANDWRITTEN from the pinned Codex record shape — no real
// conversation text, ids, or account names (public repo).

func claimLine(ts string, primaryPct float64) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{"primary":{"used_percent":%v,"window_minutes":300,"resets_at":1761902400},"secondary":null}}}`, ts, primaryPct)
}

func malformedClaimLine(ts string) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{"primary":{"used_percent":"high","window_minutes":300,"resets_at":1761902400}}}}`, ts)
}

var fillerLine = `{"timestamp":"2026-10-31T04:00:00.000Z","type":"event_msg","payload":{"type":"agent_message","message":"fake agent message"}}`

// writeRollout drops a rollout file into <home>/sessions/<dayDir>/ and stamps
// its mtime — the knob that lets a "carried" file look new while holding old
// records.
func writeRollout(t *testing.T, home, dayDir, name string, lines []string, mtime time.Time) string {
	t.Helper()
	dir := filepath.Join(home, "sessions", dayDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}

func rolloutName(n int) string {
	return fmt.Sprintf("rollout-2026-10-31T04-00-00-00000000-0000-4000-8000-%012d.jsonl", n)
}

var base = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

func TestRefreshEmptyStoreIsNone(t *testing.T) {
	got := New().Refresh(t.TempDir(), Options{})
	if got.Status != StatusNone {
		t.Fatalf("status = %v, want none — an absent store is a real answer", got.Status)
	}
}

func TestRefreshReadsNewestClaim(t *testing.T) {
	home := t.TempDir()
	writeRollout(t, home, "2026/10/31", rolloutName(1), []string{
		fillerLine,
		claimLine("2026-10-31T11:00:00.000Z", 55.5),
	}, base)
	got := New().Refresh(home, Options{})
	if got.Status != StatusObserved {
		t.Fatalf("status = %v (%s), want observed", got.Status, got.Reason)
	}
	if len(got.Reading.Windows) != 1 || got.Reading.Windows[0].UsedPercent != 55.5 {
		t.Fatalf("windows = %+v", got.Reading.Windows)
	}
	if !got.Reading.RecordedAt.Equal(mustTime(t, "2026-10-31T11:00:00.000Z")) {
		t.Fatalf("recordedAt = %s", got.Reading.RecordedAt)
	}
}

// The carried-file trap: a file copied at T+10min has a NEW mtime but OLD
// records. Ordering by mtime would crown it; the reader must order by record
// timestamp, so the file holding the genuinely newer record wins.
func TestRefreshOrdersByRecordTimestampNotMtime(t *testing.T) {
	home := t.TempDir()
	writeRollout(t, home, "2026/10/31", rolloutName(1), []string{
		claimLine("2026-10-31T10:00:00.000Z", 10.0),
	}, base.Add(10*time.Minute)) // carried in: fresh mtime, stale records
	writeRollout(t, home, "2026/10/31", rolloutName(2), []string{
		claimLine("2026-10-31T11:30:00.000Z", 22.0),
	}, base.Add(-time.Hour))
	got := New().Refresh(home, Options{})
	if got.Status != StatusObserved || len(got.Reading.Windows) != 1 || got.Reading.Windows[0].UsedPercent != 22.0 {
		t.Fatalf("got = %+v, want the 22%% claim from the older-mtime file", got)
	}
}

// A record timestamp is not bounded by its file's mtime: a file whose mtime
// precedes the best claim found can still hold an even newer record, so the
// scan must not early-exit on mtime order.
func TestRefreshOldMtimeFileCanHoldNewestClaim(t *testing.T) {
	home := t.TempDir()
	writeRollout(t, home, "2026/10/31", rolloutName(1), []string{
		claimLine("2026-10-31T11:30:00.000Z", 5.0),
	}, base) // mtime 12:00
	writeRollout(t, home, "2026/10/31", rolloutName(2), []string{
		claimLine("2026-10-31T11:45:00.000Z", 99.0),
	}, time.Date(2026, 10, 31, 11, 0, 0, 0, time.UTC)) // mtime 11:00 < the 11:30 claim
	got := New().Refresh(home, Options{})
	if got.Status != StatusObserved || got.Reading.Windows[0].UsedPercent != 99.0 {
		t.Fatalf("got = %+v — the old-mtime file's 11:45 claim is the newest evidence", got)
	}
}

// The watermark scopes the poison: only the copied prefix holds the SOURCE
// account's numbers — and the copied bytes can carry a misleadingly NEW
// record timestamp, so ordering alone cannot save us. The source's 99% claim
// inside the prefix must be ignored while the target's own post-carry claim
// counts.
func TestRefreshHonorsCarriedPrefixWatermark(t *testing.T) {
	home := t.TempDir()
	prefix := []string{
		fillerLine,
		claimLine("2026-10-31T12:30:00.000Z", 99.0), // source's wall, copied in
	}
	// The watermark lands on a record boundary: bytes copied = prefix length.
	prefixLen := int64(len(strings.Join(prefix, "\n")) + 1)
	path := writeRollout(t, home, "2026/10/31", rolloutName(1), append(prefix,
		claimLine("2026-10-31T11:45:00.000Z", 5.0), // target's own post-carry record
	), base)

	got := New().Refresh(home, Options{CarriedPrefixes: map[string]int64{path: prefixLen}})
	if got.Status != StatusObserved || len(got.Reading.Windows) != 1 || got.Reading.Windows[0].UsedPercent != 5.0 {
		t.Fatalf("got = %+v, want the post-watermark 5%% claim, never the copied 99%%", got)
	}

	// Control: with no watermark the copied 99% claim is the newest record
	// timestamp in the store — the exact poison R3 exists to prevent.
	got = New().Refresh(home, Options{})
	if got.Status != StatusObserved || got.Reading.Windows[0].UsedPercent != 99.0 {
		t.Fatalf("control got = %+v, want the misleadingly-new copied 99%%", got)
	}
}

func TestWatermarkCoveringWholeFileContributesNothing(t *testing.T) {
	home := t.TempDir()
	path := writeRollout(t, home, "2026/10/31", rolloutName(1), []string{
		claimLine("2026-10-31T10:00:00.000Z", 99.0),
	}, base)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	got := New().Refresh(home, Options{CarriedPrefixes: map[string]int64{path: info.Size() + 10}})
	if got.Status != StatusNone {
		t.Fatalf("status = %v, want none — every byte is the source's copied prefix", got.Status)
	}
}

func TestRefreshNewestMalformedClaimIsUnknown(t *testing.T) {
	home := t.TempDir()
	writeRollout(t, home, "2026/10/31", rolloutName(1), []string{
		claimLine("2026-10-31T10:00:00.000Z", 20.0),
	}, base.Add(-time.Hour))
	writeRollout(t, home, "2026/10/31", rolloutName(2), []string{
		malformedClaimLine("2026-10-31T11:00:00.000Z"),
	}, base)
	got := New().Refresh(home, Options{})
	if got.Status != StatusUnknown || got.Reason != "malformed-record" {
		t.Fatalf("status = %v reason %q, want unknown/malformed-record", got.Status, got.Reason)
	}
}

// An old malformed claim must NOT poison a newer clean one — only the newest
// claim decides.
func TestRefreshOldMalformedDoesNotPoisonNewerValid(t *testing.T) {
	home := t.TempDir()
	writeRollout(t, home, "2026/10/31", rolloutName(1), []string{
		malformedClaimLine("2026-10-31T09:00:00.000Z"),
	}, base.Add(-2*time.Hour))
	writeRollout(t, home, "2026/10/31", rolloutName(2), []string{
		claimLine("2026-10-31T11:00:00.000Z", 30.0),
	}, base)
	got := New().Refresh(home, Options{})
	if got.Status != StatusObserved || got.Reading.Windows[0].UsedPercent != 30.0 {
		t.Fatalf("got = %+v, want observed 30%%", got)
	}
}

func TestRefreshBoundedTailIsUnknown(t *testing.T) {
	home := t.TempDir()
	// A claim below the per-file cap: the window covers only the tail filler.
	writeRollout(t, home, "2026/10/31", rolloutName(1), []string{
		claimLine("2026-10-31T10:00:00.000Z", 70.0),
		strings.Repeat(fillerLine+"\n", 40),
	}, base)
	got := New().Refresh(home, Options{MaxPerFileBytes: 512})
	if got.Status != StatusUnknown || got.Reason != "scan-bounded" {
		t.Fatalf("status = %v reason %q — an unscanned region must report unknown, not none", got.Status, got.Reason)
	}
}

func TestRefreshNoClaimsIsNone(t *testing.T) {
	home := t.TempDir()
	writeRollout(t, home, "2026/10/31", rolloutName(1), []string{
		fillerLine, fillerLine,
	}, base)
	got := New().Refresh(home, Options{})
	if got.Status != StatusNone {
		t.Fatalf("status = %v, want none — no evidence is not a healthy account", got.Status)
	}
}

func TestRefreshMemoSkipsUnchangedFiles(t *testing.T) {
	home := t.TempDir()
	path := writeRollout(t, home, "2026/10/31", rolloutName(1), []string{
		claimLine("2026-10-31T10:00:00.000Z", 10.0),
	}, base)
	ix := New()
	first := ix.Refresh(home, Options{})
	if first.Status != StatusObserved || first.BytesRead == 0 {
		t.Fatalf("first = %+v", first)
	}
	second := ix.Refresh(home, Options{})
	if second.BytesRead != 0 {
		t.Fatalf("second refresh read %d bytes — an unchanged file must be memoized", second.BytesRead)
	}
	// Appending a record changes (size, mtime) → rescan picks it up.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(claimLine("2026-10-31T11:59:00.000Z", 77.0) + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := os.Chtimes(path, base.Add(time.Hour), base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	third := ix.Refresh(home, Options{})
	if third.Status != StatusObserved || third.Reading.Windows[0].UsedPercent != 77.0 {
		t.Fatalf("third = %+v, want the appended 77%% claim", third)
	}
}

func TestRefreshDeadlineReportsUnknown(t *testing.T) {
	home := t.TempDir()
	writeRollout(t, home, "2026/10/31", rolloutName(1), []string{
		claimLine("2026-10-31T10:00:00.000Z", 10.0),
	}, base)
	calls := 0
	now := func() time.Time {
		calls++
		if calls <= 1 {
			return base // the Refresh-start clock: deadline = base + 1m
		}
		return base.Add(time.Hour) // every check after start is "past"
	}
	got := newForTest(now).Refresh(home, Options{Deadline: time.Minute})
	if got.Status != StatusUnknown || got.Reason != "scan-bounded" {
		t.Fatalf("status = %v reason %q — a refresh past its deadline is unknown, not healthy", got.Status, got.Reason)
	}
}

// The per-file deadline check runs before each file — expiry while the last
// candidate is being read must still report unknown, not a completed verdict.
func TestRefreshDeadlineAfterLastScanIsUnknown(t *testing.T) {
	home := t.TempDir()
	writeRollout(t, home, "2026/10/31", rolloutName(1), []string{
		claimLine("2026-10-31T10:00:00.000Z", 10.0),
	}, base)
	calls := 0
	clock := func() time.Time {
		calls++
		if calls <= 5 {
			return base // start, three dir-descend checks, one scan-loop check
		}
		return base.Add(time.Hour)
	}
	got := newForTest(clock).Refresh(home, Options{Deadline: time.Minute})
	if got.Status != StatusUnknown || got.Reason != "scan-bounded" {
		t.Fatalf("status = %v reason %q — a refresh that finished over budget is scan-bounded", got.Status, got.Reason)
	}
}

func TestRefreshMaxFilesCapsCandidates(t *testing.T) {
	home := t.TempDir()
	for i := 0; i < 10; i++ {
		writeRollout(t, home, "2026/10/31", rolloutName(i+1), []string{
			claimLine(fmt.Sprintf("2026-10-31T10:%02d:00.000Z", i), float64(i)),
		}, base.Add(time.Duration(i)*time.Minute))
	}
	got := New().Refresh(home, Options{MaxFiles: 3})
	if got.Files != 3 {
		t.Fatalf("files = %d, want the 3 newest candidates", got.Files)
	}
	// Seven files were dropped unexamined — any of them can hold the newest
	// claim (record timestamps are not bounded by mtime), so a capped scan
	// reports unknown, not a stale observation.
	if got.Status != StatusUnknown || got.Reason != "scan-bounded" {
		t.Fatalf("got = %+v — a capped scan with dropped candidates must report unknown", got)
	}
}

// A store holding more rollouts than MaxFiles is not blanket-unknown (#6809):
// the candidates the cap dropped are bounded by the documented bound — a
// record can postdate its file's mtime, but not by more than the
// still-appending horizon — so drops whose ceilings cannot beat the newest
// found claim are proven irrelevant and the reading is observed.
func TestRefreshOverflowStoreStillObserved(t *testing.T) {
	home := t.TempDir()
	// 199 old rollouts: the cap keeps the 32 newest by mtime and drops the
	// rest; every dropped ceiling (2026-09-01 mtime + 30d) precedes the
	// newest file's claim.
	for i := 0; i < 199; i++ {
		writeRollout(t, home, "2026/09/01", rolloutName(i+1), []string{
			claimLine("2026-09-01T10:00:00.000Z", 5.0),
		}, mustTime(t, "2026-09-01T12:00:00.000Z"))
	}
	writeRollout(t, home, "2026/11/05", rolloutName(300), []string{
		claimLine("2026-11-05T11:00:00.000Z", 42.0),
	}, mustTime(t, "2026-11-05T12:00:00.000Z"))
	got := New().Refresh(home, Options{})
	if got.Status != StatusObserved || got.Reading.Windows[0].UsedPercent != 42.0 {
		t.Fatalf("got = %+v — dropped files that cannot hold a newer record must not sink the reading", got)
	}
}

// The other half of the bound: a dropped file whose mtime postdates the
// newest found claim could still hold a newer record — the refresh stays
// unknown rather than reporting a stale observation.
func TestRefreshDropNewerThanFoundClaimIsUnknown(t *testing.T) {
	home := t.TempDir()
	writeRollout(t, home, "2026/11/05", rolloutName(1), []string{
		claimLine("2026-10-01T10:00:00.000Z", 30.0), // stale claim, newest mtime
	}, mustTime(t, "2026-11-05T12:00:00.000Z"))
	writeRollout(t, home, "2026/11/04", rolloutName(2), []string{
		claimLine("2026-11-04T10:00:00.000Z", 99.0), // never read: loses the mtime race
	}, mustTime(t, "2026-11-04T12:00:00.000Z"))
	got := New().Refresh(home, Options{MaxFiles: 1})
	if got.Status != StatusUnknown || got.Reason != "scan-bounded" {
		t.Fatalf("status = %v reason %q — a dropped file whose mtime beats the found claim could hold the newest record", got.Status, got.Reason)
	}
}

// A file the MaxFiles cap drops is unexamined content — its records are not
// bounded by the mtime that lost the race — so the refresh reports unknown
// rather than a stale observation.
func TestRefreshMaxFilesDropCanHideNewestClaim(t *testing.T) {
	home := t.TempDir()
	writeRollout(t, home, "2026/10/31", rolloutName(1), []string{
		claimLine("2026-10-31T10:00:00.000Z", 5.0), // held: newest mtime, stale claim
	}, time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC))
	writeRollout(t, home, "2026/10/31", rolloutName(2), []string{
		claimLine("2026-10-31T11:30:00.000Z", 99.0), // dropped: older mtime, newer claim
	}, time.Date(2026, 10, 31, 11, 30, 0, 0, time.UTC))
	got := New().Refresh(home, Options{MaxFiles: 1})
	if got.Status != StatusUnknown || got.Reason != "scan-bounded" {
		t.Fatalf("status = %v reason %q — a dropped candidate that could hold the newest claim must not read as a stale 5%%", got.Status, got.Reason)
	}
}

// The per-file window is clamped to the remaining total budget: a file whose
// tail exceeds what is left is a bounded read, not a silent overshoot.
func TestRefreshTotalBudgetClampsPerFileWindow(t *testing.T) {
	home := t.TempDir()
	writeRollout(t, home, "2026/10/31", rolloutName(1), []string{
		claimLine("2026-10-31T10:00:00.000Z", 40.0),
		strings.Repeat(fillerLine+"\n", 30),
	}, base)
	got := New().Refresh(home, Options{MaxPerFileBytes: 1 << 20, MaxTotalBytes: 512})
	if got.Status != StatusUnknown || got.Reason != "scan-bounded" {
		t.Fatalf("status = %v reason %q — a window clamped below the eligible region is bounded", got.Status, got.Reason)
	}
	if got.BytesRead > 512 {
		t.Fatalf("BytesRead = %d exceeds the total budget", got.BytesRead)
	}
}

// When the tail cap leaves part of the eligible region unscanned, a claim in
// the scanned tail is still not the newest evidence — the unscanned prefix
// can hold a newer record timestamp (record order is not timestamp order).
func TestRefreshCappedTailWithClaimIsUnknown(t *testing.T) {
	home := t.TempDir()
	writeRollout(t, home, "2026/10/31", rolloutName(1), []string{
		claimLine("2026-10-31T11:00:00.000Z", 99.0), // below the cap: unscanned
		strings.Repeat(fillerLine+"\n", 30),
		claimLine("2026-10-31T10:00:00.000Z", 5.0), // inside the cap: found
	}, base)
	got := New().Refresh(home, Options{MaxPerFileBytes: 512})
	if got.Status != StatusUnknown || got.Reason != "scan-bounded" {
		t.Fatalf("status = %v reason %q — a claim found in a truncated region is not certified-newest", got.Status, got.Reason)
	}
}

// A malformed claim with a valid record timestamp orders by that timestamp:
// an old malformed record must not shadow the newer valid claims appended
// after it, which file-mtime ordering would do forever.
func TestRefreshMalformedClaimOrdersByRecordTimestamp(t *testing.T) {
	home := t.TempDir()
	writeRollout(t, home, "2026/10/31", rolloutName(1), []string{
		malformedClaimLine("2026-10-31T09:00:00.000Z"),
		claimLine("2026-10-31T11:00:00.000Z", 33.0),
	}, base)
	got := New().Refresh(home, Options{})
	if got.Status != StatusObserved || got.Reading.Windows[0].UsedPercent != 33.0 {
		t.Fatalf("got = %+v — the valid 11:00 claim must beat the malformed 09:00 one", got)
	}
}

// Filtered entries still advance the deadline cadence: a directory full of
// compressed rollouts must expire the scan rather than returning "none".
func TestRefreshDeadlineCountsFilteredEntries(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "10", "31")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("rollout-%03d.jsonl.zst", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	clock := func() time.Time {
		calls++
		if calls <= 4 {
			return base // refresh start + the three dir-descend checks
		}
		return base.Add(time.Hour)
	}
	got := newForTest(clock).Refresh(home, Options{Deadline: time.Minute})
	if got.Status != StatusUnknown || got.Reason != "scan-bounded" {
		t.Fatalf("status = %v reason %q — an all-filtered directory must still trip the deadline", got.Status, got.Reason)
	}
}

func TestRefreshIgnoresCompressedAndNonRolloutFiles(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "10", "31")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"rollout-a-00000000-0000-4000-8000-000000000001.jsonl.zst",
		"not-a-rollout.jsonl",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(claimLine("2026-10-31T11:00:00.000Z", 90.0)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := New().Refresh(home, Options{})
	if got.Status != StatusNone {
		t.Fatalf("status = %v — compressed/non-rollout files are not read", got.Status)
	}
}

func TestRefreshSymlinkedRolloutIsUnknown(t *testing.T) {
	home := t.TempDir()
	target := writeRollout(t, home, "2026/10/31", rolloutName(1), []string{
		claimLine("2026-10-31T11:00:00.000Z", 10.0),
	}, base)
	link := filepath.Join(filepath.Dir(target), rolloutName(2))
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	got := New().Refresh(home, Options{})
	if got.Status != StatusUnknown {
		t.Fatalf("status = %v — a symlinked rollout must be refused, not followed", got.Status)
	}
}

// The walk descends year/month/day directories newest-first, so a deadline
// truncation covers the newest part of the store and the early-stop bound
// ends enumeration cheaply. Here the newest day directory fills MaxFiles
// before the clock expires; the many older directories are all past the
// append horizon, so the walk stops — but their unexamined content is still
// reported, so the refresh is unknown, never a stale observation. Under the
// old ascending WalkDir order the same clock died in the oldest directories
// and reported unknown too; the descending order is a cost optimization, not
// a verdict difference.
func TestRefreshDescendsNewestDirsFirst(t *testing.T) {
	home := t.TempDir()
	now := mustTime(t, "2026-11-05T12:00:00.000Z")
	// Exactly MaxFiles files: the slate fills without a drop, so the only
	// skipped content is the early-stopped September remainder.
	for i := 0; i < 4; i++ {
		writeRollout(t, home, "2026/11/05", rolloutName(i+1), []string{
			claimLine("2026-11-05T11:00:00.000Z", 40.0),
		}, now)
	}
	for _, day := range []string{"01", "02", "03", "04", "05", "06", "07", "08", "09", "10"} {
		writeRollout(t, home, "2026/09/"+day, rolloutName(100), []string{
			claimLine("2026-09-05T10:00:00.000Z", 1.0),
		}, now.Add(-61*24*time.Hour))
	}
	calls := 0
	clock := func() time.Time {
		calls++
		// The descending walk reaches a horizon-bounded directory within a
		// handful of dir checks and stops before ever consulting an expired
		// clock; the old ascending order burned its checks on the September
		// directories first. The margin also covers the per-file scan loop,
		// which consults the clock once per candidate.
		if calls <= 10 {
			return now
		}
		return now.Add(time.Hour)
	}
	got := newForTest(clock).Refresh(home, Options{MaxFiles: 4, Deadline: time.Minute})
	if got.Status != StatusUnknown || got.Reason != "scan-bounded" {
		t.Fatalf("got = %+v — unexamined remainder dirs must report unknown, not a stale observation", got)
	}
}

// A file in yesterday's directory can hold today's mtime — its session kept
// appending across midnight — so the early-stop bound must not skip it: the
// bound is the append horizon relative to the weakest held mtime, not the
// directory's calendar age.
func TestRefreshYesterdayDirWithTodayMtimeIsFound(t *testing.T) {
	home := t.TempDir()
	now := mustTime(t, "2026-11-05T12:00:00.000Z")
	for i := 0; i < 5; i++ {
		writeRollout(t, home, "2026/11/05", rolloutName(i+1), []string{
			claimLine("2026-11-05T10:00:00.000Z", 30.0),
		}, now)
	}
	// Still being appended — its mtime is the newest in the store.
	writeRollout(t, home, "2026/11/04", rolloutName(50), []string{
		claimLine("2026-11-05T11:30:00.000Z", 88.0),
	}, now.Add(time.Minute))
	// MaxFiles covers all six files so no candidate is dropped.
	got := newForTest(func() time.Time { return now }).Refresh(home, Options{MaxFiles: 6})
	if got.Status != StatusObserved || got.Reading.Windows[0].UsedPercent != 88.0 {
		t.Fatalf("got = %+v — the still-appended yesterday-dir claim is the newest", got)
	}
}

// A file still being appended inside a directory older than the horizon is
// skipped without a stat once the slate is full — and its newer claim is in
// fact missed. What the bound certifies is only that skipping cannot change
// the mtime slate; the refresh still reports the directory as unexamined, so
// the miss surfaces as unknown rather than a lagged observation. The test
// pins the bound's exact failure shape so it is a reviewed choice, not an
// accident.
func TestRefreshHorizonBoundSkipsStillAppendedOldDir(t *testing.T) {
	home := t.TempDir()
	now := mustTime(t, "2026-11-05T12:00:00.000Z")
	// Exactly MaxFiles files: a full slate with no dropped candidate, so the
	// only skipped content is the bounded September remainder.
	for i := 0; i < 5; i++ {
		writeRollout(t, home, "2026/11/05", rolloutName(i+1), []string{
			claimLine("2026-11-05T10:00:00.000Z", 30.0),
		}, now)
	}
	// 51 days old: end of day + 30d horizon precedes the held floor.
	writeRollout(t, home, "2026/09/15", rolloutName(60), []string{
		claimLine("2026-11-05T11:45:00.000Z", 99.0),
	}, now)
	got := newForTest(func() time.Time { return now }).Refresh(home, Options{MaxFiles: 5})
	if got.Status != StatusUnknown || got.Reason != "scan-bounded" {
		t.Fatalf("got = %+v — a skipped directory's content is unexamined, so the miss must surface as unknown", got)
	}
}

// The bound only certifies a remainder when the slate is FULL: with fewer
// than MaxFiles candidates, old directories are still walked — a stale claim
// beats nothing.
func TestRefreshUnfilledSlateStillWalksOldDirs(t *testing.T) {
	home := t.TempDir()
	now := mustTime(t, "2026-11-05T12:00:00.000Z")
	writeRollout(t, home, "2026/11/05", rolloutName(1), []string{
		claimLine("2026-11-05T10:00:00.000Z", 30.0),
	}, now)
	writeRollout(t, home, "2026/09/15", rolloutName(60), []string{
		claimLine("2026-11-05T11:45:00.000Z", 71.0),
	}, now)
	got := newForTest(func() time.Time { return now }).Refresh(home, Options{MaxFiles: 4})
	if got.Status != StatusObserved || got.Reading.Windows[0].UsedPercent != 71.0 {
		t.Fatalf("got = %+v — an unfilled slate must keep walking into old dirs", got)
	}
}

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
