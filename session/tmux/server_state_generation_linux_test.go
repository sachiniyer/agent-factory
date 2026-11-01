//go:build linux

package tmux

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/systemdunit"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// These tests pin the daemon half of #6339: the record doctor pins its reads
// to must be provably written by the CURRENT daemon generation and must
// account for every socket the daemon lineage has used. Before the fix, a
// restart that adopted an already-running server returned from
// ensureDedicatedServer without rewriting the record — stamped with nothing
// and still naming a socket the restarted daemon may no longer use — and a
// restart under a different TMUX_TMPDIR recorded the new socket while the old
// server and its sessions stayed alive with no record of that socket at all.
//
// The records are read back as raw JSON so the daemon_pid and prior_sockets
// fields are asserted on the on-disk bytes, the contract doctor reads — a
// reader that predates the fields simply does not see them, which is exactly
// the stale-record shape this fix eliminates.

// serverStateRecord is the on-disk shape of tmux-server-state.json, decoded
// field by field so a build that predates daemon_pid/prior_sockets simply
// reads zero values for them — which is itself the assertion these tests make.
type serverStateRecord struct {
	Mode             string   `json:"mode"`
	Socket           string   `json:"socket"`
	Unit             string   `json:"unit"`
	DaemonPID        int      `json:"daemon_pid"`
	PriorSockets     []string `json:"prior_sockets"`
	HistoryTruncated bool     `json:"history_truncated"`
}

func readServerStateRecord(t *testing.T, home string) serverStateRecord {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, serverStateFileName))
	if err != nil {
		t.Fatalf("read %s: %v — the daemon must record its tmux server decision so doctor can pin to it",
			serverStateFileName, err)
	}
	var rec serverStateRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("parse %s: %v", serverStateFileName, err)
	}
	return rec
}

// TestEnsureDaemonServer_AdoptWritesCurrentSocketRecord is the adopt half of
// #6339: when ensureDedicatedServer finds a tmux server already running on its
// socket it must still write the record — stamped with this daemon's pid —
// rather than return early and leave whatever a previous generation wrote.
// Before the fix the record simply did not exist here: doctor pinned its
// reads to a stale or absent socket and read a live session's absence as
// death.
func TestEnsureDaemonServer_AdoptWritesCurrentSocketRecord(t *testing.T) {
	testguard.IsolateTmux(t)

	// A foreign/legacy tmux server already running on the ambient socket —
	// exactly the server a restarted daemon adopts.
	out, err := exec.Command("tmux", "new-session", "-d", "-s", "preexisting", "sleep", "60").CombinedOutput()
	if err != nil {
		t.Fatalf("start preexisting tmux server: %v: %s", err, out)
	}

	home := t.TempDir()
	t.Setenv(systemdunit.DaemonMarkerEnv, systemdunit.DaemonUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))
	restore := ConfigureDaemonServer(home)
	t.Cleanup(restore)

	if err := EnsureDaemonServer(); err != nil {
		t.Fatalf("ensure daemon server over an adoptable server: %v", err)
	}

	rec := readServerStateRecord(t, home)
	if rec.Socket != DefaultSocketPath() {
		t.Fatalf("adopted record socket = %q, want the socket this daemon resolved (%q)",
			rec.Socket, DefaultSocketPath())
	}
	if rec.DaemonPID != os.Getpid() {
		t.Fatalf("adopted record daemon_pid = %d, want the writing generation's pid %d — "+
			"unstamped records cannot be proven current (#6339)", rec.DaemonPID, os.Getpid())
	}
	// "adopted" literal rather than the ServerMode constant: this file must
	// compile against a build that predates the mode.
	if rec.Mode != "adopted" {
		t.Fatalf("adopted record mode = %q, want %q — the server was not launched by af",
			rec.Mode, "adopted")
	}
}

