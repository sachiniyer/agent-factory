package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/quota"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
)

// autoAccountSwap is only a scheduling opportunity until admitAccountSwap
// returns it under the limit-resume fence. from is the account that produced the
// wall (empty means ambient); to is a configured candidate.
type autoAccountSwap struct {
	manual                   bool
	mission, headSHA, reason string
	promptOverride           string
	from                     string
	previousAccount          string
	previousAccountAgent     string
	previousAuto             bool
	previousConversation     session.AgentConversationData
	to                       string
	candidates               []string
	fromAgent                string
	agent                    string
	// accountAgent is the registry namespace the swap's account name resolves
	// in — the agent the launch command resolves to, which a program_overrides
	// redirect can set apart from the requested target enum held in agent
	// (#4430 review round 2). Empty on the auto path, where agent already IS
	// the resolved/live agent; a committed swap restores it from the durable
	// pin so a restart under changed overrides cannot relabel the transaction
	// with the new config's agent (#4430 review round 8). The manual handoff
	// sets it explicitly so `program_overrides.aider = "codex"` resolves the
	// account in codex's registry while program resolution still reads
	// aider's override.
	accountAgent string
	// accountOnly records that the manual request named no --to: its agent is
	// the running identity, not an enum whose override produced the pane, so it
	// must never be re-resolved into a cross-agent launch (#4430 review).
	accountOnly bool
	// crossAgent is manual admission's one decision about whether the swap
	// launches agent's command or keeps the recorded program. The launch
	// preflight and the identity commit both read it.
	crossAgent  bool
	alreadySet  bool
	fallbackDue bool
	fellBack    bool
}

// accountNamespace is the agent whose account registry answers the swap's
// name — accountAgent when an override redirect set it, otherwise agent
// itself. Keeping it a method rather than another write site is what stops
// the three namespace consumers (Selected, the limit ledger, the messages)
// from drifting back to the requested enum.
func (s *autoAccountSwap) accountNamespace() string {
	if s.accountAgent != "" {
		return s.accountAgent
	}
	return s.agent
}

var loadAccountLimitEvidenceForSwap = func() ([]session.AccountLimitObservationData, error) {
	return loadAccountLimitEvidenceSnapshot(
		loadPersistedAccountLimitObservations,
		loadAccountLimitLedger,
	)
}

// testHookAccountSwapBeforeAdmissionReturns pauses the final admission after it
// has rebuilt quota evidence. Tests use it to publish a competing live limit in
// the only window that matters: after the final read but before identity commit.
// No-op in production.
var testHookAccountSwapBeforeAdmissionReturns = func() {}

// testHookAccountSwapBeforeFinalFence pauses a scheduled replacement before it
// enters final admission. Tests use it to apply a newer live identity policy
// after the scheduler snapshot but before any destructive replacement work.
// No-op in production.
var testHookAccountSwapBeforeFinalFence = func() {}

// setLimitReached records a live limit through the manager-owned publication
// boundary, so a final account-swap admission observes either the whole limit
// publication or the whole durable identity commit.
func (m *Manager) setLimitReached(instance *session.Instance, resetAt time.Time) {
	m.accountLimitMu.Lock()
	defer m.accountLimitMu.Unlock()
	instance.SetLimitReached(resetAt)
}

func (m *Manager) setLimitReachedAtEpoch(instance *session.Instance, resetAt time.Time, epoch uint64) bool {
	m.accountLimitMu.Lock()
	defer m.accountLimitMu.Unlock()
	return instance.SetLimitReachedAtEpoch(resetAt, epoch)
}

func (m *Manager) parkHandoffAtLimit(instance *session.Instance, resetAt time.Time) error {
	m.accountLimitMu.Lock()
	defer m.accountLimitMu.Unlock()
	return instance.Transition(session.ParkHandoff(resetAt))
}

func (m *Manager) reparkLimitUnderResumeFence(instance *session.Instance, resetAt time.Time) error {
	m.accountLimitMu.Lock()
	defer m.accountLimitMu.Unlock()
	return instance.ReparkLimitUnderResumeFence(resetAt)
}

// accountLimitEvidencePass memoizes the expensive durable scan only for one
// scheduler pass. Admission deliberately does not use this snapshot: it calls
// accountSwapOpportunityFromFacts again under the operation fence so a swap can
// never commit from evidence that predates the destructive boundary.
type accountLimitEvidencePass struct {
	loaded       bool
	observations []session.AccountLimitObservationData
	err          error
}

func (p *accountLimitEvidencePass) load() ([]session.AccountLimitObservationData, error) {
	if !p.loaded {
		p.observations, p.err = loadAccountLimitEvidenceForSwap()
		p.loaded = true
	}
	return p.observations, p.err
}

