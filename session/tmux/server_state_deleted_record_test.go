package tmux

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// These tests pin the absent-file arm of recordServerState's switch over the
// previous record (#6339). ReadServerState returns a tri-state: (ok=true,
// err=nil) when the record exists and decoded; (ok=false, err=nil) when the
// record file does not exist; (ok=false, err!=nil) when the record could not
// be read. The err arm admits incomplete coverage (HistoryTruncated=true) and
// the ok arm carries the prior socket lineage forward, but the absent-file arm
// — (ok=false, err=nil) — silently fell through and wrote a record that
// claimed complete, empty history, dropping every socket generation the daemon
// had used. When the record file is deleted while the daemon stays live (a
// state-dir cleanup, a lost file), the daemon still holds its last written
// record in lastServerState; the fix carries that lineage forward rather than
// the false claim. A genuinely fresh lineage (zero lastServerState, no file)
// is a true first record with trivially complete history and falls through
// unchanged.

// materializeSocket creates an empty file at a path a prior socket generation
// can be proven reachable through — the write path never probes socket
// reachability, but the paths are realistic.
func materializeSocket(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("materialize socket %s: %v", path, err)
	}
	return path
}

// resetLastServerState clears the in-memory lastServerState under the mutex so
// a test starts from a known lineage.
func resetLastServerState() {
	serverStateMu.Lock()
	lastServerState = ServerState{}
	serverStateMu.Unlock()
}

func removeRecord(t *testing.T, home string) {
	t.Helper()
	if err := os.Remove(filepath.Join(home, serverStateFileName)); err != nil {
		t.Fatalf("remove %s: %v", serverStateFileName, err)
	}
}

// TestRecordServerState_DeletedRecordCarriesInMemoryLineage is the core fix
// test: after the daemon builds a multi-generation socket history, the record
// file is deleted while the process stays live, and a later rewrite (a decision
// change that defeats the dedupe) must carry the lineage forward from
// lastServerState rather than write a falsely complete, empty-history record
// stamped with the live daemon's pid (#6339). Before the fix, the absent-file
// fall-through dropped every prior socket and wrote a record doctor's
// record-only death proof read as current and complete — arming a SIGKILL
// against a process whose session was live on a dropped prior socket.
func TestRecordServerState_DeletedRecordCarriesInMemoryLineage(t *testing.T) {
	home := t.TempDir()
	sockDir := t.TempDir()
	resetLastServerState()

	gen1 := materializeSocket(t, sockDir, "gen1")
	gen2 := materializeSocket(t, sockDir, "gen2")
	gen3 := materializeSocket(t, sockDir, "gen3")

	// Build a two-generation history: gen1 -> gen2, so the record carries
	// prior_sockets=[gen1] and lastServerState holds the same lineage.
	recordServerState(home, ServerState{Mode: ServerModeDedicated, Socket: gen1, At: time.Now()})
	recordServerState(home, ServerState{Mode: ServerModeDedicated, Socket: gen2, At: time.Now()})
	st, ok, err := ReadServerState(home)
	if err != nil || !ok {
		t.Fatalf("read record after gen2: ok=%v err=%v", ok, err)
	}
	if len(st.PriorSockets) != 1 || st.PriorSockets[0] != gen1 {
		t.Fatalf("after gen2, prior_sockets=%v, want [%s]", st.PriorSockets, gen1)
	}

	// Delete the record file while the daemon process stays live.
	removeRecord(t, home)

	// A decision change (mode flip dedicated -> adopted) defeats the dedupe
	// and forces a rewrite — the production paths recordAdoptedServer,
	// dedicatedServerReady (on relaunch after server death), and the
	// fallback record all trigger this.
	recordServerState(home, ServerState{Mode: ServerModeAdopted, Socket: gen3, At: time.Now()})

	st, ok, err = ReadServerState(home)
	if err != nil || !ok {
		t.Fatalf("read rewritten record: ok=%v err=%v", ok, err)
	}
	// The rewritten record must carry the lineage forward: gen1 and gen2
	// are both prior sockets now (gen2 moved into history when the daemon
	// moved to gen3). Before the fix, priors was empty and the record
	// falsely claimed complete history — arming doctor's record-only kill.
	want := []string{gen1, gen2}
	if len(st.PriorSockets) != len(want) {
		t.Fatalf("after deleted-record rewrite, prior_sockets=%v, want %v — "+
			"the in-memory lineage must carry forward rather than claim complete empty history (#6339)",
			st.PriorSockets, want)
	}
	for i := range want {
		if st.PriorSockets[i] != want[i] {
			t.Fatalf("after deleted-record rewrite, prior_sockets[%d]=%q, want %q",
				i, st.PriorSockets[i], want[i])
		}
	}
	if st.HistoryTruncated {
		t.Fatal("nothing was capped — history_truncated must stay false after the carry-forward")
	}
	if st.DaemonPID != os.Getpid() {
		t.Fatalf("rewritten record daemon_pid=%d, want the live generation's pid %d",
			st.DaemonPID, os.Getpid())
	}
	if st.Mode != ServerModeAdopted {
		t.Fatalf("rewritten record mode=%q, want %q", st.Mode, ServerModeAdopted)
	}
	if st.Socket != gen3 {
		t.Fatalf("rewritten record socket=%q, want %q", st.Socket, gen3)
	}
}

