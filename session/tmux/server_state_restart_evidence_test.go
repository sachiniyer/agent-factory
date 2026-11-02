package tmux

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// These tests pin the cross-restart arm of recordServerState's switch over the
// previous record (#6610). #6609 carries the socket lineage forward when the
// record file vanishes under a LIVE daemon — the process still holds its last
// written record in lastServerState. A restarted process does not: the record
// file is gone AND lastServerState is zeroed, so the first write of a new
// daemon generation falls through with nothing to carry. That write used to
// claim {Socket: S2, PriorSockets: [], HistoryTruncated: false} — a complete,
// empty history stamped with the live daemon's pid, which doctor's
// record-only death proof spends on a SIGKILL against sessions that survived
// the restart on the first generation's socket.
//
// The property these tests pin: a record may claim a complete socket history
// only when the AF home shows NO earlier daemon generation. The home keeps
// the evidence the deleted record can no longer speak for — persisted
// instance rows (sessions ran here on some tmux socket), pending-create
// markers (a session possibly stranded live before its row committed), and
// the server-death marker (written only by an af-owned tmux server's wrapper
// at exit). Any of it means the lineage's socket history is unrecoverable, so
// the write admits HistoryTruncated and doctor reports rather than kills. A
// genuinely fresh home shows none of it, and its first record's empty history
// is honestly complete.

// writeRepoInstancesFile plants a per-repo instances.json under the home's
// instances store — the persisted residue an earlier generation's session
// leaves behind.
func writeRepoInstancesFile(t *testing.T, home, repoID, payload string) {
	t.Helper()
	dir := filepath.Join(home, "instances", repoID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create instances repo dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "instances.json"), []byte(payload), 0o600); err != nil {
		t.Fatalf("write instances.json: %v", err)
	}
}

// writePendingCreateMarker plants one pending-create journal entry — the
// durable record of a session create that may have stranded live without ever
// committing an instance row (#6301).
func writePendingCreateMarker(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, "pending-creates")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create pending-creates dir: %v", err)
	}
	marker := `{"instance_id":"9ce7d04f-6f0d-4e1a-9b0a-1c1e2d3f4a5b","session_name":"af_x_stranded","program":"claude"}`
	if err := os.WriteFile(filepath.Join(dir, "9ce7d04f-6f0d-4e1a-9b0a-1c1e2d3f4a5b.json"), []byte(marker), 0o600); err != nil {
		t.Fatalf("write pending-create marker: %v", err)
	}
}

// writeServerDeathMarker plants the tmux-server-death.json an af-owned
// dedicated server's wrapper leaves when tmux exits (#5429).
func writeServerDeathMarker(t *testing.T, home, socket string) {
	t.Helper()
	death := ServerDeath{
		At:            time.Now().Add(-time.Hour),
		Unit:          "agent-factory-tmux-server-prev",
		Socket:        socket,
		ExitCode:      1,
		UptimeSeconds: 60,
	}
	if err := writeJSONFile(filepath.Join(home, serverDeathFileName), death); err != nil {
		t.Fatalf("write server-death marker: %v", err)
	}
}

// readBackTruncation reads the record and fails unless it was written with
// HistoryTruncated set — the admission the record-only death proof refuses to
// spend on a kill.
func readBackTruncation(t *testing.T, home string) ServerState {
	t.Helper()
	st, ok, err := ReadServerState(home)
	if err != nil || !ok {
		t.Fatalf("read rewritten record: ok=%v err=%v", ok, err)
	}
	if !st.HistoryTruncated {
		t.Fatalf("record claims complete socket history (history_truncated=false, %+v) — "+
			"a home that shows an earlier daemon generation must admit the lineage it cannot "+
			"reconstruct, or doctor reads a session live on a dropped socket as dead (#6610)", st)
	}
	return st
}

