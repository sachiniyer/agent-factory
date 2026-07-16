package daemon

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"testing"
)

// Task provenance is an assertion the daemon makes about its own delivery, never
// a claim a client gets to make (#1892). It rides the gob control socket — which
// is how the daemon's own task-delivery loopback carries it — but the HTTP/JSON
// plane, reachable over TCP with a token since #1592, must not be able to set it.
//
// Without that boundary, anyone who can reach the API could create an ordinary
// session tagged with a capped watch task's id; countTaskRunsLocked counts by
// TaskID, so that unrelated session would eat the task's slots and park its
// events.

// TestCreateSessionRequestRejectsTaskProvenanceFromJSON pins that a JSON body
// cannot set task provenance or the cap, no matter what it claims.
func TestCreateSessionRequestRejectsTaskProvenanceFromJSON(t *testing.T) {
	body := []byte(`{
		"title": "totally-normal-session",
		"repo_path": "/tmp/repo",
		"program": "claude",
		"task_id": "capped01",
		"max_concurrent_runs": 99
	}`)

	var req CreateSessionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if req.TaskID != "" {
		t.Fatalf("a JSON client set task_id to %q; it could then consume that task's concurrency slots", req.TaskID)
	}
	if req.MaxConcurrentRuns != 0 {
		t.Fatalf("a JSON client set max_concurrent_runs to %d; the cap is the daemon's to supply from the task record", req.MaxConcurrentRuns)
	}
	// The rest of the request still decodes: only provenance is off-limits.
	if req.Title != "totally-normal-session" {
		t.Fatalf("title = %q, want the request to decode normally otherwise", req.Title)
	}
}

// TestCreateSessionRequestKeepsTaskProvenanceOverGob is the other half: the
// daemon's own delivery loopback goes over the net/rpc GOB control socket, so the
// fields must still survive THAT round-trip. If they did not, every task-created
// session would lose its provenance and the cap would count nothing.
func TestCreateSessionRequestKeepsTaskProvenanceOverGob(t *testing.T) {
	req := CreateSessionRequest{
		TitleBase:         "dlq-triage",
		RepoPath:          "/tmp/repo",
		Program:           "claude",
		TaskID:            "capped01",
		MaxConcurrentRuns: 3,
	}

	back := gobRoundTrip(t, req)
	if back.TaskID != "capped01" {
		t.Fatalf("task_id = %q after the gob round-trip, want it preserved; task deliveries carry provenance this way", back.TaskID)
	}
	if back.MaxConcurrentRuns != 3 {
		t.Fatalf("max_concurrent_runs = %d after the gob round-trip, want 3", back.MaxConcurrentRuns)
	}
}

// gobRoundTrip encodes and decodes a request exactly as the net/rpc control
// socket does.
func gobRoundTrip(t *testing.T, req CreateSessionRequest) CreateSessionRequest {
	t.Helper()
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(req); err != nil {
		t.Fatalf("gob encode: %v", err)
	}
	var back CreateSessionRequest
	if err := gob.NewDecoder(&buf).Decode(&back); err != nil {
		t.Fatalf("gob decode: %v", err)
	}
	return back
}