// TestEnsureDaemonServer_AdoptPreservesDedicatedOnSameSocket is the continuity
// half: when the already-running server sits on the SAME socket the previous
// generation dedicated, the adopt path must keep the dedicated claim (the
// running server is almost certainly that dedicated server) while refreshing
// the generation stamp. Before the fix the early return left the old record
// untouched — stamped with nothing, so a later doctor could not tell it from
// a stale one.
func TestEnsureDaemonServer_AdoptPreservesDedicatedOnSameSocket(t *testing.T) {
	testguard.IsolateTmux(t)
	home := t.TempDir()

	// The previous generation's record: a dedicated server on this same
	// socket. Written through recordServerState so the shape is exactly what
	// the daemon leaves behind.
	serverStateMu.Lock()
	lastServerState = ServerState{}
	serverStateMu.Unlock()
	recordServerState(home, ServerState{
		Mode:   ServerMode("dedicated"),
		Socket: DefaultSocketPath(),
		Unit:   "agent-factory-tmux-server-prev",
		At:     time.Now(),
	})

	// A server (the surviving dedicated one, or an adopted equivalent) runs
	// on that socket when the new generation's ensure runs.
	out, err := exec.Command("tmux", "new-session", "-d", "-s", "preexisting", "sleep", "60").CombinedOutput()
	if err != nil {
		t.Fatalf("start preexisting tmux server: %v: %s", err, out)
	}
	t.Setenv(systemdunit.DaemonMarkerEnv, systemdunit.DaemonUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))
	restore := ConfigureDaemonServer(home)
	t.Cleanup(restore)

	if err := EnsureDaemonServer(); err != nil {
		t.Fatalf("ensure daemon server over an adoptable server: %v", err)
	}

	rec := readServerStateRecord(t, home)
	if rec.Mode != "dedicated" {
		t.Fatalf("same-socket adopt record mode = %q, want the preserved %q", rec.Mode, "dedicated")
	}
	if rec.Unit != "agent-factory-tmux-server-prev" {
		t.Fatalf("same-socket adopt record unit = %q, want the preserved unit", rec.Unit)
	}
	// The record's socket must be the one the daemon's sessions actually run
	// on — the ambient socket both generations resolved — so doctor's pinned
	// reads look at the same server master did. Only the stamp is new.
	if rec.Socket != DefaultSocketPath() {
		t.Fatalf("same-socket adopt record socket = %q, want the unchanged ambient socket %q",
			rec.Socket, DefaultSocketPath())
	}
	if rec.DaemonPID != os.Getpid() {
		t.Fatalf("same-socket adopt record daemon_pid = %d, want the refreshed generation stamp %d",
			rec.DaemonPID, os.Getpid())
	}
}

// socketForTest materializes a path a prior socket generation can be proven
// reachable through: the liveness prune drops entries whose socket file is
// gone, so history tests need a real file standing in for each live server.
func socketForTest(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("materialize stand-in socket %s: %v", path, err)
	}
	return path
}

// TestRecordServerState_RetainsPriorSocketGenerations is the TMUX_TMPDIR half
// of #6339: when the daemon records a new socket, the record must retain the
// previous socket in prior_sockets — the old server and its sessions may still
// be alive there, and doctor can only read a session's absence as death once
// every socket generation is accounted for.
func TestRecordServerState_RetainsPriorSocketGenerations(t *testing.T) {
	home := t.TempDir()
	sockDir := t.TempDir()
	serverStateMu.Lock()
	lastServerState = ServerState{}
	serverStateMu.Unlock()

	gen1 := socketForTest(t, sockDir, "gen1")
	gen2 := socketForTest(t, sockDir, "gen2")
	gen3 := socketForTest(t, sockDir, "gen3")

	recordServerState(home, ServerState{Mode: ServerMode("dedicated"), Socket: gen1, At: time.Now()})
	recordServerState(home, ServerState{Mode: ServerMode("dedicated"), Socket: gen2, At: time.Now()})
	rec := readServerStateRecord(t, home)
	if got, want := rec.PriorSockets, []string{gen1}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("prior_sockets after first move = %v, want %v", got, want)
	}

	recordServerState(home, ServerState{Mode: ServerMode("dedicated"), Socket: gen3, At: time.Now()})
	rec = readServerStateRecord(t, home)
	want := []string{gen1, gen2}
	if len(rec.PriorSockets) != len(want) || rec.PriorSockets[0] != want[0] || rec.PriorSockets[1] != want[1] {
		t.Fatalf("prior_sockets after second move = %v, want %v", rec.PriorSockets, want)
	}
	if rec.DaemonPID != os.Getpid() {
		t.Fatalf("record daemon_pid = %d, want the writing generation's pid %d", rec.DaemonPID, os.Getpid())
	}

	// Rewriting the SAME socket carries the history forward unchanged — no
	// growth, no loss.
	recordServerState(home, ServerState{Mode: ServerMode("dedicated"), Socket: gen3, At: time.Now(), Unit: "u"})
	rec = readServerStateRecord(t, home)
	if len(rec.PriorSockets) != len(want) || rec.PriorSockets[0] != want[0] || rec.PriorSockets[1] != want[1] {
		t.Fatalf("prior_sockets after same-socket rewrite = %v, want %v", rec.PriorSockets, want)
	}
}