// committedAccountSwap recognizes a replacement whose identity checkpoint
// landed but whose prompt/notice transaction did not finish. It is independent
// of current config: disabling future choices cannot make a completed move
// silent, and a manual retry must finish the same notice as the scheduler.
func committedAccountSwap(instance *session.Instance) *autoAccountSwap {
	if instance == nil || !instance.SupportsAutomaticAccountSwap() {
		return nil
	}
	from, to, pending := instance.PendingAccountSwap()
	current, currentAuto := instance.AccountSelection()
	manual, mission := instance.PendingManualAccountSwap()
	agent := instance.CurrentAgentName()
	var accountAgent string
	if manual {
		agent = sessionenv.AgentForCommand(instance.AgentProgram())
		// A manual swap can redirect through program_overrides: agent is the
		// recorded requested enum while the committed account was selected in
		// the resolved command's namespace — the one Selected, the limit
		// ledger, and the conversation-id repair must all answer in (#4430
		// review round 3). The durable transaction now carries that namespace:
		// after a restart under changed overrides, ResolvedPaneProgram answers
		// the NEW config (attach rewrites the metadata before any retry reads
		// it) and HandoffEffectiveAgentForPath is current-config by
		// construction — neither can still name the registry the commit used
		// (#4430 review round 4).
		accountAgent = instance.PendingAccountSwapAgent()
		if accountAgent == "" {
			accountAgent = sessionenv.AgentForCommand(instance.ResolvedPaneProgram())
		}
		if accountAgent == "" {
			accountAgent = session.HandoffEffectiveAgentForPath(instance.Path, agent)
		}
	} else if pinned := instance.AccountAgent(); pinned != "" {
		// An automatic transaction's registry is the same durable pin: the
		// commit wrote it, and a restart under changed program_overrides can
		// leave pane metadata (CurrentAgentName) naming the NEW config's agent
		// while the committed accounts still live in the pinned namespace.
		// Recovery launches the frozen program under the pin, so the notice
		// and completion log must name it too (#4430 review round 8).
		agent = pinned
		accountAgent = pinned
	}
	if !pending || (!currentAuto && !manual) || strings.TrimSpace(to) == "" || current != to {
		return nil
	}
	fromAgent := agent
	var headSHA string
	if manual {
		// The pending swap fences every other handoff, so the ledger's newest
		// entry is this transaction's own record: the retry response owes the
		// caller its recorded outgoing agent and attribution boundary, neither
		// of which the post-checkpoint live state still knows.
		if handoff, ok := instance.LastHandoff(); ok {
			if strings.TrimSpace(handoff.From.Agent) != "" {
				fromAgent = handoff.From.Agent
			}
			headSHA = handoff.HeadSHA
		}
	}
	return &autoAccountSwap{
		manual: manual, mission: mission, headSHA: headSHA,
		from:         from,
		to:           to,
		fromAgent:    fromAgent,
		agent:        agent,
		accountAgent: accountAgent,
		alreadySet:   true,
	}
}

// accountSwapAgent names the account NAMESPACE a swap decision belongs to.
//
// It is the RESOLVED, live agent — not sessionenv.AgentForCommand(i.Program) —
// because that is the agent the limit was attributed to. setLimitReachedLocked
// records its observation under currentAgentNameLocked, which reads the running
// tmux command, so a session configured as claude but resolved to codex by
// program_overrides files its wall in the CODEX namespace. Deriving candidates
// from the configured enum would then scan a namespace with no observation in
// it, find every claude account "unlimited", and hand each one to a preflight
// that refuses it as agent drift (#3174 review).
//
// A disagreement between the two yields NO swap rather than a choice: the wall
// was filed under the running agent while the record claims the configured
// enum, and rotating either registry silently spends an account the limit was
// never attributed to (#3082/#3108). Not an error, for the same
// reason the unsupported-agent case below is not one — this runs on every poll
// of a limit-blocked row, and the caller logs a warning per call with no
// backoff, so an error here is a line every daemon_poll_interval for as long as
// the session stays parked. "No swap applies" is the honest answer and the one
// every other ineligible branch already gives.
func accountSwapAgent(instance *session.Instance) string {
	configured := sessionenv.AgentForCommand(instance.AgentProgram())
	live := instance.CurrentAgentName()
	if live == "" {
		live = configured
	}
	// A pinned account names the registry it was selected in (#4430 round 4),
	// and when it matches the live agent it settles the configured/live
	// disagreement rather than adding to it: a redirected manual handoff such
	// as `--to aider --account work` with aider resolving to codex SUPPORTS a
	// settled record whose enum is aider while the running agent and the
	// durable pin are both codex. The pin is proof that state was committed
	// under the lock, so the limit filed under the live agent's namespace may
	// scan that registry's candidates.
	if pinned := instance.AccountAgent(); pinned != "" {
		// When the live agent no longer matches the durable namespace — an
		// override edit repointing the recorded program — rotating either
		// registry would spend an account the pin never named, so no swap
		// applies.
		if pinned != live {
			return ""
		}
	} else if configured != "" && live != configured {
		// No pin to prove the mismatch was committed: an unpinned live/configured
		// disagreement remains ambiguous drift (#3082/#3108) and yields no swap.
		return ""
	}
	if _, supported := sessionenv.SupportsAccounts(live); !supported {
		return ""
	}
	return live
}

