package tmux

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The record #6339 adds fields to — daemon_pid, prior_sockets, and the
// "adopted" mode — crosses binary versions in both directions: a user who
// downgrades af (or runs an older `af doctor` against a newer daemon's home)
// meets a record written by this schema, and a user upgrading meets one
// written before these fields existed. The decode must be compatible in both
// directions — readJSONFile unmarshals without DisallowUnknownFields, and
// these tests pin that property so a later "tidier" decoder cannot quietly
// break the downgrade path.

// serverStatePre6339 is the record shape as every af build before #6339 wrote
// and read it. Decoding a NEW record into it is what an old binary does.
type serverStatePre6339 struct {
	Mode   string    `json:"mode"`
	Socket string    `json:"socket"`
	Unit   string    `json:"unit"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

// TestServerState_OldBinaryReadsNewRecord writes a record through the current
// schema — daemon_pid stamp, prior_sockets history, adopted mode — and decodes
// it with the pre-#6339 field set, exactly what an older af's json.Unmarshal
// sees. The unknown fields must be ignored, not fail the decode, and every
// field the old schema knows must still answer: the socket an old doctor pins
// its reads to, and the mode/unit it reports.
func TestServerState_OldBinaryReadsNewRecord(t *testing.T) {
	home := t.TempDir()
	sock := filepath.Join(t.TempDir(), "prior-sock")
	prior := filepath.Join(t.TempDir(), "older-sock")
	if err := os.WriteFile(prior, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	serverStateMu.Lock()
	lastServerState = ServerState{}
	serverStateMu.Unlock()
	recordServerState(home, ServerState{
		Mode:             ServerMode("adopted"),
		Socket:           sock,
		PriorSockets:     []string{prior},
		HistoryTruncated: true,
		At:               time.Now(),
	})

	data, err := os.ReadFile(filepath.Join(home, serverStateFileName))
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	var old serverStatePre6339
	if err := json.Unmarshal(data, &old); err != nil {
		t.Fatalf("a pre-#6339 decoder rejected the new record — the decode must "+
			"ignore fields it does not know (daemon_pid, prior_sockets): %v", err)
	}
	if old.Socket != sock || old.Mode != "adopted" {
		t.Fatalf("old-schema decode lost the fields it knows: socket=%q mode=%q",
			old.Socket, old.Mode)
	}

	// And the new record still reads through the new schema, priors and all.
	st, ok, err := ReadServerState(home)
	if err != nil || !ok {
		t.Fatalf("new-schema read of its own record: ok=%v err=%v", ok, err)
	}
	// recordServerState always stamps the WRITER's pid — os.Getpid() here —
	// over whatever a caller supplies, which is itself part of the contract.
	if st.DaemonPID != os.Getpid() || len(st.PriorSockets) != 1 || st.PriorSockets[0] != prior ||
		!st.HistoryTruncated {
		t.Fatalf("new-schema read dropped the generation fields: %+v", st)
	}
}

// TestServerState_NewBinaryReadsOldRecord writes a record in the pre-#6339
// shape — no daemon_pid, no prior_sockets — and reads it with the current
// schema. The daemon must keep working exactly as it did before the fields
// existed (the next recordServerState stamps the current generation and
// carries the old socket into history), while doctor merely reads a record it
// cannot prove current — the cautious side of the contract.
func TestServerState_NewBinaryReadsOldRecord(t *testing.T) {
	home := t.TempDir()
	sock := filepath.Join(t.TempDir(), "old-sock")
	next := filepath.Join(t.TempDir(), "next-sock")
	// Marshal the pre-#6339 shape itself — the bytes an old binary wrote.
	oldRecord, err := json.Marshal(serverStatePre6339{
		Mode: "dedicated", Socket: sock, Unit: "agent-factory-tmux-server", At: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, serverStateFileName), oldRecord, 0o600); err != nil {
		t.Fatal(err)
	}

	st, ok, err := ReadServerState(home)
	if err != nil || !ok {
		t.Fatalf("new-schema read of a pre-#6339 record: ok=%v err=%v", ok, err)
	}
	if st.Socket != sock || st.Mode != ServerMode("dedicated") {
		t.Fatalf("old record lost the fields the old schema wrote: %+v", st)
	}
	if st.DaemonPID != 0 || len(st.PriorSockets) != 0 {
		t.Fatalf("old record must decode with an empty generation binding, got %+v", st)
	}

	// The daemon works exactly as it did: the next recordServerState (the
	// write every spawn path already makes) succeeds, gains the stamp, and
	// carries the socket it moved off into history — if that socket is still
	// reachable.
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	serverStateMu.Lock()
	lastServerState = ServerState{}
	serverStateMu.Unlock()
	recordServerState(home, ServerState{Mode: ServerMode("dedicated"), Socket: next, At: time.Now()})
	st, ok, err = ReadServerState(home)
	if err != nil || !ok {
		t.Fatalf("record rewrite over an old record: ok=%v err=%v", ok, err)
	}
	if st.DaemonPID != os.Getpid() {
		t.Fatalf("rewrite over an old record stamped daemon_pid = %d, want %d",
			st.DaemonPID, os.Getpid())
	}
	if len(st.PriorSockets) != 1 || st.PriorSockets[0] != sock {
		t.Fatalf("rewrite over an old record lost the socket it moved off: %+v", st.PriorSockets)
	}
}
