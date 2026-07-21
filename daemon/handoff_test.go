package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
)

// registerHandoffSubject is registerStarted plus the agent tab a handoff needs:
// the ledger and the conversation slot both live on Tabs[0], so an instance
// without one has nothing to hand off. SetTmuxSession materializes it.
func registerHandoffSubject(t *testing.T, m *Manager, repoID, repoPath, title string, backend session.Backend) *session.Instance {
	t.Helper()
	t.Cleanup(task.SetTrustPromptTimingForTest(time.Millisecond))
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
	mu              sync.Mutex
	swapCalls       int
	swapErr         error
	sentPrompts     []string
	previewCalls    int
	hasUpdatedCalls int
	events          []string
	noHandoff       bool
}

func (b *handoffBackend) Capabilities() session.Capabilities {
	caps := b.FakeBackend.Capabilities()
	if b.noHandoff {
		caps.Handoff = false
	}
	return caps
}

func (b *handoffBackend) SwapAgent(i *session.Instance, _ session.AgentSwapPlan) error {
	b.mu.Lock()
	b.swapCalls++
	b.events = append(b.events, "swap")
	err := b.swapErr
	b.mu.Unlock()
	if err != nil {
		return err
	}
	// Mirror the local backend's SetProgram: readiness and conversation capture
	// must identify the incoming agent from the pane command, not stale tmux state.
	i.SetTmuxSession(tmux.NewTmuxSession(i.Title, i.AgentProgram()))
	return nil
}

func (b *handoffBackend) Preview(*session.Instance) (string, error) {
	// One fixture containing each simple agent's ready glyph keeps these daemon
	// orchestration tests independent of which supported target they choose.
	b.mu.Lock()
	b.previewCalls++
	b.events = append(b.events, "ready")
	b.mu.Unlock()
	return "ready\n❯\n›\n> \n╰", nil
}

func (b *handoffBackend) HasUpdated(*session.Instance) (bool, bool, string) {
	b.mu.Lock()
	b.hasUpdatedCalls++
	b.mu.Unlock()
	return false, false, ""
}

func (b *handoffBackend) SendPromptCommand(_ *session.Instance, prompt string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, "send")
	b.sentPrompts = append(b.sentPrompts, prompt)
	return nil
}

func (b *handoffBackend) eventSnapshot() (events []string, previews, statusPolls int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.events...), b.previewCalls, b.hasUpdatedCalls
}

type rejectingHandoffBackend struct {
	*handoffBackend
	err error
}

func (b *rejectingHandoffBackend) PrepareAgentSwap(*session.Instance, string) (session.AgentSwapPlan, error) {
	return session.AgentSwapPlan{}, b.err
}

type blockingSwapBackend struct {
	*handoffBackend
	entered chan struct{}
	release chan struct{}
}

func (b *blockingSwapBackend) SwapAgent(i *session.Instance, plan session.AgentSwapPlan) error {
	close(b.entered)
	<-b.release
	return b.handoffBackend.SwapAgent(i, plan)
}

type stalePollHandoffBackend struct {
	*handoffBackend
	entered chan struct{}
	release chan struct{}
}

func (b *stalePollHandoffBackend) HasUpdated(*session.Instance) (bool, bool, string) {
	close(b.entered)
	<-b.release
	return false, false, ""
}

func (b *stalePollHandoffBackend) IsAlive(*session.Instance) (bool, error) {
	return false, nil
}

type rolloutHandoffBackend struct {
	*handoffBackend
	codexHome string
}

