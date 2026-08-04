package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
)

// The multi-client half of agent handoff (#2013 / #2782). A handoff rewrites the
// session's agent program, which is the single most visible fact about a row —
// the rail renders it, and the handoff picker EXCLUDES it when offering targets.
// Every other session mutation announces itself on the events plane; this one did
// not, so a second web window kept offering to hand off to the agent the session
// had just been handed off to.
//
// These assert on the published EVENTS. A Snapshot assertion would pass with
// nothing published at all — which is exactly the state these exist to fail on —
// and the status poll cannot substitute: it skips a session with an operation in
// flight, and afterwards takes the already-final state as its own baseline.

// drainSessionUpdates collects every session.updated already queued for ch.
//
// The handoff verbs publish synchronously, so by the time the call under test
// returns, its whole sequence is sitting in the subscriber's buffer. Reading it
// all (rather than the first event) is what lets a test say something about the
// ORDER of the announcements, and keeps an unrelated poll event from being
// mistaken for the one under test.
func drainSessionUpdates(t *testing.T, ch <-chan agentproto.Event) []session.InstanceData {
	t.Helper()
	var out []session.InstanceData
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				// The hub closes an evicted subscriber's channel on buffer overflow.
				return out
			}
			if ev.Type != agentproto.EventSessionUpdated {
				continue
			}
			var data session.InstanceData
			if err := json.Unmarshal(ev.Data, &data); err != nil {
				t.Fatalf("unmarshal session.updated payload: %v", err)
			}
			out = append(out, data)
		default:
			return out
		}
	}
}

// firstUpdateWhere returns the index of the first collected update satisfying
// pred, or -1. Tests match on SHAPE rather than position so an unrelated poll
// publish cannot shift them.
func firstUpdateWhere(updates []session.InstanceData, pred func(session.InstanceData) bool) int {
	for i := range updates {
		if pred(updates[i]) {
			return i
		}
	}
	return -1
}

// TestHandoffSession_PublishesSwapThenSettlement covers the interactive path, and
// pins BOTH announcements plus their order.
//
// The mid-flight one is not a nicety: the readiness wait can run for a minute, and
// it carries the replacement fence, which is what closes the other clients' action
// gates (CanHandoff/CanKill project off it). Announcing the durable checkpoint
// there instead would tell them the swap had settled and re-open gates the daemon
// is still holding shut.
//
// The settlement one is what lowers that fence again. Without it a second window
// would sit on a permanently "working" row.
func TestHandoffSession_PublishesSwapThenSettlement(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &handoffBackend{FakeBackend: session.NewFakeBackend()}
	const title = "handoff-events"
	registerHandoffSubject(t, manager, repoID, repoPath, title, backend)

	id, ch := manager.events.subscribe()
	defer manager.events.unsubscribe(id)

	if _, err := manager.HandoffSession(HandoffSessionRequest{
		Title: title, RepoID: repoID, To: tmux.ProgramGemini,
	}); err != nil {
		t.Fatalf("HandoffSession: %v", err)
	}

	updates := drainSessionUpdates(t, ch)
	if len(updates) == 0 {
		t.Fatal("handoff published no session.updated: every other client keeps rendering the OUTGOING agent, " +
			"and the handoff picker keeps excluding the wrong one")
	}

	inFlight := firstUpdateWhere(updates, func(d session.InstanceData) bool {
		return d.Program == tmux.ProgramGemini && d.InFlightOp == session.OpReplacing
	})
	if inFlight < 0 {
		t.Fatalf("no session.updated announced the swap while the replacement was still in flight; got %s",
			describeUpdates(updates))
	}
	settled := firstUpdateWhere(updates, func(d session.InstanceData) bool {
		return d.Program == tmux.ProgramGemini && d.InFlightOp == session.OpNone
	})
	if settled < 0 {
		t.Fatalf("no session.updated lowered the replacement fence; other clients stay stuck on a working row: got %s",
			describeUpdates(updates))
	}
	if settled < inFlight {
		t.Fatalf("settlement was announced before the swap (%d < %d): session.updated replaces a client's WHOLE "+
			"projection, so the older payload would win: got %s", settled, inFlight, describeUpdates(updates))
	}

	// Every announcement must name the session it is about, or a client cannot
	// route it to a row.
	for i, u := range updates {
		if u.Title != title {
			t.Fatalf("update %d carried session %q, want %q", i, u.Title, title)
		}
	}
}