// accountSwapOpportunityFromFacts is a scheduling hint, never admission. It
// returns candidates only when identity policy and the registered-account,
// durable-repo, live-session, and retained-limit sources were all read
// successfully. "Unblocked" means only that none of those complete sources has
// a live or unexpired limit observation; it is not a provider quota claim.
func (m *Manager) accountSwapOpportunityFromFacts(instance *session.Instance, global *config.Config) (*autoAccountSwap, error) {
	return m.accountSwapOpportunityFromFactsWithEvidence(instance, global, loadAccountLimitEvidenceForSwap)
}

func (m *Manager) accountSwapOpportunityFromFactsWithEvidence(
	instance *session.Instance,
	global *config.Config,
	loadEvidence accountLimitEvidenceLoader,
) (*autoAccountSwap, error) {
	if instance == nil || !accountSwapResumeEligible(instance) || !instance.SupportsAutomaticAccountSwap() {
		return nil, nil
	}
	if committed := committedAccountSwap(instance); committed != nil {
		return committed, nil
	}
	if global == nil || !global.LimitAutoResume {
		return nil, nil
	}
	agent := accountSwapAgent(instance)
	if agent == "" {
		return nil, nil
	}
	current, currentAuto := instance.AccountSelection()
	limitedAccount, _ := instance.LimitAccount()
	root := instance.GetRepoPath()
	if root == "" {
		root = instance.Path
	}
	resolved, err := config.ResolveConfigForIdentityDecisionFromGlobal(root, global)
	if err != nil {
		return nil, fmt.Errorf("resolve limit account candidates for %q: %w", instance.Title, err)
	}
	if len(resolved.LimitAccountCandidates) == 0 {
		return nil, nil
	}
	home, err := config.GetConfigDir()
	if err != nil {
		return nil, fmt.Errorf("locate registered accounts for %q: %w", instance.Title, err)
	}
	registered, err := agentaccount.List(home, agent)
	if err != nil {
		return nil, fmt.Errorf("list registered %s accounts for %q: %w", agent, instance.Title, err)
	}

	limited, err := m.limitedAccountsForSwap(agent, loadEvidence)
	if err != nil {
		return nil, err
	}
	candidates := quota.SelectAccountCandidates(quota.AccountSelection{
		CurrentAccount:      current,
		CurrentAutoSelected: currentAuto,
		Candidates:          resolved.LimitAccountCandidates,
		Registered:          registered,
		Limited:             limited,
	})
	if len(candidates) == 0 {
		return nil, nil
	}
	return &autoAccountSwap{
		from:                 limitedAccount,
		previousAccount:      current,
		previousAccountAgent: instance.AccountAgent(),
		previousAuto:         currentAuto,
		to:                   candidates[0],
		candidates:           candidates,
		agent:                agent,
	}, nil
}

// preflightAccountSwapCandidates is the candidate-specific half of admission.
// Keeping the ordered set here lets the sole gate distinguish an unprovable
// candidate from an unprovable policy or evidence read without making the
// scheduler itself an authority.
func preflightAccountSwapCandidates(swap *autoAccountSwap, validate func(string) error) (*autoAccountSwap, error) {
	candidates := append([]string(nil), swap.candidates...)
	if len(candidates) == 0 && strings.TrimSpace(swap.to) != "" {
		candidates = []string{swap.to}
	}
	if len(candidates) == 0 {
		return nil, errors.New("no account candidate is available for launch preflight")
	}
	var refusals []error
	for _, candidate := range candidates {
		if err := validate(candidate); err != nil {
			refusals = append(refusals, fmt.Errorf("account %q: %w", candidate, err))
			continue
		}
		admitted := *swap
		admitted.to = candidate
		admitted.candidates = candidates
		return &admitted, nil
	}
	return nil, fmt.Errorf("no explicitly configured account has a proven launch: %w", errors.Join(refusals...))
}