// TestRecordServerState_RestartDeletedRecord_AdmitsTruncatedHistory is the
// core #6610 regression: generation 1 records socket gen1 and persists a
// session row during its lifetime; the record file is deleted BETWEEN
// generations; generation 2 — a fresh process, lastServerState zeroed —
// records socket gen2. It must not write the falsely complete, empty history
// the fall-through used to produce.
func TestRecordServerState_RestartDeletedRecord_AdmitsTruncatedHistory(t *testing.T) {
	home := t.TempDir()
	sockDir := t.TempDir()
	resetLastServerState()

	gen1 := materializeSocket(t, sockDir, "gen1")
	gen2 := materializeSocket(t, sockDir, "gen2")

	// Generation 1: a genuinely fresh first record — complete, empty history —
	// then the generation's lifetime persists a session row under the home.
	recordServerState(home, ServerState{Mode: ServerModeDedicated, Socket: gen1, At: time.Now()})
	st, ok, err := ReadServerState(home)
	if err != nil || !ok {
		t.Fatalf("read gen1 record: ok=%v err=%v", ok, err)
	}
	if st.HistoryTruncated {
		t.Fatal("a fresh home's first record must claim complete history")
	}
	writeRepoInstancesFile(t, home, "a1b2c3d4",
		`{"schema_version":1,"instances":[{"id":"i1","title":"old-session","tmux_name":"af_a1b2_old-session"}]}`)

	// The record is deleted BETWEEN generations; a restarted process has no
	// lineage in memory to carry forward.
	removeRecord(t, home)
	resetLastServerState()

	// Generation 2 on a different socket: the write must admit truncated
	// history rather than claim the complete, empty one.
	recordServerState(home, ServerState{Mode: ServerModeDedicated, Socket: gen2, At: time.Now()})
	st = readBackTruncation(t, home)
	if st.Socket != gen2 {
		t.Fatalf("recorded socket = %q, want the writing generation's socket %q", st.Socket, gen2)
	}
	if st.DaemonPID != os.Getpid() {
		t.Fatalf("record daemon_pid = %d, want the writing generation's pid %d", st.DaemonPID, os.Getpid())
	}
}

