package daemon

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// THE ROOT CREATE, OFF THE POLL GOROUTINE (#3721).
//
// ensureResolvedRoot reads state on the poll goroutine and then acts on it. The
// reading half is cheap and in memory: the layer resolution, the deletion
// tombstone, the kill-grace window, the m.instances lookup, the adopt of a live
// root. This file is the acting half, and every step of it can touch a
// filesystem that never answers — the checkout-marker read, the dead root's
// teardown, the program resolution's own config.RepoFromPath, the Claude
// transcript inspection, and finally reserveCreate's `git rev-parse`, which has
// no context and no deadline BY DESIGN: a create is an admission path, and
// admission keeps RepoFromPath's full error contract and unbounded caller
// lifetime.
//
// Running that on the poll goroutine made one unreachable checkout a daemon-wide
// outage. EnsureRootAgents sits between RefreshStatuses and RestoreLostSessions
// in poll_loop.go, so a root create stalled on a hard-mounted NFS export stopped
// status refresh, Lost recovery and settlement retries for EVERY session on the
// box — not for the stalled repo. poll_loop.go priced that cost as bounded ("a
// (re-)create blocks this poll briefly while the session starts"), which is true
// of a healthy filesystem and has no upper bound on a stalled one.
//
// The fix moves the create; it does not bound it. A create torn down on a timer
// is a half-created session — a record, a worktree, or a runtime that no pass
// reconciles — which is strictly worse than a slow one. So the create keeps its
// unbounded lifetime and gets its own goroutine, and what the poll goroutine
// gives up is only WAITING for it.
//
// The re-attribution probe (#3299) is the precedent for the sweep side of this,
// not for the create itself: that pass bounds its wait with rootHealProbeGrace
// and discards a late result, which is right for a probe that only OBSERVES and
// wrong for a create that MUTATES. Here the sweep does not wait at all, and no
// outcome is ever discarded.

// rootCreateJob is one candidate's create, handed from the poll goroutine to the
// goroutine that runs it.
//
// inst is the record the poll observed — nil for a first-ever create, or a
// Dead/Lost/Archived one to reap. It may be stale by the time the create runs,
// and reapDeadRoot is what settles that: it re-reads m.instances under m.mu
// behind the title's op-lock and refuses on a record that moved, on a kill in
// flight, and on a pending conversation capture. A stale inst therefore yields
// "nothing reaped" and the pass returns, leaving the next tick to decide on a
// current read.
type rootCreateJob struct {
	stateKey   string
	st         *rootEnsureState
	repo       *config.RepoContext
	resolution config.RootAgentResolution
	identity   *resolvedProjectRoot
	inst       *session.Instance
}

// launchRootCreate starts one create for this repo, or does nothing because one
// is already running.
//
// The in-flight mark is keyed by REPO ID, not by the ensure state key, and that
// is load-bearing: two root_agents entries can name two spellings of one
// repository — a linked worktree path and the main root resolve to the same ID —
// which is two rootEnsureStates and one repo. Keyed by state key, both would
// launch, and the only thing between them would be reserveCreate's title
// conflict: the belt missing in exactly the shape it exists for. That conflict
// and the per-title op-lock remain underneath as the hard guard; this is the
// belt.
//
// The mark also closes a window this change opens. CreateSession registers the
// instance in m.instances partway through, before the agent is up, so a poll tick
// can now observe a root that is still being created — where before, the create
// held the poll goroutine and no tick could. A tick that computed that half-built
// record's status as Dead would land on this boundary and, without the mark, try
// to reap the session the create goroutine is still building. (The half-built row
// itself is not new territory: every API- and task-initiated create has always run
// concurrently with the poll, which is why startConversationCaptureLocked
// publishes the pending capture in the same critical section that makes the
// instance visible. The root create was simply the one create in the daemon that
// was not on that footing.)
//
// A tick that finds the mark set returns WITHOUT touching the retry state. A
// create still running is not an outcome: charging it a failure would poison the
// backoff curve, and charging it a success would clear a real failure streak that
// nothing has answered.
func (m *Manager) launchRootCreate(job rootCreateJob) {
	// The test, the mark, the Add and the spawn are ONE lock hold, so "this repo
	// has a create" and "that create exists" can never be observed apart. The go
	// statement itself does not block — it only makes the goroutine runnable —
	// so holding mu across it costs nothing, and if that goroutine's first act
	// were ever to take mu it would simply wait for this return.
	//
	// The value is the workspace, not a bare marker: it is what the shutdown
	// wait prints, and a repo ID is a hash an operator cannot act on.
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, inFlight := m.rootCreatesInFlight[job.repo.ID]; inFlight {
		return
	}
	m.rootCreatesInFlight[job.repo.ID] = job.repo.WorkspacePath()
	m.rootCreateWG.Add(1)

	// Both defers are registered before any work, so every exit path of
	// runRootCreate unwinds through them: the checkout refusal, the reap error,
	// the "nothing reaped" return, each create failure, and success. So does a
	// panic — which then ends the process, because the daemon recovers nowhere
	// and this is not the place to start one. That is not a regression: a panic
	// in this create killed the daemon before it moved goroutines too.
	//
	// The ORDER is deliberate in both directions. runRootCreate records the
	// outcome itself, before it returns, so the retry state is committed before
	// the mark drops and no tick can observe "no create in flight" without also
	// observing the backoff that create earned. And Done runs after the clear
	// (defers unwind last-registered-first), so a returned waitRootAgentCreates
	// implies the marks are already gone.
	go func() {
		defer m.rootCreateWG.Done()
		defer m.finishRootCreate(job.repo.ID)
		m.runRootCreate(job)
	}()
}