// admitAccountSwap is the sole admission gate for a new identity replacement.
// It runs under the limit-resume fence immediately before teardown and rebuilds
// every fact instead of trusting the scheduler's earlier timing hint. A swap is
// admitted only when the complete identity policy parses, every durable repo was
// scanned, registered accounts and unexpired limit evidence are readable, and
// the exact selected-account launch plan can be frozen. Any failed or incomplete
// read is a refusal that leaves the existing runtime and identity untouched.
func (m *Manager) admitAccountSwap(instance *session.Instance, global *config.Config) (*autoAccountSwap, error) {
	swap, err := m.accountSwapOpportunityFromFacts(instance, global)
	if err != nil {
		return nil, err
	}
	if swap == nil {
		return nil, errors.New("no configured, registered account without current limit evidence is available")
	}
	if swap.alreadySet {
		return nil, errors.New("identity selection is already committed; new-swap admission cannot authorize recovery")
	}
	if instanceHasVSCodeTab(instance) {
		return nil, fmt.Errorf("cannot switch accounts for %q while it has a VS Code tab: integrated login shells can override the selected account from shell startup files, so af cannot prove their identity boundary", instance.Title)
	}
	admitted, err := preflightAccountSwapCandidates(swap, instance.ValidateAccountSwap)
	if err == nil {
		testHookAccountSwapBeforeAdmissionReturns()
	}
	return admitted, err
}

// commitNewAccountSwapIdentity performs the destructive, durable half of a new
// automatic replacement. The caller holds the config-apply, account-limit, and
// personal-project policy fences. fallbackEligible is true only while no
// identity has been selected, so an already-due ordinary resume may retain the
// old identity after an admission or teardown refusal, but never after a failed
// identity checkpoint.
func (m *Manager) commitNewAccountSwapIdentity(
	repoID, key, requestedTitle string,
	instance *session.Instance,
	scheduled *autoAccountSwap,
	global *config.Config,
) (fallbackEligible bool, err error) {
	fallbackDue := scheduled.fallbackDue
	var admitted *autoAccountSwap
	if scheduled.manual {
		admitted, err = m.admitManualAccountSwap(instance, scheduled)
	} else {
		admitted, err = m.admitAccountSwap(instance, global)
	}
	if err != nil {
		return true, fmt.Errorf("no configured account can replace the limited identity for %q: %w", requestedTitle, err)
	}
	admitted.fallbackDue = fallbackDue
	// Update the scheduler-owned opportunity too: the first candidate can be
	// unprovable while a later explicitly configured one is admitted, and the
	// completion log must name the identity actually selected.
	*scheduled = *admitted

	err = m.prepareRuntimeForAccountSwap(key, instance)
	if err == nil {
		// The outgoing runtime is conclusively stopped, so its append-only
		// transcript is final: carry it into the incoming account now, before
		// the checkpoint, so the pending record only ever claims a conversation
		// the new account already holds (#4367). A failed copy re-plans a
		// stated fresh start; only an unplannable fallback is an error here.
		err = instance.CarryAccountSwapConversation()
	}
	if err != nil {
		if errors.Is(err, session.ErrAccountSwapAgentTeardownBlind) {
			// An absent binding does not prove the old writer stopped. Reuse the
			// inert runtime state so neither status refresh nor Lost recovery can
			// launch another agent into this worktree.
			instance.MarkStartupStateUnknown()
			refusal := fmt.Errorf("account handoff for %q refused; automatic recovery disabled: inspect worktree %q and vanished agent pane %q for a detached child still writing before explicitly removing or replacing the session: %w",
				requestedTitle, instance.GetWorktreePath(), instance.TabTmuxName(0), err)
			return false, errors.Join(refusal, m.persistSettlement(repoID, key, instance))
		}
		if scheduled.manual && !instance.LimitReached() {
			probe := probeLiveness(instance, instance.AgentServer())
			if probe == probeAbsent || probe == probeAnsweredDead {
				// The agent may have stopped before a sibling refused teardown.
				// No identity was selected: persist ordinary recovery on the old one.
				_ = instance.Transition(session.ObserveLiveness(session.LiveLost))
				err = errors.Join(err, m.persistSettlement(repoID, key, instance))
			}
		}
		return true, err
	}
	var previousConversation session.AgentConversationData
	var handoff session.HandoffSwap
	previousPrompt := instance.GetPrompt()
	if scheduled.manual {
		// Admission and teardown are complete, and the outgoing identity is still
		// installed. Freeze exactly the work the replacement will inherit. The
		// brief's To is the EFFECTIVE agent — the agent the frozen launch command
		// runs — not the requested enum: with program_overrides.codex = "gemini"
		// a `--to codex` swap launches Gemini, and Render's same-agent check must
		// compare From (the resolved live identity) against that, or a cross-agent
		// handoff would render as an account change and a redirected same-agent
		// carry would claim its conversation was lost (#4430 review round 5).
		brief := instance.BuildMissionBrief(scheduled.accountNamespace(), scheduled.promptOverride, scheduled.reason)
		brief.Conversation = instance.PreparedAccountSwapConversation()
		brief.CrossAgent = scheduled.crossAgent
		scheduled.headSHA = brief.Work.HeadSHA
		scheduled.mission = brief.Render()
		handoff, err = instance.SelectAccountForHandoff(scheduled.from, scheduled.to, scheduled.agent, scheduled.accountNamespace(), scheduled.crossAgent, scheduled.reason, scheduled.headSHA, scheduled.mission)
		previousConversation = handoff.From
	} else {
		previousConversation, err = instance.SelectAccountAutomatically(scheduled.from, scheduled.to)
	}
	if err != nil {
		return false, err
	}
	scheduled.previousConversation = previousConversation
	if scheduled.manual && strings.TrimSpace(scheduled.promptOverride) != "" {
		instance.SetPrompt(strings.TrimSpace(scheduled.promptOverride))
	}
	// The old runtime is conclusively stopped. Make the new identity durable
	// BEFORE starting it, so a crash can never relaunch on the old account while
	// the session reports the replacement.
	if err := m.persistSettlement(repoID, key, instance); err != nil {
		if scheduled.manual {
			_ = instance.RevertHandoff(handoff)
			instance.SetPrompt(previousPrompt)
		}
		_ = instance.RestoreAccountSelectionUnderResumeFence(
			scheduled.previousAccount, scheduled.previousAccountAgent,
			scheduled.previousAuto, scheduled.previousConversation)
		if scheduled.manual && !instance.LimitReached() {
			// Teardown already succeeded, and the prepared launch belongs to the
			// rejected target identity. Hand the restored old identity to ordinary
			// Lost recovery, with a settlement obligation if disk is still down.
			_ = instance.Transition(session.ObserveLiveness(session.LiveLost))
			err = errors.Join(err, m.persistSettlement(repoID, key, instance))
		}
		return false, err
	}
	return false, nil
}

