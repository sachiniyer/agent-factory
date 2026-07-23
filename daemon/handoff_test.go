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
	sendErr         error
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
	if b.sendErr != nil {
		return b.sendErr
	}
	b.sentPrompts = append(b.sentPrompts, prompt)
	return nil
}

func (b *handoffBackend) setSendErr(err error) {
	b.mu.Lock()
	b.sendErr = err
	b.mu.Unlock()
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

type blockingReadinessHandoffBackend struct {
	*handoffBackend
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingReadinessHandoffBackend) Preview(i *session.Instance) (string, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return b.handoffBackend.Preview(i)
}

type limitHandoffBackend struct {
	*handoffBackend
}

func (b *limitHandoffBackend) Preview(*session.Instance) (string, error) {
	return "You've hit your usage limit. Try again at Jul 25th, 2026 5:55 PM.", nil
}

type goneReadinessHandoffBackend struct {
	*handoffBackend
}

func (b *goneReadinessHandoffBackend) Preview(*session.Instance) (string, error) {
	return "", tmux.ErrSessionGone
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

// TestHandoffSession_RefusesTheReservedRootSession keeps the root refusal locked
// at the daemon boundary (#2436).
//
// The rule now lives in the shared ValidateRuntimeAction guard rather than in a
// standalone IsReservedTitle check here, because holding it here alone is what
// let the TUI open its picker for root and only surface the refusal after the
// user confirmed. This test asserts the daemon still refuses regardless of where
// the rule is implemented, so the move cannot quietly become a removal — and
// there was no daemon-level test covering it before, which is how a guard came
// to be relied on without ever being exercised.
func TestHandoffSession_RefusesTheReservedRootSession(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &handoffBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, session.RootSessionTitle, backend)

	_, err := manager.HandoffSession(HandoffSessionRequest{
		Title: session.RootSessionTitle, RepoID: repoID, To: tmux.ProgramGemini,
	})
	if err == nil {
		t.Fatal("handoff accepted the daemon-managed root agent")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "root") {
		t.Fatalf("error = %q, want a refusal that names the reserved root agent", err)
	}
	if swaps, prompts := backend.snapshot(); swaps != 0 || len(prompts) != 0 {
		t.Fatalf("refused root handoff touched the runtime (swaps=%d prompts=%d), want neither", swaps, len(prompts))
	}
	if got := inst.AgentProgram(); got != tmux.ProgramClaude {
		t.Fatalf("Program = %q after refused handoff, want %q", got, tmux.ProgramClaude)
	}
	if ledger := inst.Handoffs(); len(ledger) != 0 {
		t.Fatalf("refused root handoff wrote %d ledger entries, want 0", len(ledger))
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

// A successful process swap is only the middle of a handoff. The incoming
// agent is still idle until its mission lands, so the replacement fence must
// cover readiness and delivery too. Otherwise a status tick in this window can
// observe Ready and end a task-backed run before the work has even been sent.
func TestHandoffSession_ReplacementFenceCoversMissionDelivery(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	base := &handoffBackend{FakeBackend: session.NewFakeBackend()}
	backend := &blockingReadinessHandoffBackend{
		handoffBackend: base,
		entered:        make(chan struct{}),
		release:        make(chan struct{}),
	}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "fenced-delivery", backend)
	inst.SetLimitReached(time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC))
	done := make(chan error, 1)
	go func() {
		_, err := manager.HandoffSession(HandoffSessionRequest{
			Title: "fenced-delivery", RepoID: repoID, To: tmux.ProgramGemini,
		})
		done <- err
	}()

	select {
	case <-backend.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handoff never reached incoming-agent readiness")
	}
	opDuringReadiness := inst.GetInFlightOp()
	manager.refreshInstanceStatus(repoID, inst)
	_, _, statusPolls := base.eventSnapshot()
	raw, checkpointErr := config.LoadRepoInstances(repoID)
	var checkpoint []session.InstanceData
	if checkpointErr == nil {
		checkpointErr = json.Unmarshal(raw, &checkpoint)
	}
	close(backend.release)
	if err := <-done; err != nil {
		t.Fatalf("HandoffSession: %v", err)
	}

	if opDuringReadiness != session.OpReplacing {
		t.Fatalf("in-flight op while the incoming mission was still undelivered = %v, want OpReplacing", opDuringReadiness)
	}
	if statusPolls != 0 {
		t.Fatalf("status poll probed %d times before the incoming mission landed; an idle prompt here is not a completed task run", statusPolls)
	}
	if checkpointErr != nil {
		t.Fatalf("load runtime-swap checkpoint: %v", checkpointErr)
	}
	if len(checkpoint) != 1 || checkpoint[0].Program != tmux.ProgramGemini ||
		checkpoint[0].Liveness != session.LiveRunning || !checkpoint[0].LimitResetAt.IsZero() ||
		!strings.Contains(checkpoint[0].PendingHandoffMission, "continuing work") {
		t.Fatalf("disk checkpoint during delivery fence = %+v; want incoming program as Running with the outgoing limit cleared and the rendered mission pending", checkpoint)
	}
	if got := inst.GetInFlightOp(); got != session.OpNone {
		t.Fatalf("in-flight op after mission delivery = %v, want OpNone", got)
	}
}

// A swap can exec the target and still lose its pane before readiness. That is
// not a prompt-delivery failure: no usable incoming runtime was confirmed, so
// the row must be retained inert instead of persisted as healthy Running.
func TestHandoffSession_ReadinessFailureRetainsStartupUnknown(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	base := &handoffBackend{FakeBackend: session.NewFakeBackend()}
	backend := &goneReadinessHandoffBackend{handoffBackend: base}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "startup-died", backend)

	_, err := manager.HandoffSession(HandoffSessionRequest{
		Title: "startup-died", RepoID: repoID, To: tmux.ProgramGemini,
	})
	if !errors.Is(err, task.ErrAgentReadiness) || !errors.Is(err, tmux.ErrSessionGone) {
		t.Fatalf("HandoffSession error = %v, want readiness classification wrapping ErrSessionGone", err)
	}
	if !inst.StartupStateUnknown() || inst.Started() {
		t.Fatalf("failed incoming startup remained actionable: startupUnknown=%v started=%v", inst.StartupStateUnknown(), inst.Started())
	}
	if got := inst.GetInFlightOp(); got != session.OpNone {
		t.Fatalf("startup-unknown handoff retained op %v, want OpNone", got)
	}
	if inst.PendingHandoffMission() == "" {
		t.Fatal("startup-unknown handoff discarded the mission that never landed")
	}
	rec := recordFor(t, repoID, "startup-died")
	if rec == nil || !rec.StartupStateUnknown || rec.PendingHandoffMission == "" {
		t.Fatalf("persisted failed handoff = %+v, want startup-unknown with its mission pending", rec)
	}
}

