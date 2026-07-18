package daemon

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// registerHandoffSubject is registerStarted plus the agent tab a handoff needs:
// the ledger and the conversation slot both live on Tabs[0], so an instance
// without one has nothing to hand off. SetTmuxSession materializes it.
func registerHandoffSubject(t *testing.T, m *Manager, repoID, repoPath, title string, backend session.Backend) *session.Instance {
	t.Helper()
	inst := registerStarted(t, m, repoID, repoPath, title, backend, true, session.Running)
	inst.SetTmuxSession(tmux.NewTmuxSession(title, tmux.ProgramClaude))
	inst.SetAgentConversation(session.AgentConversationData{
		Agent:       tmux.ProgramClaude,
		ID:          "conv-outgoing-42",
		CaptureKind: session.ConversationCaptureInjected,
	})
	return inst
}

// handoffBackend is a FakeBackend instrumented for the agent-handoff tests
// (#2013): it records the swap and the prompt that follows it, and can fail the
// swap so the rollback path is exercised.
type handoffBackend struct {
	*session.FakeBackend
	mu          sync.Mutex
	swapCalls   int
	swapErr     error
	sentPrompts []string
	noHandoff   bool
}

func (b *handoffBackend) Capabilities() session.Capabilities {
	caps := b.FakeBackend.Capabilities()
	if b.noHandoff {
		caps.Handoff = false
	}
	return caps
}

func (b *handoffBackend) SwapAgent(i *session.Instance) error {
	b.mu.Lock()
	b.swapCalls++
	err := b.swapErr
	b.mu.Unlock()
	if err != nil {
		return err
	}
	_ = i.Transition(session.ConfirmLive())
	return nil
}

func (b *handoffBackend) SendPromptCommand(_ *session.Instance, prompt string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sentPrompts = append(b.sentPrompts, prompt)
	return nil
}

func (b *handoffBackend) snapshot() (swaps int, prompts []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.swapCalls, append([]string(nil), b.sentPrompts...)
}

// The mission-delivery regression. A handoff must deliver a BRIEF — one that
// says the agent is inheriting work and points at the diff — not the session's
// stored prompt.
//
// Re-sending the stored prompt is exactly what the #1146 limit-resume path does,
// and it is the obvious wrong shortcut here: it type-checks, it delivers
// something, and the session visibly resumes. But the incoming agent has no idea
// it is continuing anyone's work, so it starts the task over from scratch on top
// of a half-finished branch. This test fails if handoff ever degrades to that.
func TestHandoffSession_DeliversMissionBriefNotTheStoredPrompt(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &handoffBackend{FakeBackend: session.NewFakeBackend()}
	// registerStarted creates the instance running claude (SupportedPrograms[0],
	// and the default agent). The target is gemini ([3]) so neither an index-0
	// bug nor a fall-back-to-default bug can pass.
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "handoff-me", backend)
	inst.Prompt = "fix the flaky retry test"
	inst.SetLimitReached(time.Time{})

	resp, err := manager.HandoffSession(HandoffSessionRequest{
		Title:  "handoff-me",
		RepoID: repoID,
		To:     tmux.ProgramGemini,
	})
	if err != nil {
		t.Fatalf("HandoffSession: %v", err)
	}

	if !resp.OK || resp.To != tmux.ProgramGemini || resp.From != tmux.ProgramClaude {
		t.Fatalf("response = %+v, want a claude → gemini swap", resp)
	}
	if got := inst.AgentProgram(); got != tmux.ProgramGemini {
		t.Fatalf("Program = %q, want %q", got, tmux.ProgramGemini)
	}

	swaps, prompts := backend.snapshot()
	if swaps != 1 {
		t.Fatalf("SwapAgent called %d times, want 1 — the running agent must actually be replaced, not just recorded", swaps)
	}
	if len(prompts) != 1 {
		t.Fatalf("delivered %d prompts, want 1", len(prompts))
	}
	mission := prompts[0]
	if strings.TrimSpace(mission) == inst.Prompt {
		t.Fatalf("delivered the bare stored prompt; the incoming agent must be told it is CONTINUING work, or it starts over.\ngot: %s", mission)
	}
	if !strings.Contains(mission, "fix the flaky retry test") {
		t.Fatalf("mission omits the goal.\ngot: %s", mission)
	}
	if !strings.Contains(mission, "continuing work") {
		t.Fatalf("mission does not frame the work as inherited.\ngot: %s", mission)
	}
	if !strings.Contains(mission, tmux.ProgramClaude) {
		t.Fatalf("mission does not name the outgoing agent.\ngot: %s", mission)
	}

	// A limit block describes the OUTGOING agent's plan. The pane now runs a
	// different agent, so leaving it set would keep the session badged [limit]
	// and make the auto-resume scheduler act on an agent that is no longer there.
	if inst.LimitReached() {
		t.Fatal("limit state still set after a handoff; it belonged to the agent that was just replaced")
	}

	ledger := inst.Handoffs()
	if len(ledger) != 1 {
		t.Fatalf("ledger has %d entries, want 1", len(ledger))
	}
	if ledger[0].Reason != session.HandoffReasonUsageLimit {
		t.Fatalf("ledger reason = %q, want %q for a session that was limit-blocked", ledger[0].Reason, session.HandoffReasonUsageLimit)
	}
}