// TestRecordServerState_PriorSocketsDropsUnreachableGeneration pins one half
// of the history bound: a prior socket whose file is gone can never be
// connected to again — a tmux server owns its socket file for its whole life —
// so keeping it would guarantee a permanently unlistable generation blinding
// every doctor run.
func TestRecordServerState_PriorSocketsDropsUnreachableGeneration(t *testing.T) {
	home := t.TempDir()
	sockDir := t.TempDir()
	serverStateMu.Lock()
	lastServerState = ServerState{}
	serverStateMu.Unlock()

	dead := socketForTest(t, sockDir, "gone")
	live := socketForTest(t, sockDir, "live")
	current := socketForTest(t, sockDir, "current")

	recordServerState(home, ServerState{Mode: ServerMode("dedicated"), Socket: dead, At: time.Now()})
	if err := os.Remove(dead); err != nil {
		t.Fatalf("retire stand-in socket %s: %v", dead, err)
	}
	recordServerState(home, ServerState{Mode: ServerMode("dedicated"), Socket: live, At: time.Now()})
	recordServerState(home, ServerState{Mode: ServerMode("dedicated"), Socket: current, At: time.Now()})

	rec := readServerStateRecord(t, home)
	want := []string{live}
	if len(rec.PriorSockets) != len(want) || rec.PriorSockets[0] != want[0] {
		t.Fatalf("prior_sockets with an unreachable generation = %v, want only the reachable %v",
			rec.PriorSockets, want)
	}
	if rec.HistoryTruncated {
		t.Fatal("dropping an unreachable socket loses no coverage — history_truncated must stay false")
	}
}

// TestRecordServerState_PriorSocketsCapped pins the other half of the bound:
// however many socket generations a lineage accumulates, the record keeps at
// most maxPriorSocketGenerations of them — the newest, since those are the
// sockets a leftover server is most likely still answering on.
func TestRecordServerState_PriorSocketsCapped(t *testing.T) {
	home := t.TempDir()
	sockDir := t.TempDir()
	serverStateMu.Lock()
	lastServerState = ServerState{}
	serverStateMu.Unlock()

	total := maxPriorSocketGenerations + 3
	sockets := make([]string, 0, total)
	for i := 0; i < total; i++ {
		sockets = append(sockets, socketForTest(t, sockDir, "gen"+strconv.Itoa(i)))
	}
	for _, sock := range sockets {
		recordServerState(home, ServerState{Mode: ServerMode("dedicated"), Socket: sock, At: time.Now()})
	}
	recordServerState(home, ServerState{Mode: ServerMode("dedicated"), Socket: socketForTest(t, sockDir, "final"), At: time.Now()})

	rec := readServerStateRecord(t, home)
	if len(rec.PriorSockets) != maxPriorSocketGenerations {
		t.Fatalf("prior_sockets length = %d, want the bound %d", len(rec.PriorSockets), maxPriorSocketGenerations)
	}
	want := sockets[len(sockets)-maxPriorSocketGenerations:]
	for i := range want {
		if rec.PriorSockets[i] != want[i] {
			t.Fatalf("prior_sockets[%d] = %q, want %q — the bound keeps the newest generations",
				i, rec.PriorSockets[i], want[i])
		}
	}
	// Dropping a still-reachable generation is an admission the record can no
	// longer account for every socket the daemon has used: the record must SAY
	// so, and once said the admission never clears — the dropped server may
	// host sessions forever.
	if !rec.HistoryTruncated {
		t.Fatal("cap eviction dropped a still-reachable generation — history_truncated must be set")
	}
	recordServerState(home, ServerState{Mode: ServerMode("dedicated"), Socket: socketForTest(t, sockDir, "later"), At: time.Now()})
	rec2 := readServerStateRecord(t, home)
	if !rec2.HistoryTruncated {
		t.Fatal("a truncated history stays truncated on every later record")
	}
}