// revalidateGoneAccountSwap re-plans a committed replacement whose runtime is
// gone. If it had already been launched on a carried conversation, the new
// account may be unable to run that resume, so this launch starts fresh and
// its notice says why (#4367).
func revalidateGoneAccountSwap(instance *session.Instance, to string) error {
	if err := instance.AbandonCarriedConversationAfterFailedLaunch(to); err != nil {
		return err
	}
	return instance.ValidateAccountSwap(to)
}

func accountSwapIdentity(agent, account string) string {
	if strings.TrimSpace(account) == "" {
		return "the ambient " + agent + " identity"
	}
	return fmt.Sprintf("%s account %q", agent, account)
}

// accountSwapPrompt renders the notice delivered to a replacement. A manual
// swap's mission already states its conversation outcome; an automatic swap
// states it here, from the committed record (#4367).
func accountSwapPrompt(swap *autoAccountSwap, prompt string, conversation session.HandoffConversation) string {
	if swap.manual {
		fromAgent := swap.fromAgent
		if strings.TrimSpace(fromAgent) == "" {
			fromAgent = swap.agent
		}
		return fmt.Sprintf("[Agent Factory] Handed off from %s to %s. Continue the same task.\n\n%s",
			accountSwapIdentity(fromAgent, swap.from), accountSwapIdentity(swap.accountNamespace(), swap.to), swap.mission)
	}
	notice := fmt.Sprintf(
		"[Agent Factory] This session switched from %s to %s after the previous identity reached its usage limit. "+
			"The replacement was explicitly allowed by limit_account_candidates and had no current limit observation. "+
			"Continue the same task under the new identity.",
		accountSwapIdentity(swap.agent, swap.from), accountSwapIdentity(swap.accountNamespace(), swap.to))
	switch failure := strings.TrimSpace(conversation.CarryFailure); {
	case conversation.Carried:
		notice += " Your conversation was carried over to the new identity, so everything above is still yours to use."
	case failure != "":
		notice += fmt.Sprintf(" af tried to carry the previous conversation over, but %s, so this is a fresh conversation: "+
			"the earlier messages are not available to you — only the working tree and its git history are.", failure)
	}
	if strings.TrimSpace(prompt) == "" {
		return notice + "\n\ncontinue"
	}
	return notice + "\n\n" + strings.TrimSpace(prompt)
}

// accountSwapTrustDismissInterval paces the guarded trust-dialog dismissal a
// replacement Codex's conversation capture runs while it waits. The interval
// must stay a fraction of conversationCaptureTimeout: the dialog has to be
// answered early enough that Codex can still finish its session file inside
// the capture window.
var accountSwapTrustDismissInterval = 200 * time.Millisecond