func (b *rolloutHandoffBackend) SwapAgent(i *session.Instance, plan session.AgentSwapPlan) error {
	if err := b.handoffBackend.SwapAgent(i, plan); err != nil {
		return err
	}
	path := filepath.Join(b.codexHome, "sessions", "2026", "07", "21",
		"rollout-2026-07-21T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(`{"type":"session_meta"}`+"\n"), 0o644)
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

// TestHandoffSession_RefusesArchivedSession is the #2231 regression. Backend
// capability says what the runtime implementation can do; it does not say that
// this particular session currently has a runtime. An archived row is inert and
// its worktree is in archive storage, so handoff must not turn it live in place.
func TestHandoffSession_RefusesArchivedSession(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &handoffBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "archived", backend)
	inst.SetStatusForTest(session.Archived)
	inst.SetStartedForTest(false)

	_, err := manager.HandoffSession(HandoffSessionRequest{
		Title: "archived", RepoID: repoID, To: tmux.ProgramGemini,
	})
	if err == nil {
		t.Fatal("handoff accepted an archived session and reanimated it outside the restore lifecycle")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "archived") {
		t.Fatalf("error = %q, want an actionable archived-session refusal", err)
	}
	if swaps, prompts := backend.snapshot(); swaps != 0 || len(prompts) != 0 {
		t.Fatalf("archived handoff touched the runtime (swaps=%d prompts=%d), want neither", swaps, len(prompts))
	}
	if got := inst.AgentProgram(); got != tmux.ProgramClaude {
		t.Fatalf("Program = %q after refused handoff, want %q", got, tmux.ProgramClaude)
	}
	if ledger := inst.Handoffs(); len(ledger) != 0 {
		t.Fatalf("refused archived handoff wrote %d ledger entries, want 0", len(ledger))
	}
	if got := inst.GetLiveness(); got != session.LiveArchived {
		t.Fatalf("liveness = %v after refused handoff, want Archived", got)
	}
}

func TestHandoffSession_PreflightFailureLeavesOutgoingAgentUntouched(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	base := &handoffBackend{FakeBackend: session.NewFakeBackend()}
	backend := &rejectingHandoffBackend{handoffBackend: base, err: errors.New("gemini executable is missing")}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "preflight-fails", backend)

	_, err := manager.HandoffSession(HandoffSessionRequest{
		Title: "preflight-fails", RepoID: repoID, To: tmux.ProgramGemini,
	})
	if err == nil || !strings.Contains(err.Error(), "without stopping its current agent") {
		t.Fatalf("preflight error = %v, want an actionable refusal before teardown", err)
	}
	if swaps, prompts := base.snapshot(); swaps != 0 || len(prompts) != 0 {
		t.Fatalf("failed preflight touched runtime: swaps=%d prompts=%d", swaps, len(prompts))
	}
	if inst.AgentProgram() != tmux.ProgramClaude || len(inst.Handoffs()) != 0 || inst.GetInFlightOp() != session.OpNone {
		t.Fatalf("failed preflight mutated record: program=%q handoffs=%d op=%v",
			inst.AgentProgram(), len(inst.Handoffs()), inst.GetInFlightOp())
	}
}

func TestHandoffSession_ReplacementFenceSkipsStatusPoll(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	base := &handoffBackend{FakeBackend: session.NewFakeBackend()}
	backend := &blockingSwapBackend{
		handoffBackend: base,
		entered:        make(chan struct{}),
		release:        make(chan struct{}),
	}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "fenced", backend)
	done := make(chan error, 1)
	go func() {
		_, err := manager.HandoffSession(HandoffSessionRequest{
			Title: "fenced", RepoID: repoID, To: tmux.ProgramGemini,
		})
		done <- err
	}()

	select {
	case <-backend.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handoff never reached the runtime swap")
	}
	if got := inst.GetInFlightOp(); got != session.OpReplacing {
		t.Fatalf("in-flight op during swap = %v, want OpReplacing", got)
	}
	manager.refreshInstanceStatus(repoID, inst)
	_, _, statusPolls := base.eventSnapshot()
	if statusPolls != 0 {
		t.Fatalf("status poll probed %d times during replacement; it can observe the intentional no-pane gap as Lost", statusPolls)
	}
	close(backend.release)
	if err := <-done; err != nil {
		t.Fatalf("HandoffSession: %v", err)
	}
}