// TestRecordServerState_RestartDeletedRecord_EvidenceSources pins each shape
// of prior-generation residue independently: every one must force the
// truncated admission on its own, because any one can be the only evidence a
// lineage leaves behind.
func TestRecordServerState_RestartDeletedRecord_EvidenceSources(t *testing.T) {
	cases := []struct {
		name  string
		plant func(t *testing.T, home, priorSocket string)
	}{
		{"persisted instance row", func(t *testing.T, home, _ string) {
			writeRepoInstancesFile(t, home, "a1b2c3d4",
				`{"schema_version":1,"instances":[{"id":"i1","title":"s","tmux_name":"af_a1b2_s"}]}`)
		}},
		{"instance rows in a second repo", func(t *testing.T, home, _ string) {
			writeRepoInstancesFile(t, home, "e5f6a7b8",
				`{"schema_version":1,"instances":[{"id":"i1"},{"id":"i2"}]}`)
		}},
		{"pending-create marker", func(t *testing.T, home, _ string) {
			writePendingCreateMarker(t, home)
		}},
		{"server-death marker", func(t *testing.T, home, prior string) {
			writeServerDeathMarker(t, home, prior)
		}},
		{"unreadable instances.json", func(t *testing.T, home, _ string) {
			writeRepoInstancesFile(t, home, "a1b2c3d4", "{not json")
		}},
		{"corrupt server-death marker", func(t *testing.T, home, _ string) {
			if err := os.WriteFile(filepath.Join(home, serverDeathFileName), []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			sockDir := t.TempDir()
			resetLastServerState()

			gen1 := materializeSocket(t, sockDir, "gen1")
			gen2 := materializeSocket(t, sockDir, "gen2")

			// Generation 1's record, then the residue its lifetime leaves,
			// then the record deleted between generations.
			recordServerState(home, ServerState{Mode: ServerModeDedicated, Socket: gen1, At: time.Now()})
			tc.plant(t, home, gen1)
			removeRecord(t, home)
			resetLastServerState()

			recordServerState(home, ServerState{Mode: ServerModeDedicated, Socket: gen2, At: time.Now()})
			readBackTruncation(t, home)
		})
	}
}

// TestRecordServerState_RestartDeletedRecord_NoEvidence_WritesCompleteHistory
// is the fresh-home control the issue demands: a home that shows NO earlier
// daemon generation — no instance rows, no pending creates, no death marker —
// still writes a complete, empty history on a first-ever start. The admission
// must never fire merely because time passed or because the home holds
// unrelated af state.
func TestRecordServerState_RestartDeletedRecord_NoEvidence_WritesCompleteHistory(t *testing.T) {
	cases := []struct {
		name  string
		plant func(t *testing.T, home string)
	}{
		{"genuinely fresh home", func(t *testing.T, home string) {}},
		{"unrelated af state only", func(t *testing.T, home string) {
			// daemon.pid, state.json, config — an af process ran here, but no
			// session or af-owned server generation is recorded.
			for _, name := range []string{"daemon.pid", "state.json", "config.toml", "agent-factory.log"} {
				if err := os.WriteFile(filepath.Join(home, name), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{"instances store with no rows", func(t *testing.T, home string) {
			writeRepoInstancesFile(t, home, "a1b2c3d4", `{"schema_version":1,"instances":[]}`)
			writeRepoInstancesFile(t, home, "e5f6a7b8", `{"schema_version":1,"instances":[]}`)
		}},
		{"repo dir without instances.json", func(t *testing.T, home string) {
			if err := os.MkdirAll(filepath.Join(home, "instances", "a1b2c3d4", "artifacts"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"empty pending-creates dir", func(t *testing.T, home string) {
			if err := os.MkdirAll(filepath.Join(home, "pending-creates"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			resetLastServerState()
			tc.plant(t, home)

			sock := materializeSocket(t, t.TempDir(), "sock")
			recordServerState(home, ServerState{Mode: ServerModeDedicated, Socket: sock, At: time.Now()})

			st, ok, err := ReadServerState(home)
			if err != nil || !ok {
				t.Fatalf("read first record: ok=%v err=%v", ok, err)
			}
			if st.HistoryTruncated {
				t.Fatal("a home with no prior-generation evidence must claim complete history — " +
					"a first-ever start must not be marked truncated (#6610)")
			}
			if len(st.PriorSockets) != 0 {
				t.Fatalf("a first-ever record has no prior sockets: got %v", st.PriorSockets)
			}
		})
	}
}

// TestRecordServerState_RestartTruncation_SticksAcrossGenerations pins that
// the admission, once written, is inherited by every later record — the next
// generation's ordinary carry-forward keeps it set (HistoryTruncated never
// clears), so the kill arm stays holstered for as long as the lineage cannot
// account for the dropped socket.
func TestRecordServerState_RestartTruncation_SticksAcrossGenerations(t *testing.T) {
	home := t.TempDir()
	sockDir := t.TempDir()
	resetLastServerState()

	gen1 := materializeSocket(t, sockDir, "gen1")
	gen2 := materializeSocket(t, sockDir, "gen2")
	gen3 := materializeSocket(t, sockDir, "gen3")

	recordServerState(home, ServerState{Mode: ServerModeDedicated, Socket: gen1, At: time.Now()})
	writeRepoInstancesFile(t, home, "a1b2c3d4",
		`{"schema_version":1,"instances":[{"id":"i1","tmux_name":"af_a1b2_s"}]}`)
	removeRecord(t, home)
	resetLastServerState()
	recordServerState(home, ServerState{Mode: ServerModeDedicated, Socket: gen2, At: time.Now()})
	readBackTruncation(t, home)

	// A THIRD generation (record present this time — the ok arm) must keep
	// the admission: truncation never clears.
	resetLastServerState()
	recordServerState(home, ServerState{Mode: ServerModeDedicated, Socket: gen3, At: time.Now()})
	readBackTruncation(t, home)
}

// TestRecordServerState_RestartDeletedRecord_LiveProcessCarryForwardUnchanged
// guards the ordering of the absent-file arms: when THIS process still holds
// its lineage (lastServerState non-zero — the #6609 arm), the carry-forward
// wins over the evidence admission. A home can show earlier generations while
// this process still knows every socket it has used.
func TestRecordServerState_RestartDeletedRecord_LiveProcessCarryForwardUnchanged(t *testing.T) {
	home := t.TempDir()
	sockDir := t.TempDir()
	resetLastServerState()

	gen1 := materializeSocket(t, sockDir, "gen1")
	gen2 := materializeSocket(t, sockDir, "gen2")
	gen3 := materializeSocket(t, sockDir, "gen3")

	// This process built its own lineage: gen1 -> gen2 in memory.
	recordServerState(home, ServerState{Mode: ServerModeDedicated, Socket: gen1, At: time.Now()})
	recordServerState(home, ServerState{Mode: ServerModeDedicated, Socket: gen2, At: time.Now()})

	// Evidence of earlier generations exists alongside — but the record file
	// vanished under THIS live process, whose memory is the better lineage.
	writeRepoInstancesFile(t, home, "a1b2c3d4",
		`{"schema_version":1,"instances":[{"id":"i1","tmux_name":"af_a1b2_s"}]}`)
	removeRecord(t, home)

	recordServerState(home, ServerState{Mode: ServerModeAdopted, Socket: gen3, At: time.Now()})
	st, ok, err := ReadServerState(home)
	if err != nil || !ok {
		t.Fatalf("read rewritten record: ok=%v err=%v", ok, err)
	}
	want := []string{gen1, gen2}
	if len(st.PriorSockets) != len(want) || st.PriorSockets[0] != want[0] || st.PriorSockets[1] != want[1] {
		t.Fatalf("the in-memory carry-forward must win over the evidence admission: prior_sockets=%v, want %v",
			st.PriorSockets, want)
	}
	if st.HistoryTruncated {
		t.Fatal("a fully-accounted in-memory lineage is not truncated")
	}
}
