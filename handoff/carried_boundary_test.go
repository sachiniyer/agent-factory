package handoff

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A carry that copied zero bytes is a record: af knows nothing in the file
// is the source account's. Only a path with no entry anywhere is
// unrecorded, and a committed re-run covers the whole file only then
// (#7270).
func TestCarriedBoundaryTellsZeroFromUnrecorded(t *testing.T) {
	marked := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name         string
		snap         Snapshot
		want         int64
		wantRecorded bool
	}{
		{name: "no entry anywhere", snap: Snapshot{}, want: 0, wantRecorded: false},
		{
			name: "an explicit zero watermark",
			snap: Snapshot{State: State{CarriedPrefixes: map[string]int64{"/r.jsonl": 0}}},
			want: 0, wantRecorded: true,
		},
		{
			name: "a watermark",
			snap: Snapshot{State: State{CarriedPrefixes: map[string]int64{"/r.jsonl": 12}}},
			want: 12, wantRecorded: true,
		},
		{
			name: "another file's watermark",
			snap: Snapshot{State: State{CarriedPrefixes: map[string]int64{"/other.jsonl": 12}}},
			want: 0, wantRecorded: false,
		},
		{
			name: "a quarantine marker and no watermark",
			snap: Snapshot{Rails: Rails{UnrecordedCarries: map[string]UnrecordedCarry{
				"/r.jsonl": {At: marked, Covered: 40}}}},
			want: 40, wantRecorded: true,
		},
		{
			name: "a marker wider than the watermark",
			snap: Snapshot{
				State: State{CarriedPrefixes: map[string]int64{"/r.jsonl": 12}},
				Rails: Rails{UnrecordedCarries: map[string]UnrecordedCarry{"/r.jsonl": {At: marked, Covered: 40}}},
			},
			want: 40, wantRecorded: true,
		},
		{
			name: "a legacy marker with no boundary",
			snap: Snapshot{Rails: Rails{UnrecordedCarries: map[string]UnrecordedCarry{
				"/r.jsonl": {At: marked, Covered: math.MaxInt64}}}},
			want: math.MaxInt64, wantRecorded: true,
		},
		{
			name: "an account overflow flag naming the file",
			snap: Snapshot{Rails: Rails{QuarantinedAccounts: map[string]AccountQuarantine{
				"/home": {At: marked, Path: "/r.jsonl", Covered: 30}}}},
			want: 30, wantRecorded: true,
		},
		{
			name: "an account overflow flag that names no single file",
			snap: Snapshot{Rails: Rails{QuarantinedAccounts: map[string]AccountQuarantine{
				"/home": {At: marked}}}},
			want: 0, wantRecorded: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, recorded := tc.snap.CarriedBoundary("/r.jsonl")
			if got != tc.want || recorded != tc.wantRecorded {
				t.Fatalf("CarriedBoundary = (%d, %v), want (%d, %v)", got, recorded, tc.want, tc.wantRecorded)
			}
		})
	}
}

// The explicit zero entry is durable: it is written, survives a later
// read-modify-write, and decodes as present — while an older file with no
// entry still decodes as unrecorded.
func TestRecordCarriedPrefixKeepsAnExplicitZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instances", StateFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	store := NewStore(path)
	if err := store.RecordCarriedPrefix("/r.jsonl", 0); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordCarriedPrefix("/other.jsonl", 7); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields struct {
		Prefixes map[string]json.RawMessage `json:"carried_prefix_len"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if got, ok := fields.Prefixes["/r.jsonl"]; !ok || string(got) != "0" {
		t.Fatalf("the zero record is not on disk: %s", raw)
	}
	snap := Snapshot{State: mustLoad(t, store)}
	if n, recorded := snap.CarriedBoundary("/r.jsonl"); n != 0 || !recorded {
		t.Fatalf("CarriedBoundary = (%d, %v) after a zero record, want (0, true)", n, recorded)
	}
	if _, recorded := snap.CarriedBoundary("/never.jsonl"); recorded {
		t.Fatal("a path af never recorded reads as recorded")
	}
}

// The merge is worst-case for presence too. A positive watermark only the
// first cut saw is kept: it excludes more. A zero only the first cut saw is
// dropped: a zero record trusts the whole file to the target, so absence —
// a whole-file cover on a re-run, Claude's whole-file fallback — is the more
// conservative reading (#7277). No writer deletes an entry, so only an
// outside delete between the reads gets here.
func TestLoadSnapshotDropsAZeroRecordOnlyTheFirstCutSaw(t *testing.T) {
	states := []State{
		{CarriedPrefixes: map[string]int64{"/zero.jsonl": 0, "/twelve.jsonl": 12}},
		{},
	}
	si := 0
	snap, stateErr, railsErr := loadSnapshot(
		func() (State, error) { s := states[si]; si++; return s, nil },
		func() (Rails, error) { return Rails{}, nil })
	if stateErr != nil || railsErr != nil {
		t.Fatalf("errs: %v %v", stateErr, railsErr)
	}
	if n, recorded := snap.CarriedBoundary("/zero.jsonl"); recorded {
		t.Fatalf("CarriedBoundary = (%d, true): a zero only the stale cut saw must not trust the file", n)
	}
	if n, recorded := snap.CarriedBoundary("/twelve.jsonl"); n != 12 || !recorded {
		t.Fatalf("CarriedBoundary = (%d, %v), want (12, true): the wider span either cut saw stands", n, recorded)
	}
}

// An account overflow flag that names no single file says more than one
// carry into that home went unrecorded, and af cannot tell which files took
// them. No record of a file under that home is complete, so a committed
// re-run must cover it whole rather than narrow to an older watermark
// (#7277, probe F). The flag of another home, or one naming another file,
// leaves the record alone.
func TestCarriedBoundaryUnderAnUntrackableFlagIsUnrecorded(t *testing.T) {
	const path = "/af/accounts/codex/work/sessions/r.jsonl"
	marked := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	state := State{CarriedPrefixes: map[string]int64{path: 12}}
	for _, tc := range []struct {
		name         string
		flags        map[string]AccountQuarantine
		want         int64
		wantRecorded bool
	}{
		{name: "its home's untrackable flag", want: 0, wantRecorded: false,
			flags: map[string]AccountQuarantine{"/af/accounts/codex/work": {At: marked}}},
		{name: "another home's untrackable flag", want: 12, wantRecorded: true,
			flags: map[string]AccountQuarantine{"/af/accounts/codex/other": {At: marked}}},
		{name: "a flag on a home whose name is a prefix of its home's", want: 12, wantRecorded: true,
			flags: map[string]AccountQuarantine{"/af/accounts/codex/wor": {At: marked}}},
		{name: "its home's flag naming another file", want: 12, wantRecorded: true,
			flags: map[string]AccountQuarantine{"/af/accounts/codex/work": {
				At: marked, Path: "/af/accounts/codex/work/sessions/other.jsonl", Covered: 30}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := Snapshot{State: state, Rails: Rails{QuarantinedAccounts: tc.flags}}
			got, recorded := snap.CarriedBoundary(path)
			if got != tc.want || recorded != tc.wantRecorded {
				t.Fatalf("CarriedBoundary = (%d, %v), want (%d, %v)", got, recorded, tc.want, tc.wantRecorded)
			}
		})
	}
}