func TestHandoffSession_StaleOutgoingPollCannotMarkIncomingAgentLost(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	base := &handoffBackend{FakeBackend: session.NewFakeBackend()}
	backend := &stalePollHandoffBackend{
		handoffBackend: base,
		entered:        make(chan struct{}),
		release:        make(chan struct{}),
	}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "stale-poll", backend)
	pollDone := make(chan struct{})
	go func() {
		manager.refreshInstanceStatus(repoID, inst)
		close(pollDone)
	}()
	select {
	case <-backend.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("status poll never captured the outgoing epoch")
	}

	if _, err := manager.HandoffSession(HandoffSessionRequest{
		Title: "stale-poll", RepoID: repoID, To: tmux.ProgramGemini,
	}); err != nil {
		t.Fatalf("HandoffSession: %v", err)
	}
	close(backend.release)
	select {
	case <-pollDone:
	case <-time.After(2 * time.Second):
		t.Fatal("stale status poll did not return")
	}
	if got := inst.GetLiveness(); got != session.LiveRunning {
		t.Fatalf("stale outgoing observation changed incoming liveness to %v, want Running", got)
	}
}

func TestHandoffSession_OverrideBecomesDurablePrompt(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &handoffBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "new-goal", backend)
	inst.SetPrompt("obsolete create-time goal")

	const newGoal = "finish the receipt race without reverting the existing fix"
	if _, err := manager.HandoffSession(HandoffSessionRequest{
		Title: "new-goal", RepoID: repoID, To: tmux.ProgramGemini, Brief: newGoal,
	}); err != nil {
		t.Fatalf("HandoffSession: %v", err)
	}
	if got := inst.GetPrompt(); got != newGoal {
		t.Fatalf("in-memory prompt = %q, want handoff override %q", got, newGoal)
	}
	raw, err := config.LoadRepoInstances(repoID)
	if err != nil {
		t.Fatalf("LoadRepoInstances: %v", err)
	}
	var stored []session.InstanceData
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("decode instances: %v", err)
	}
	if len(stored) != 1 || stored[0].Prompt != newGoal {
		t.Fatalf("persisted prompt = %+v, want %q", stored, newGoal)
	}
}

func TestHandoffSession_WaitsForIncomingReadinessBeforeBriefing(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &handoffBackend{FakeBackend: session.NewFakeBackend()}
	registerHandoffSubject(t, manager, repoID, repoPath, "ordered-delivery", backend)

	if _, err := manager.HandoffSession(HandoffSessionRequest{
		Title: "ordered-delivery", RepoID: repoID, To: tmux.ProgramGemini,
	}); err != nil {
		t.Fatalf("HandoffSession: %v", err)
	}
	events, previews, _ := backend.eventSnapshot()
	if previews == 0 {
		t.Fatalf("handoff sent a mission without a readiness probe: events=%v", events)
	}
	want := []string{"swap", "ready", "send"}
	if len(events) < len(want) || events[0] != want[0] || events[1] != want[1] || events[len(events)-1] != want[2] {
		t.Fatalf("handoff event order = %v, want runtime swap then readiness then send", events)
	}
}

func TestHandoffSession_CapturesIncomingCodexConversation(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	base := &handoffBackend{FakeBackend: session.NewFakeBackend()}
	backend := &rolloutHandoffBackend{handoffBackend: base, codexHome: codexHome}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "capture-codex", backend)

	if _, err := manager.HandoffSession(HandoffSessionRequest{
		Title: "capture-codex", RepoID: repoID, To: tmux.ProgramCodex,
	}); err != nil {
		t.Fatalf("HandoffSession: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if conv := inst.AgentConversation(); conv.Agent == tmux.ProgramCodex && conv.ID == "019f386f-7206-7fc2-803b-f7045e07a242" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("incoming Codex conversation was never captured: %+v", inst.AgentConversation())
}
