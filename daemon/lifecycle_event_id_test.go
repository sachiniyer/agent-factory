package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
)

// drainNextSessionEvent waits for the next session.* event on ch and decodes its
// InstanceData payload, failing the test if none arrives in time.
func drainNextSessionEvent(t *testing.T, ch <-chan agentproto.Event, want agentproto.EventType) session.InstanceData {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type != want {
				continue // skip unrelated events (e.g. session.updated from a poll)
			}
			var data session.InstanceData
			if len(ev.Data) > 0 {
				if err := json.Unmarshal(ev.Data, &data); err != nil {
					t.Fatalf("unmarshal %s payload: %v", want, err)
				}
			}
			return data
		case <-deadline:
			t.Fatalf("no %s event published within the deadline", want)
			return session.InstanceData{}
		}
	}
}

// TestKillEventCarriesStableID pins the #1592 Phase 5 PR5 structural fix: the
// delete-class lifecycle events (killed here) must carry the session's stable id,
// not only its title, so a client keys its rail by the id that disambiguates
// cross-repo title collisions. Before the fix the KillSession handler published
// session.InstanceData{Title: req.Title} with an empty ID.
func TestKillEventCarriesStableID(t *testing.T) {
	manager, repo, data := createRealKillSession(t, "kill-id-evt")
	if data.ID == "" {
		t.Fatalf("created session has no stable id: %+v", data)
	}
	cs := &controlServer{manager: manager}

	_, ch := manager.events.subscribe()
	var resp KillSessionResponse
	if err := cs.KillSession(KillSessionRequest{Title: "kill-id-evt", RepoID: repo.ID}, &resp); err != nil {
		t.Fatalf("KillSession: %v", err)
	}

	got := drainNextSessionEvent(t, ch, agentproto.EventSessionKilled)
	if got.ID != data.ID {
		t.Fatalf("killed event ID = %q, want %q (the stable id, not the title)", got.ID, data.ID)
	}
	if got.Title != "kill-id-evt" {
		t.Fatalf("killed event Title = %q, want %q", got.Title, "kill-id-evt")
	}
}

// TestArchiveAndRestoreEventsCarryStableID pins that archive and restore also
// stamp the stable id onto their events — archive resolving it BEFORE the
// relocation and restore resolving it AFTER the session is re-registered.
func TestArchiveAndRestoreEventsCarryStableID(t *testing.T) {
	manager, repo, data := createRealKillSession(t, "arch-id-evt")
	if data.ID == "" {
		t.Fatalf("created session has no stable id: %+v", data)
	}
	cs := &controlServer{manager: manager}

	_, ch := manager.events.subscribe()

	var aResp ArchiveSessionResponse
	if err := cs.ArchiveSession(ArchiveSessionRequest{Title: "arch-id-evt", RepoID: repo.ID}, &aResp); err != nil {
		t.Fatalf("ArchiveSession: %v", err)
	}
	archived := drainNextSessionEvent(t, ch, agentproto.EventSessionArchived)
	if archived.ID != data.ID {
		t.Fatalf("archived event ID = %q, want %q", archived.ID, data.ID)
	}

	var rResp RestoreArchivedResponse
	if err := cs.RestoreArchived(RestoreArchivedRequest{ID: data.ID, Title: "stale-display-title", RepoID: repo.ID}, &rResp); err != nil {
		t.Fatalf("RestoreArchived: %v", err)
	}
	restored := drainNextSessionEvent(t, ch, agentproto.EventSessionRestored)
	if restored.ID != data.ID {
		t.Fatalf("restored event ID = %q, want %q", restored.ID, data.ID)
	}
	if restored.Title != "arch-id-evt" {
		t.Fatalf("restored event Title = %q, want canonical %q", restored.Title, "arch-id-evt")
	}
}