// beginLiveAccountSwapConversationCapture establishes the missing before-image
// when recovery inherited an already-running replacement pane. Its original
// pre-launch snapshot existed only in the daemon that started the pane, so take
// a new baseline immediately before mission delivery. The account home excludes
// other identities; the resolved pane cwd distinguishes concurrent rollouts
// from other sessions sharing this account (#4715).
func beginLiveAccountSwapConversationCapture(instance *session.Instance, swap *autoAccountSwap) (session.ConversationCaptureSnapshot, error) {
	home, err := config.GetConfigDir()
	if err != nil {
		return session.ConversationCaptureSnapshot{}, fmt.Errorf("locate account registry: %w", err)
	}
	account, err := agentaccount.Selected(home, tmux.ProgramCodex, swap.to)
	if err != nil {
		return session.ConversationCaptureSnapshot{}, fmt.Errorf("locate Codex account %q: %w", swap.to, err)
	}
	if strings.TrimSpace(account.Dir) == "" {
		return session.ConversationCaptureSnapshot{}, fmt.Errorf("Codex account %q has no conversation store", swap.to)
	}
	workDir := instance.GetWorktreePath()
	if strings.TrimSpace(workDir) == "" {
		return session.ConversationCaptureSnapshot{}, errors.New("live Codex replacement has no worktree path for conversation correlation")
	}
	program := instance.ResolvedPaneProgram()
	launch, err := tmux.CommandEnvironmentFromCommand(program, workDir)
	if err != nil {
		return session.ConversationCaptureSnapshot{}, fmt.Errorf("resolve live Codex replacement working directory: %w", err)
	}
	if !launch.WorkingDirKnown() {
		return session.ConversationCaptureSnapshot{}, errors.New("live Codex replacement working directory is not provable")
	}
	return session.BeginConversationCaptureAtCodexHomeAndWorkingDir(account.Dir, launch.WorkingDir), nil
}

func (m *Manager) prepareLiveAccountSwapConversationCapture(instance *session.Instance, swap *autoAccountSwap) (session.ConversationCaptureSnapshot, bool) {
	snap, err := beginLiveAccountSwapConversationCapture(instance, swap)
	if err == nil {
		return snap, true
	}
	// Conversation metadata is additive; completing the committed mission is
	// mandatory. An uncorrelated account-home snapshot could record another
	// session's rollout, so degrade to no capture rather than either guessing or
	// rebuilding the stuck-swap loop (#4715).
	m.warn().Printf("post-delivery conversation capture for %q disabled because no safe live-runtime baseline could be established; continuing account-swap recovery without conversation metadata: %v", instance.Title, err)
	return session.ConversationCaptureSnapshot{}, false
}

// captureAccountSwapConversation binds Codex discovery to the replacement
// runtime while the limit-resume operation still owns its fence. When Codex has
// already minted a rollout, account swaps cannot use the ordinary asynchronous
// capture: that goroutine serializes its write through the same per-session
// operation lock held by the caller, so the pending recovery marker could
// otherwise be cleared and checkpointed before the conversation id became
// durable.
//
// The capture window also owns the trap fixed in #4393: a fresh account home may
// still paint its directory-trust dialog after the readiness check (#4392), the
// status poll skips a pending-swap row, and delivery's own dismissal runs only
// after this returns. Pump the existing guarded recognizer so the replacement
// can reach its composer. Reaching it does not itself mint a rollout; the
// no-rollout branch below handles that separate #4712 ordering constraint. The
// boolean result asks the caller to capture again after delivery creates the
// rollout and retires the pending recovery marker.
func captureAccountSwapConversation(instance *session.Instance, snap session.ConversationCaptureSnapshot) (bool, error) {
	token := instance.AgentRuntimeToken()
	if token.Agent() != tmux.ProgramCodex {
		return false, nil
	}
	type captureResult struct {
		conversation session.AgentConversationData
		err          error
	}
	resultCh := make(chan captureResult, 1)
	go func() {
		conversation, err := session.CaptureAgentConversation(token.Agent(), snap, conversationCaptureTimeout)
		resultCh <- captureResult{conversation, err}
	}()
	var res captureResult
	ticker := time.NewTicker(accountSwapTrustDismissInterval)
	for done := false; !done; {
		select {
		case res = <-resultCh:
			done = true
		case <-ticker.C:
			instance.CheckAndHandleTrustPrompt()
		}
	}
	ticker.Stop()
	if res.err != nil {
		return false, fmt.Errorf("capture replacement Codex conversation: %w", res.err)
	}
	if !res.conversation.HasID() {
		// A fresh Codex runtime does not create a rollout merely by reaching its
		// composer. The first submitted message creates it, and this capture runs
		// before mission delivery so a capture failure can never turn a delivered
		// mission into an ambiguous retry. No rollout is therefore the expected
		// fresh-conversation fallback, not a failed replacement (#4712). Tell the
		// caller to capture again only after the first mission has landed and the
		// pending recovery marker has been retired.
		return true, nil
	}
	if !instance.RecordAccountSwapConversationForRuntime(token, res.conversation) {
		return false, errors.New("replacement Codex runtime changed before its conversation id could be recorded")
	}
	return false, nil
}