// A post-ready paste failure keeps a durable pending marker. Once a later poll
// positively observes Ready, the recovery path sends that exact rendered brief
// and clears the marker in memory and on disk.
func TestResumePendingHandoffs_DeliversPostReadyFailure(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	sendErr := errors.New("paste transport failed")
	backend := &handoffBackend{FakeBackend: session.NewFakeBackend(), sendErr: sendErr}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "retry-mission", backend)

	_, err := manager.HandoffSession(HandoffSessionRequest{
		Title: "retry-mission", RepoID: repoID, To: tmux.ProgramGemini,
	})
	if !errors.Is(err, task.ErrPromptDelivery) || !errors.Is(err, sendErr) {
		t.Fatalf("HandoffSession error = %v, want post-ready prompt-delivery failure", err)
	}
	mission := inst.PendingHandoffMission()
	if mission == "" || inst.StartupStateUnknown() || inst.GetInFlightOp() != session.OpReplacing {
		t.Fatalf("post-ready failure state = pending:%q startupUnknown:%v op:%v", mission, inst.StartupStateUnknown(), inst.GetInFlightOp())
	}

	backend.setSendErr(nil)
	manager.ResumePendingHandoffs()

	if got := inst.PendingHandoffMission(); got != "" {
		t.Fatalf("delivered recovery mission remained pending: %q", got)
	}
	if got := inst.GetInFlightOp(); got != session.OpNone {
		t.Fatalf("delivered recovery mission retained replacement fence %v", got)
	}
	_, prompts := backend.snapshot()
	if len(prompts) != 1 || prompts[0] != mission {
		t.Fatalf("recovery prompts = %q, want exactly the pending rendered mission", prompts)
	}
	rec := recordFor(t, repoID, "retry-mission")
	if rec == nil || rec.PendingHandoffMission != "" {
		t.Fatalf("persisted recovered handoff = %+v, want no pending mission", rec)
	}
}

