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

// A zero record one cut sees survives the worst-case merge even when the
// other cut lacks it — presence is part of the record, not only its size.
func TestLoadSnapshotKeepsAZeroRecordFromEitherCut(t *testing.T) {
	states := []State{
		{CarriedPrefixes: map[string]int64{"/r.jsonl": 0}},
		{},
	}
	si := 0
	snap, stateErr, railsErr := loadSnapshot(
		func() (State, error) { s := states[si]; si++; return s, nil },
		func() (Rails, error) { return Rails{}, nil })
	if stateErr != nil || railsErr != nil {
		t.Fatalf("errs: %v %v", stateErr, railsErr)
	}
	if n, recorded := snap.CarriedBoundary("/r.jsonl"); n != 0 || !recorded {
		t.Fatalf("CarriedBoundary = (%d, %v), want (0, true): the merge dropped a zero record", n, recorded)
	}
}