// settleReplacementRuntime brings an account replacement's fresh provider
// runtime to a usable state before its mission is sent: the shared
// readiness/trust contract first, then — only when this attempt launched the
// pane — the synchronous Codex conversation capture.
//
// The readiness wait is load-bearing, not cosmetic. A replacement launches
// under the incoming account's own home, which has not trusted the worktree:
// Codex parks on its directory-trust modal — alive, idle, and writing no
// rollout — and Claude lands on the same class of first-run dialog. Nothing
// else on this path ran the dismissal loop the create and handoff paths share,
// so the pane sat answerable-but-untouched while the capture below timed out
// and every retry respawned into the same modal (#4392). Passing an empty
// prompt runs exactly WaitForReady plus the guarded trust-dismissal loop and
// types nothing.
//
// Its failure classes settle the way the delivery paths classify them, because
// the runtime boundary was already crossed: an incoming usage-limit wall parks
// the pending transaction at the REPLACEMENT identity's reset window so the
// retry fires after the wall lifts, while a runtime that never reached
// readiness leaves the row inert — a vanished pane may still have a detached
// child writing the worktree, so an automatic retry is not safe. Both markers
// are persisted before the error returns.
func (m *Manager) settleReplacementRuntime(
	repoID, key, requestedTitle string,
	instance *session.Instance,
	swap *autoAccountSwap,
	launched bool,
	snap session.ConversationCaptureSnapshot,
) (bool, error) {
	_, err := task.WaitForReadyAndSendPromptWithStatus(context.Background(), instance, "")
	var limitErr *task.LimitReachedError
	switch {
	case err == nil:
	case errors.As(err, &limitErr):
		// The incoming identity is itself at a wall — the same classification
		// deliverManualAccountMission applies when its send-path wait sees one.
		var parkErr error
		if swap.manual {
			m.accountLimitMu.Lock()
			parkErr = instance.ParkManualAccountSwapAtLimit(limitErr.ResetAt)
			m.accountLimitMu.Unlock()
		} else {
			parkErr = m.reparkLimitUnderResumeFence(instance, limitErr.ResetAt)
		}
		return false, errors.Join(
			fmt.Errorf("account replacement for %q reached a usage limit on the incoming identity before its runtime became usable: %w", requestedTitle, err),
			parkErr, m.persistSettlement(repoID, key, instance))
	case errors.Is(err, task.ErrAgentReadiness):
		instance.MarkStartupStateUnknown()
		return false, errors.Join(
			fmt.Errorf("account replacement for %q never reached a usable runtime: %w", requestedTitle, err),
			m.persistSettlement(repoID, key, instance))
	default:
		return false, fmt.Errorf("account replacement for %q did not become ready: %w", requestedTitle, err)
	}
	if launched {
		captureAfterDelivery, err := captureAccountSwapConversation(instance, snap)
		if err != nil {
			return false, fmt.Errorf("failed to preserve the replacement conversation for %q: %w", requestedTitle, err)
		}
		return captureAfterDelivery, nil
	}
	return false, nil
}

// prepareRuntimeForAccountSwap establishes that every old local pane is gone
// before the replacement is recorded. An unanswered probe refuses, while an
// absent agent still triggers a sibling-pane recheck for retry safety.
func (m *Manager) prepareRuntimeForAccountSwap(key string, instance *session.Instance) error {
	probe := probeLiveness(instance, instance.AgentServer())
	if probe == probeUnknown {
		return fmt.Errorf("cannot switch accounts for %q: its current runtime did not answer the liveness probe; not starting another identity while the old one may still be running", instance.Title)
	}
	if err := m.stopVSCodeForAccountSwap(key, instance); err != nil {
		return err
	}
	switch probe {
	case probeAlive, probeAnsweredDead:
		return instance.StopForAccountSwap()
	case probeAbsent:
		return instance.StopRemainingPanesForAccountSwap()
	default:
		return fmt.Errorf("cannot switch accounts for %q: unrecognized runtime state", instance.Title)
	}
}

