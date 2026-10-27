package daemon

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/proposal"
	"github.com/sachiniyer/agent-factory/session"

	"github.com/stretchr/testify/require"
)

func nextPushEvent(t *testing.T, ch <-chan agentproto.Event, state proposal.PushState) proposal.PushStatus {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type != agentproto.EventProposalPush {
				continue
			}
			var st proposal.PushStatus
			require.NoError(t, json.Unmarshal(ev.Data, &st))
			if st.State == state {
				return st
			}
		case <-deadline:
			t.Fatalf("no proposal.push event with state %s", state)
		}
	}
}

func startPushSession(t *testing.T, title string) (*Manager, *controlServer, session.InstanceData) {
	t.Helper()
	manager, _, data := createRealKillSession(t, title)
	cs := &controlServer{manager: manager}
	code, env := postProposalRoute(t, cs, "StartProposal", StartProposalRequest{SessionID: data.ID, Slug: "p", Docs: []string{"goals"}})
	require.Equal(t, http.StatusOK, code, env)
	return manager, cs, data
}

func humanSave(t *testing.T, cs *controlServer, id, text string) {
	t.Helper()
	code, env := postProposalRoute(t, cs, "ReadProposalDoc", ReadProposalDocRequest{SessionID: id, Slug: "p", Doc: "goals"})
	require.Equal(t, http.StatusOK, code, env)
	base := env["data"].(map[string]any)["commit"].(string)
	code, env = postProposalRoute(t, cs, "SaveProposalDoc", SaveProposalDocRequest{SessionID: id, Slug: "p", Doc: "goals", Text: text, Base: base})
	require.Equal(t, http.StatusOK, code, env)
}

func TestProposalPush_ModePersistsOnTheDocsTab(t *testing.T) {
	manager, cs, data := startPushSession(t, "pushmode")

	code, env := postProposalRoute(t, cs, "SetProposalPushMode", SetProposalPushModeRequest{SessionID: data.ID, Mode: "manual"})
	require.Equal(t, http.StatusOK, code, env)
	st := env["data"].(map[string]any)
	require.Equal(t, "manual", st["mode"])
	require.Equal(t, "p", st["slug"])
	require.Equal(t, data.ID, st["session_id"])
	require.Equal(t, "idle", st["state"])
	require.Nil(t, st["due_at"])
	require.Contains(t, st, "due_in_ms")
	require.Nil(t, st["due_in_ms"])

	tabs := docsTabsOf(t, manager, data.ID)
	require.Len(t, tabs, 1)
	require.Equal(t, "manual", tabs[0].PushMode)

	code, env = postProposalRoute(t, cs, "SetProposalPushMode", SetProposalPushModeRequest{SessionID: data.ID, Mode: "sometimes"})
	require.Equal(t, http.StatusInternalServerError, code)
	require.Contains(t, env["error"].(map[string]any)["message"], "use auto or manual")

	code, env = postProposalRoute(t, cs, "GetProposal", GetProposalRequest{SessionID: data.ID, Slug: "p"})
	require.Equal(t, http.StatusOK, code, env)
	require.Equal(t, "manual", env["data"].(map[string]any)["push"].(map[string]any)["mode"])
}

func TestProposalPush_GetProposalWithoutASessionHasNoPush(t *testing.T) {
	manager, repo, data := createRealKillSession(t, "pushget")
	cs := &controlServer{manager: manager}
	code, env := postProposalRoute(t, cs, "StartProposal", StartProposalRequest{SessionID: data.ID, Slug: "q", Docs: []string{"goals"}})
	require.Equal(t, http.StatusOK, code, env)
	code, env = postProposalRoute(t, cs, "GetProposal", GetProposalRequest{RepoID: repo.ID, Slug: "q"})
	require.Equal(t, http.StatusOK, code, env)
	d := env["data"].(map[string]any)
	require.Contains(t, d, "push")
	require.Nil(t, d["push"])
}

func TestProposalPush_HoldAndSendNowWhileBusy(t *testing.T) {
	manager, cs, data := startPushSession(t, "pushhold")
	instance, _, _, _, _, err := manager.resolveActionSession(data.ID, "", "")
	require.NoError(t, err)
	instance.SetStatusForTest(session.Running)
	manager.pushProposals()

	id, ch, _ := manager.events.subscribeWithOverflow()
	defer manager.events.unsubscribe(id)

	humanSave(t, cs, data.ID, "human edit\n")
	st := nextPushEvent(t, ch, proposal.PushCountdown)
	require.Equal(t, 1, st.Unsent)
	require.NotNil(t, st.DueAt)
	require.NotNil(t, st.DueInMs)
	require.LessOrEqual(t, *st.DueInMs, int64(proposal.PushQuiet/time.Millisecond))

	code, env := postProposalRoute(t, cs, "HoldProposalPush", HoldProposalPushRequest{SessionID: data.ID})
	require.Equal(t, http.StatusOK, code, env)
	require.Equal(t, "held", env["data"].(map[string]any)["state"])
	nextPushEvent(t, ch, proposal.PushHeld)

	code, env = postProposalRoute(t, cs, "SendProposalChanges", SendProposalChangesRequest{SessionID: data.ID})
	require.Equal(t, http.StatusOK, code, env)
	reply := env["data"].(map[string]any)
	require.Equal(t, "waiting_agent", reply["state"], "a busy agent queues send now")
	require.EqualValues(t, 1, reply["unsent"])

	code, env = postProposalRoute(t, cs, "PullProposalChanges", PullProposalChangesRequest{SessionID: data.ID})
	require.Equal(t, http.StatusOK, code, env)
	require.Len(t, env["data"].(map[string]any)["edits"], 1, "a queued send must not move the cursor")
}

func TestProposalPush_RoutesNeedAnAttachedSession(t *testing.T) {
	manager, _, data := createRealKillSession(t, "pushdetached")
	cs := &controlServer{manager: manager}
	for _, verb := range []string{"HoldProposalPush", "SendProposalChanges"} {
		code, env := postProposalRoute(t, cs, verb, map[string]string{"session_id": data.ID})
		require.Equal(t, http.StatusInternalServerError, code, verb)
		require.Contains(t, env["error"].(map[string]any)["message"], "not attached to a proposal", verb)
	}
	code, env := postProposalRoute(t, cs, "SetProposalPushMode", SetProposalPushModeRequest{SessionID: data.ID, Mode: "auto"})
	require.Equal(t, http.StatusInternalServerError, code)
	require.Contains(t, env["error"].(map[string]any)["message"], "not attached to a proposal")
	code, env = postProposalRoute(t, cs, "SendProposalChanges", SendProposalChangesRequest{})
	require.Equal(t, http.StatusInternalServerError, code)
	require.Contains(t, env["error"].(map[string]any)["message"], "session_id is required")
}

func TestProposalPush_DeliveryToADeadSessionFails(t *testing.T) {
	manager, _, data := createRealKillSession(t, "pushdead")
	instance, _, _, _, _, err := manager.resolveActionSession(data.ID, "", "")
	require.NoError(t, err)
	instance.SetStatusForTest(session.Dead)
	delivery, err := proposalHost{m: manager}.SendPrompt(data.ID, "Proposal p: the user edited goals.md.")
	require.Error(t, err)
	require.Equal(t, proposal.DeliveryFailed, delivery, "a prompt that never reached the pane must rewind the cursor")
}