// finishRootCreate drops the in-flight mark, freeing the next ensure tick to
// decide this repo again.
func (m *Manager) finishRootCreate(repoID string) {
	m.mu.Lock()
	delete(m.rootCreatesInFlight, repoID)
	m.mu.Unlock()
}

// waitRootAgentCreates blocks until every launched root create has finished. It
// is silent, because tests join on every ensure pass; shutdown goes through
// waitRootAgentCreatesForShutdown, which says what it is waiting for.
func (m *Manager) waitRootAgentCreates() {
	m.rootCreateWG.Wait()
}

// waitRootAgentCreatesForShutdown is the shutdown join, and it narrates itself
// exactly once when there is anything to narrate.
//
// Shutdown JOINS an in-flight create; it never cancels one. RunDaemon closes
// stopCh and waits out the poll loop, after which no further create can be
// launched, and then waits here for one that may still be running. Three reasons
// it is a join. It is what happened before the create moved goroutines — the loop
// reads stopCh only between passes, so wg.Wait already blocked until a create
// returned. SaveInstances runs after it, and a create finishing later would have
// its record written by appendInstanceData and then overwritten by a save that
// predates it. And cancelling produces the half-created session this whole shape
// exists to avoid.
//
// The honest cost: a create on a mount that never answers means a shutdown that
// never completes — identical to before, and bounded from outside by whatever
// escalates SIGTERM to SIGKILL, never from inside. THAT is what the log line is
// for. A daemon that will not exit is indistinguishable from a hung one from the
// outside, so the last thing it writes has to name the checkout holding it and
// say the wait is deliberate; otherwise the only diagnosis available is a SIGKILL
// and a guess. Logged before the wait, since a line written after it would arrive
// only once the thing it was explaining had already resolved.
func (m *Manager) waitRootAgentCreatesForShutdown() {
	m.mu.Lock()
	pending := make([]string, 0, len(m.rootCreatesInFlight))
	for _, workspace := range m.rootCreatesInFlight {
		pending = append(pending, workspace)
	}
	m.mu.Unlock()
	if len(pending) > 0 {
		sort.Strings(pending)
		log.InfoLog.Printf("waiting for %d in-flight root-agent create(s) before shutting down (%s); a create is never cancelled — one stalled on a checkout that does not answer will hold shutdown until it does (#3721)",
			len(pending), strings.Join(pending, ", "))
	}
	m.waitRootAgentCreates()
}