// TestHandoffSession_UndeliveredMissionStillAnnouncesTheSwap is the case the
// checkpoint publish exists for. A post-ready paste failure leaves the mission
// pending behind the fence and NEVER settles inside this call — the recovery loop
// owns it from here. The agent program has still changed, and the requester learns
// that from the RPC error path; everyone else has only the events plane.
func TestHandoffSession_UndeliveredMissionStillAnnouncesTheSwap(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	sendErr := errors.New("paste transport failed")
	backend := &handoffBackend{FakeBackend: session.NewFakeBackend(), sendErr: sendErr}
	const title = "handoff-undelivered"
	inst := registerHandoffSubject(t, manager, repoID, repoPath, title, backend)

	id, ch := manager.events.subscribe()
	defer manager.events.unsubscribe(id)

	_, err := manager.HandoffSession(HandoffSessionRequest{
		Title: title, RepoID: repoID, To: tmux.ProgramGemini,
	})
	if !errors.Is(err, task.ErrPromptDelivery) {
		t.Fatalf("HandoffSession error = %v, want a post-ready prompt-delivery failure", err)
	}
	if got := inst.GetInFlightOp(); got != session.OpReplacing {
		t.Fatalf("op = %v, want the fence still raised for the retry path", got)
	}

	updates := drainSessionUpdates(t, ch)
	if firstUpdateWhere(updates, func(d session.InstanceData) bool {
		return d.Program == tmux.ProgramGemini && d.InFlightOp == session.OpReplacing
	}) < 0 {
		t.Fatalf("an undelivered handoff announced nothing: the pane already runs the incoming agent, "+
			"so every other client is now wrong about which agent this session is: got %s", describeUpdates(updates))
	}
}

// TestResumePendingHandoffs_PublishesSettlement covers the recovery loop, where
// the events plane matters MORE than on the interactive path: no client asked for
// this work, so not even the window that started the handoff has a reason to
// re-Snapshot afterwards.
func TestResumePendingHandoffs_PublishesSettlement(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	sendErr := errors.New("paste transport failed")
	backend := &handoffBackend{FakeBackend: session.NewFakeBackend(), sendErr: sendErr}
	const title = "handoff-recovered"
	inst := registerHandoffSubject(t, manager, repoID, repoPath, title, backend)

	if _, err := manager.HandoffSession(HandoffSessionRequest{
		Title: title, RepoID: repoID, To: tmux.ProgramGemini,
	}); !errors.Is(err, task.ErrPromptDelivery) {
		t.Fatalf("HandoffSession error = %v, want a post-ready prompt-delivery failure", err)
	}

	// Subscribe AFTER the failed handoff so only the recovery's own announcement
	// can satisfy the assertion.
	id, ch := manager.events.subscribe()
	defer manager.events.unsubscribe(id)

	backend.setSendErr(nil)
	manager.ResumePendingHandoffs()

	if got := inst.GetInFlightOp(); got != session.OpNone {
		t.Fatalf("op = %v, want the fence lowered after a delivered recovery mission", got)
	}
	updates := drainSessionUpdates(t, ch)
	if firstUpdateWhere(updates, func(d session.InstanceData) bool {
		return d.Program == tmux.ProgramGemini && d.InFlightOp == session.OpNone
	}) < 0 {
		t.Fatalf("handoff recovery settled silently: the row stays working on every open rail until something "+
			"unrelated republishes it: got %s", describeUpdates(updates))
	}
}

// describeUpdates renders the collected payloads in the two axes these tests turn
// on, so a failure names what WAS published instead of only what was not.
func describeUpdates(updates []session.InstanceData) string {
	if len(updates) == 0 {
		return "no session.updated events"
	}
	out := ""
	for i, u := range updates {
		if i > 0 {
			out += ", "
		}
		out += u.Program + "/" + opLabelForTest(u.InFlightOp)
	}
	return "[" + out + "]"
}

func opLabelForTest(op session.InFlightOp) string {
	switch op {
	case session.OpNone:
		return "none"
	case session.OpReplacing:
		return "replacing"
	default:
		return fmt.Sprintf("op(%d)", int(op))
	}
}