// stopVSCodeForAccountSwap confirms the daemon-owned editor is gone before the
// credential boundary changes under it.
//
// Since #3876 the editor IS account-scoped: vscodeAccountScopeForInstance
// resolves the session's selected account and ensureServerForInstanceInScope
// bakes it into the child's environ, comparing it on reuse so an editor still
// holding the previous identity is dropped rather than served. Stopping is
// therefore the right verb rather than a stand-in for a boundary af lacks: that
// environ is fixed at exec, so there is no rescoping in place — the only way
// onto the new account is a replacement, and the later render that starts one
// now starts it scoped. What this call adds over the supervisor's own lazy
// replacement is ORDER: the old identity's editor is conclusively gone before
// the identity flips, not whenever someone next opens the tab.
//
// Whether the stop is ever REACHED is a separate question, and the honest
// answer today is that it is a fail-closed belt with no known live route — kept
// because what it guards is a credential boundary, not because a caller is
// known to need it. Both doors into it are already shut:
//
//   - New swap: admitAccountSwap refuses a session that has a VS Code tab at
//     all (an integrated login shell can override the selected account from a
//     shell startup file — see the vscode_account_env.go file comment), and the
//     whole resume holds the per-session op-lock CreateTab must take
//     (archiveExclusiveTabLock), so no tab can appear between that refusal and
//     the guard arming just after it.
//   - Recovery: preflightAccountSwapCandidates → ValidateAccountSwap sets
//     accountSwapLaunch, SelectAccountAutomatically sets pendingAccountSwap,
//     and tabSpawnBlockedLocked refuses every spawn route while either is set.
//     pendingAccountSwap is durable, so that guard survives into the daemon
//     restart that finishes a committed swap — which closes, out of #3869
//     itself, the "tab added between a committed swap and the restart" window
//     an earlier version of this comment named as the reason this function
//     fires.
//
// The appends that bypass tabSpawnBlockedLocked do not reopen it either:
// restoreLocalTabs replays a persisted roster, which for a committed swap is the
// one admission proved carried no VS Code tab; restoreCarriedTabs belongs to the
// root-agent heal, and a reserved title is refused a limit resume outright; and
// metadataTabsFrom stages TabNeedsMetadataOnly kinds only, while a VS Code tab
// is TabNeedsLocalWorktreeRead.
//
// The early return is safe for the same reason the tab roster is the predicate:
// an editor exists only for a session that still has a vscode tab, an invariant
// CloseTab (stops it under that same op-lock) and stopVSCodeIfUnwanted (stops
// one whose tab vanished mid-spawn) maintain from both ends.
func (m *Manager) stopVSCodeForAccountSwap(key string, instance *session.Instance) error {
	if !instanceHasVSCodeTab(instance) {
		return nil
	}
	if m.vscode == nil {
		return fmt.Errorf("cannot switch accounts for %q: daemon has no VS Code supervisor", instance.Title)
	}
	if err := m.stopVSCodeForInstance(key, instance.ID); err != nil {
		return fmt.Errorf("cannot switch accounts for %q: cannot confirm its VS Code editor stopped: %w", instance.Title, err)
	}
	return nil
}

func (m *Manager) limitedAccountsForSwap(agent string, loadEvidence accountLimitEvidenceLoader) ([]string, error) {
	m.mu.Lock()
	instances := make([]*session.Instance, 0, len(m.instances))
	for _, other := range m.instances {
		instances = append(instances, other)
	}
	m.mu.Unlock()
	limitedSet := make(map[string]struct{})
	now := nowFunc()
	retained, err := loadEvidence()
	if err != nil {
		return nil, fmt.Errorf("load durable account-limit evidence: %w", err)
	}
	for _, observation := range retained {
		if observation.Agent != agent || strings.TrimSpace(observation.Account) == "" {
			continue
		}
		if !observation.ResetAt.IsZero() && !now.Before(observation.ResetAt.Add(limitResumeGrace)) {
			continue
		}
		limitedSet[observation.Account] = struct{}{}
	}
	for _, other := range instances {
		if other == nil {
			continue
		}
		manual, _ := other.PendingManualAccountSwap()
		// A manual handoff may already have rewritten Program to a different
		// agent. Its retained observations still name the outgoing namespace;
		// do not reinterpret the old live limit under the incoming one.
		//
		// Key the live wall on the agent it was filed under (LimitIdentity), not
		// the configured enum: setLimitReachedLocked records against
		// currentAgentNameLocked(), which honors program_overrides and so can
		// differ from AgentProgram. The durable loops below already key on
		// observation.Agent; matching that axis keeps the (Agent, Account)
		// composite-key invariant from AccountLimitObservationData intact.
		if !manual {
			liveAgent, account, limitedNow := other.LimitIdentity()
			if limitedNow && liveAgent == agent && strings.TrimSpace(account) != "" {
				resetAt, hasReset := other.LimitResetAt()
				if !hasReset || now.Before(resetAt.Add(limitResumeGrace)) {
					limitedSet[account] = struct{}{}
				}
			}
		}
		for _, observation := range other.AccountLimitObservations() {
			if observation.Agent != agent || strings.TrimSpace(observation.Account) == "" {
				continue
			}
			if !observation.ResetAt.IsZero() && !now.Before(observation.ResetAt.Add(limitResumeGrace)) {
				continue
			}
			limitedSet[observation.Account] = struct{}{}
		}
	}
	limited := make([]string, 0, len(limitedSet))
	for account := range limitedSet {
		limited = append(limited, account)
	}
	return limited, nil
}