func TestResumePendingHandoffs_PostReadyFailureClearsOutgoingLimit(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	sendErr := errors.New("paste transport failed")
	backend := &handoffBackend{FakeBackend: session.NewFakeBackend(), sendErr: sendErr}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "retry-after-outgoing-limit", backend)
	inst.SetLimitReached(time.Now().Add(time.Hour))

	_, err := manager.HandoffSession(HandoffSessionRequest{
		Title: "retry-after-outgoing-limit", RepoID: repoID, To: tmux.ProgramGemini,
	})
	if !errors.Is(err, task.ErrPromptDelivery) || !errors.Is(err, sendErr) {
		t.Fatalf("HandoffSession error = %v, want post-ready prompt-delivery failure", err)
	}
	if inst.LimitReached() {
		t.Fatal("post-ready failure retained the outgoing provider's limit on the incoming runtime")
	}
	mission := inst.PendingHandoffMission()
	if mission == "" || inst.GetInFlightOp() != session.OpReplacing {
		t.Fatalf("retry state = pending:%q op:%v, want mission behind replacement fence", mission, inst.GetInFlightOp())
	}

	backend.setSendErr(nil)
	manager.ResumePendingHandoffs()

	_, prompts := backend.snapshot()
	if len(prompts) != 1 || prompts[0] != mission {
		t.Fatalf("recovery prompts = %q, want exact pending mission; stale limit state must not divert it to limit resume", prompts)
	}
}

// A usage-limit banner can replace the incoming composer's ready prompt. That
// is a parked handoff, not a generic delivery failure: the rendered takeover
// brief must become the pending prompt so the normal limit-resume path sends
// the context that never landed on this attempt.
func TestHandoffSession_ParksIncomingLimitWithMissionPending(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	base := &handoffBackend{FakeBackend: session.NewFakeBackend()}
	backend := &limitHandoffBackend{handoffBackend: base}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "incoming-limit", backend)
	inst.SetPrompt("finish the transaction without discarding the existing work")

	_, err := manager.HandoffSession(HandoffSessionRequest{
		Title: "incoming-limit", RepoID: repoID, To: tmux.ProgramCodex,
	})
	if !errors.Is(err, task.ErrLimitReached) {
		t.Fatalf("HandoffSession error = %v, want a wrapped task.ErrLimitReached parked outcome", err)
	}
	if !inst.LimitReached() {
		t.Fatal("incoming agent hit a limit during readiness but the handoff remained LiveRunning")
	}
	if _, ok := inst.LimitResetAt(); !ok {
		t.Fatal("parked handoff lost the reset time parsed from the incoming agent's banner")
	}
	if got := inst.GetInFlightOp(); got != session.OpNone {
		t.Fatalf("parked handoff retained in-flight op %v, want OpNone", got)
	}
	pending := inst.GetPrompt()
	if !strings.Contains(pending, "continuing work") ||
		!strings.Contains(pending, "finish the transaction without discarding the existing work") {
		t.Fatalf("pending prompt is not the rendered takeover mission:\n%s", pending)
	}
	if _, prompts := base.snapshot(); len(prompts) != 0 {
		t.Fatalf("sent %d prompts despite the incoming usage-limit banner, want 0", len(prompts))
	}

	raw, loadErr := config.LoadRepoInstances(repoID)
	if loadErr != nil {
		t.Fatalf("LoadRepoInstances: %v", loadErr)
	}
	var stored []session.InstanceData
	if decodeErr := json.Unmarshal(raw, &stored); decodeErr != nil {
		t.Fatalf("decode instances: %v", decodeErr)
	}
	if len(stored) != 1 || stored[0].Liveness != session.LiveLimitReached || stored[0].Prompt != pending {
		t.Fatalf("persisted parked handoff = %+v, want LiveLimitReached with the rendered mission pending", stored)
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

// Conversation capture is asynchronous. A capture started for one Codex
// runtime must not write through two later handoffs merely because the session
// pointer — and eventually even the agent name — match again.
func TestConversationCapture_DropsResultAfterLaterHandoffs(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	backend := &handoffBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "capture-generation", backend)

	if _, err := inst.SwapAgentProgram(tmux.ProgramCodex, session.HandoffReasonManual, "sha-1", false); err != nil {
		t.Fatalf("move fixture to Codex: %v", err)
	}
	inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, tmux.ProgramCodex))
	snap := session.BeginConversationCaptureAtCodexHome(codexHome)
	token := inst.AgentRuntimeToken()

	if _, err := inst.SwapAgentProgram(tmux.ProgramClaude, session.HandoffReasonManual, "sha-2", false); err != nil {
		t.Fatalf("first later handoff: %v", err)
	}
	inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, tmux.ProgramClaude))
	if _, err := inst.SwapAgentProgram(tmux.ProgramCodex, session.HandoffReasonManual, "sha-3", false); err != nil {
		t.Fatalf("second later handoff: %v", err)
	}
	inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, tmux.ProgramCodex))

	writeDaemonCodexRolloutFile(t, codexHome, "rollout-2026-07-21T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl")
	manager.captureAgentConversation(
		repoID, daemonInstanceKey(repoID, inst.Title), inst, snap, token, time.Second,
	)
	if conv := inst.AgentConversation(); conv.HasID() {
		t.Fatalf("capture from the superseded Codex runtime overwrote the current live slot: %+v", conv)
	}
}