// A handoff on a session that is NOT limit-blocked is legitimate (an agent can
// be stuck for other reasons) and must be recorded as manual, so a reviewer can
// tell a limit-driven swap from a discretionary one.
func TestHandoffSession_RecordsManualReasonWhenNotLimited(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &handoffBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "not-limited", backend)

	if _, err := manager.HandoffSession(HandoffSessionRequest{
		Title: "not-limited", RepoID: repoID, To: tmux.ProgramAider,
	}); err != nil {
		t.Fatalf("HandoffSession: %v", err)
	}

	ledger := inst.Handoffs()
	if len(ledger) != 1 || ledger[0].Reason != session.HandoffReasonManual {
		t.Fatalf("ledger = %+v, want a single entry with reason %q", ledger, session.HandoffReasonManual)
	}
}

// The rollback regression: when the runtime swap fails, the pane is still
// running the OUTGOING agent, so a record claiming otherwise makes every later
// program-keyed decision wrong — respawn flag injection, readiness heuristics,
// the next handoff's same-agent check.
func TestHandoffSession_RollsBackTheRecordWhenTheSwapFails(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &handoffBackend{
		FakeBackend: session.NewFakeBackend(),
		swapErr:     errors.New("could not confirm the current agent stopped"),
	}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "swap-fails", backend)
	inst.Prompt = "some goal"

	_, err := manager.HandoffSession(HandoffSessionRequest{
		Title: "swap-fails", RepoID: repoID, To: tmux.ProgramGemini,
	})
	if err == nil {
		t.Fatal("HandoffSession succeeded despite a failed runtime swap")
	}

	if got := inst.AgentProgram(); got != tmux.ProgramClaude {
		t.Fatalf("Program = %q after a failed swap, want %q — the pane still runs the outgoing agent", got, tmux.ProgramClaude)
	}
	if ledger := inst.Handoffs(); len(ledger) != 0 {
		t.Fatalf("ledger has %d entries after a failed swap, want 0 — a swap that did not happen must not be recorded", len(ledger))
	}
	if _, prompts := backend.snapshot(); len(prompts) != 0 {
		t.Fatalf("delivered %d prompts after a failed swap, want 0 — briefing an agent that was never started misleads whoever reads the pane", len(prompts))
	}
}

func TestHandoffSession_RefusesSameAgentAndUnknownAgent(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &handoffBackend{FakeBackend: session.NewFakeBackend()}
	registerHandoffSubject(t, manager, repoID, repoPath, "picky", backend)

	if _, err := manager.HandoffSession(HandoffSessionRequest{
		Title: "picky", RepoID: repoID, To: tmux.ProgramClaude,
	}); err == nil {
		t.Fatal("handoff to the running agent accepted; it would stop and restart the agent for nothing")
	}
	if _, err := manager.HandoffSession(HandoffSessionRequest{
		Title: "picky", RepoID: repoID, To: "not-an-agent",
	}); err == nil {
		t.Fatal("handoff to an unknown agent accepted")
	}
	if swaps, prompts := backend.snapshot(); swaps != 0 || len(prompts) != 0 {
		t.Fatalf("a refused handoff still touched the runtime (swaps=%d prompts=%d); validation must run before anything is stopped", swaps, len(prompts))
	}
}

// A backend whose workspace is off-box cannot swap its agent in place, and must
// say so rather than half-performing the swap.
func TestHandoffSession_RefusesBackendWithoutHandoffCapability(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &handoffBackend{FakeBackend: session.NewFakeBackend(), noHandoff: true}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "remote-ish", backend)

	_, err := manager.HandoffSession(HandoffSessionRequest{
		Title: "remote-ish", RepoID: repoID, To: tmux.ProgramGemini,
	})
	if err == nil {
		t.Fatal("handoff accepted on a backend that cannot swap its agent")
	}
	if !IsHandoffUnsupported(err) {
		t.Fatalf("error = %v, want the typed ErrHandoffUnsupported sentinel so clients can render the restriction", err)
	}
	if got := inst.AgentProgram(); got != tmux.ProgramClaude {
		t.Fatalf("Program = %q, want it untouched on a refused handoff", got)
	}
	if swaps, _ := backend.snapshot(); swaps != 0 {
		t.Fatalf("SwapAgent called %d times on an unsupported backend, want 0", swaps)
	}
}