// runRootCreate is everything past the create boundary for one candidate: prove
// the checkout, reap a dead record, resolve the program, settle the conversation
// to resume, create the session, and record the outcome in the same ensure state
// and backoff a synchronous create fed. It runs on its own goroutine and is the
// only caller of that state from off the poll goroutine.
func (m *Manager) runRootCreate(job rootCreateJob) {
	stateKey, st, repo := job.stateKey, job.st, job.repo
	resolution, identity, inst := job.resolution, job.identity, job.inst
	workspace := repo.WorkspacePath()

	// What the vanished root was, snapshotted before the reap deletes the record
	// that holds it: the conversation it was in (#2616) and the tabs it had open
	// (#2628). reapedRoot distinguishes "the reaped root had none of these" from
	// "there was no prior root at all" — only the first is worth reporting, and
	// they are different answers to the question an operator asks after an outage.
	var (
		carried    reapedRootState
		reapedRoot bool
	)

	if inst != nil {
		// PROVE THE CHECKOUT BEFORE DESTROYING THE RECORD (#3366). Below
		// adopt-first, so a live root costs no marker read on the one-second
		// poll cadence; above the reap, so a refusal cannot delete the record
		// that holds the conversation (#2616) and tab roster (#2628) the heal
		// carries. It is deliberately NOT the proof the create runs on — the
		// reap below does blocking work, so createVerifiedRoot re-proves after
		// it (#3366 review). The full rationale is verifyRootCreateCheckout's,
		// next to what it gates.
		if err := m.proveRootCreateCheckout(repo.ID, identity); err != nil {
			m.rootEnsureFailed(stateKey, st, err)
			return
		}
		// An Archived root (#1028) is inert — no tmux — so it must NOT be
		// adopted as live; fall through to reap-and-recreate like Dead/Lost so
		// the always-ensured root comes back. In practice ArchiveSession
		// rejects archiving the reserved root title, so this is defensive; the
		// in-place root worktree is external, so reapDeadRoot's Cleanup is a
		// no-op that only removes daemon-owned state.
		// The root's tmux vanished (crash, tmux server death — the #1104
		// outage class; recorded as Lost since #1108, Dead by older builds).
		// Reap the dead record and fall through to re-create in place — the
		// root keeps its stronger always-ensure semantics rather than waiting
		// for the general Lost-restore loop. Kill is best-effort teardown of
		// already-dead tmux, and an in-place worktree's Cleanup never touches
		// the user's tree (#1107), so this can only remove daemon-owned state.
		//
		// Carrying agent_conversation across that reap is what keeps the two
		// halves of the heal from disagreeing (#2616): the record about to be
		// deleted holds the only pointer to the conversation the root was in, and
		// CreateSession would otherwise mint a fresh id — leaving the one session
		// every watch/monitor delivery targets healthy, Ready, and amnesiac. The
		// tab roster rides across for the same reason and from the same record
		// (#2628): a fresh create comes up with only its agent tab, so everything
		// else the user had open — a terminal, a process tab, a dev-server web
		// tab, an editor — would vanish with the record that listed it.
		m.warn().Printf("root agent for %s is gone (tmux vanished); attempting to reap and re-create it in place", workspace)
		var err error
		carried, reapedRoot, err = m.reapDeadRoot(repo.ID, inst)
		if err != nil {
			m.rootEnsureFailed(stateKey, st, fmt.Errorf("failed to remove dead root record: %w", err))
			return
		}
		if !reapedRoot {
			return
		}
	}

	program := rootAgentProgramForProfile(workspace, resolution.RootAgent)
	skipRecordedResume := false
	if carried.conversation.Agent == tmux.ProgramClaude && carried.conversation.HasID() {
		transcriptProgram, resolveErr := rootAgentTranscriptProgram(workspace, resolution.RootAgent)
		state, inspectErr := session.ClaudeProjectConversationState{}, resolveErr
		if inspectErr == nil {
			state, inspectErr = session.InspectClaudeProjectConversations(transcriptProgram, workspace, carried.conversation)
		}
		switch {
		case inspectErr != nil:
			m.warn().Printf("root agent for %s could not verify its recorded claude conversation %s against the project transcript store: %v; attempting the recorded conversation",
				workspace, carried.conversation.ID, inspectErr)
		case !state.RecordedExists && state.Resume.HasID():
			m.warn().Printf("root agent for %s recorded claude conversation %s has no transcript; substituting newest on-disk project conversation %s",
				workspace, carried.conversation.ID, state.Resume.ID)
			carried.conversation = state.Resume
		case !state.RecordedExists:
			m.warn().Printf("root agent for %s recorded claude conversation %s has no transcript and the project has no replacement transcript; starting fresh",
				workspace, carried.conversation.ID)
			skipRecordedResume = true
		}
	}
	req := CreateSessionRequest{
		Title:    session.RootSessionTitle,
		RepoPath: workspace,
		Program:  program,
		InPlace:  true,
		// Say local out loud, because InPlace already decided it. A root agent is
		// documented as the `af sessions create --here` shape — in-place at the
		// repo root, no worktree, no branch (configuration.md § root_agents) — and
		// an in-place session has no meaning on a runtime that works in a sandbox
		// clone it cannot see.
		//
		// Left empty, the create resolves the repo's `backend` key, so a project
		// that opted into docker/ssh/hook silently got a root whose agent ran in a
		// clone while its record claimed the working tree — the #2778 contradiction,
		// on the one session the daemon guarantees. Once #2778 refuses that
		// contradiction, the same repos would instead lose their always-on root
		// entirely. Neither is what `root_agents` asks for.
		//
		// This does not override the repo's choice for anything else: `backend`
		// still governs every ordinary session there. It only stops the reserved,
		// daemon-owned, hardcoded-in-place session from being read as a sandbox
		// create it can never be.
		Backend:       string(session.BackendLocal),
		allowReserved: true,
		// Both zero on every path that did not just reap a root — a first-ever
		// create, or a kill whose grace window elapsed (KillSession already deleted
		// that record, so there is nothing to continue or rebuild).
		resumeConversation: carried.conversation,
		restoreTabs:        carried.tabs,
		// True only for a heal, including one whose reaped root had nothing to
		// carry — that is the heal whose replacement most needs the marker.
		replacesReapedRecord:  reapedRoot,
		pendingRecreateNotice: carried.notice,
	}
	if skipRecordedResume {
		req.resumeConversation = session.AgentConversationData{}
	}
	data, err := m.createVerifiedRoot(repo.ID, identity, req)
	if err != nil && !isRootCheckoutRefusal(err) && req.resumeConversation.HasID() {
		// The always-on guarantee outranks continuity. A conversation the provider
		// can no longer resume (cleared history, a transcript store the agent no
		// longer has) makes the resumed command exit at startup — and since the
		// reaped record is already gone, retrying it here rather than next tick is
		// what keeps an unresumable id from costing the root a backoff interval of
		// downtime. Losing the history is the bug this carry fixes; losing the ROOT
		// would be worse than the bug.
		if req.resumeConversation.Agent == tmux.ProgramClaude {
			transcriptProgram, resolveErr := rootAgentTranscriptProgram(workspace, resolution.RootAgent)
			state, inspectErr := session.ClaudeProjectConversationState{}, resolveErr
			if inspectErr == nil {
				state, inspectErr = session.InspectClaudeProjectConversations(transcriptProgram, workspace, req.resumeConversation)
			}
			if inspectErr != nil {
				m.warn().Printf("root agent for %s could not re-check failed claude conversation %s against the project transcript store: %v",
					workspace, req.resumeConversation.ID, inspectErr)
			} else if !state.RecordedExists && state.Resume.HasID() && !strings.EqualFold(state.Resume.ID, req.resumeConversation.ID) {
				m.warn().Printf("root agent for %s could not be re-created on claude conversation %s because its transcript disappeared (%v); substituting newest on-disk project conversation %s",
					workspace, req.resumeConversation.ID, err, state.Resume.ID)
				req.resumeConversation = state.Resume
				carried.conversation = state.Resume
				data, err = m.createVerifiedRoot(repo.ID, identity, req)
			}
		}
	}
	if err != nil && !isRootCheckoutRefusal(err) && req.resumeConversation.HasID() {
		m.warn().Printf("root agent for %s could not be re-created on its prior %s conversation %s (%v); retrying with a fresh agent",
			workspace, req.resumeConversation.Agent, req.resumeConversation.ID, err)
		req.resumeConversation = session.AgentConversationData{}
		data, err = m.createVerifiedRoot(repo.ID, identity, req)
	}
	if err != nil {
		if isRootCheckoutRefusal(err) {
			// Report the refusal as itself. createVerifiedRoot returns before
			// CreateSession, so no create was attempted and none may be reported
			// as having failed — and the identical refusal on the pre-reap arm
			// above carries no such prefix, so wrapping it here would make one
			// cause read as two.
			m.rootEnsureFailed(stateKey, st, err)
			return
		}
		m.rootEnsureFailed(stateKey, st, fmt.Errorf("failed to create root session: %w", err))
		return
	}
	log.InfoLog.Printf("ensured root agent for %s (in-place, program %q)", workspace, program)
	if reapedRoot {
		reportRootConversationCarry(workspace, carried.conversation, data.AgentConversation, data.CurrentAgent)
		reportRootTabCarry(workspace, carried.tabs, data.Tabs)
	}
	m.rootEnsureSucceeded(st)
}