// TestRecordServerState_DeletedRecordPreservesTruncation verifies that a
// truncated history (set by the cap) stays truncated after a deleted-record
// rewrite — the dropped generation may still host sessions, so the admission
// must never clear, and the in-memory carry-forward inherits it.
func TestRecordServerState_DeletedRecordPreservesTruncation(t *testing.T) {
	home := t.TempDir()
	sockDir := t.TempDir()
	resetLastServerState()

	// Build a history that exceeds the cap, setting HistoryTruncated.
	total := maxPriorSocketGenerations + 3
	for i := 0; i < total; i++ {
		recordServerState(home, ServerState{
			Mode:   ServerModeDedicated,
			Socket: materializeSocket(t, sockDir, fmt.Sprintf("gen%d", i)),
			At:     time.Now(),
		})
	}
	recordServerState(home, ServerState{
		Mode:   ServerModeDedicated,
		Socket: materializeSocket(t, sockDir, "final"),
		At:     time.Now(),
	})
	st, _, _ := ReadServerState(home)
	if !st.HistoryTruncated {
		t.Fatal("cap eviction must set history_truncated before deletion")
	}

	// Delete the file and rewrite with a decision change.
	removeRecord(t, home)
	recordServerState(home, ServerState{
		Mode:   ServerModeAdopted,
		Socket: materializeSocket(t, sockDir, "next"),
		At:     time.Now(),
	})

	st, ok, err := ReadServerState(home)
	if err != nil || !ok {
		t.Fatalf("read rewritten record: ok=%v err=%v", ok, err)
	}
	if !st.HistoryTruncated {
		t.Fatal("a truncated history must stay truncated after a deleted-record rewrite — " +
			"the dropped generation may still host sessions (#6339)")
	}
}

// TestRecordServerState_DeletedRecordDedupeStillSuppresses verifies the dedupe
// still suppresses the write after file deletion when the decision is unchanged
// — the fallback path runs on every spawn, and an unchanged decision must not
// rewrite even with the file absent and priors in memory.
func TestRecordServerState_DeletedRecordDedupeStillSuppresses(t *testing.T) {
	home := t.TempDir()
	sockDir := t.TempDir()
	resetLastServerState()

	gen1 := materializeSocket(t, sockDir, "gen1")
	gen2 := materializeSocket(t, sockDir, "gen2")

	recordServerState(home, ServerState{Mode: ServerModeDedicated, Socket: gen1, At: time.Now()})
	recordServerState(home, ServerState{Mode: ServerModeDedicated, Socket: gen2, At: time.Now()})

	removeRecord(t, home)

	// Identical decision (same mode, unit, socket, reason) — the dedupe
	// must suppress the write even with the file absent.
	recordServerState(home, ServerState{Mode: ServerModeDedicated, Socket: gen2, At: time.Now()})

	if _, ok, _ := ReadServerState(home); ok {
		t.Fatal("an unchanged decision was rewritten after file deletion — the dedupe must suppress it")
	}
}

// TestRecordServerState_FreshProcessNoRecordWritesCompleteHistory verifies
// that a genuinely fresh lineage (zero lastServerState, no record file) writes
// a complete, empty-history record — the true first-record case. The fix
// carries the lineage forward only when lastServerState holds one; a fresh
// process has nothing to carry and writes trivially complete history.
func TestRecordServerState_FreshProcessNoRecordWritesCompleteHistory(t *testing.T) {
	home := t.TempDir()
	resetLastServerState()

	sock := filepath.Join(t.TempDir(), "fresh-sock")
	recordServerState(home, ServerState{Mode: ServerModeDedicated, Socket: sock, At: time.Now()})

	st, ok, err := ReadServerState(home)
	if err != nil || !ok {
		t.Fatalf("read first record: ok=%v err=%v", ok, err)
	}
	if len(st.PriorSockets) != 0 {
		t.Fatalf("a first-ever record has no prior sockets: got %v", st.PriorSockets)
	}
	if st.HistoryTruncated {
		t.Fatal("a first-ever record has trivially complete history — history_truncated must be false")
	}
	if st.DaemonPID != os.Getpid() {
		t.Fatalf("first record daemon_pid=%d, want %d", st.DaemonPID, os.Getpid())
	}
}
