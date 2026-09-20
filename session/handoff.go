package session

import (
	"fmt"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Handoff reasons. Only the usage-limit reason exists today; the constant is
// named rather than inlined because the automatic trigger (deferred, see
// docs/design/agent-handoff.md §2.2) will add its own and the ledger has to
// distinguish "a human chose this" from "af chose this" after the fact.
const (
	HandoffReasonUsageLimit = "usage limit"
	HandoffReasonManual     = "manual"
)

// AgentHandoff is one entry in a tab's append-only handoff ledger (#2013): the
// record that this session's agent was swapped for another one, mid-work, on
// the same worktree and branch.
//
// It carries the outgoing agent's conversation identity because the swap
// destroys it on the live tab — Tab.Conversation holds exactly one provider id
// and the incoming agent's capture overwrites it. Preserving it here is what
// keeps a handoff reversible: hand back later and the original conversation is
// still addressable, instead of degrading to that provider's "resume whatever
// was most recent in this directory" behavior.
//
// HeadSHA is the attribution boundary. af authors none of the agent's commits
// and writes no commit trailers, so it cannot mark the work itself; what it can
// do is pin the branch tip at the instant of the swap, which turns "who wrote
// which half" into a git range a reviewer can verify rather than a label they
// have to trust.
type AgentHandoff struct {
	// From is the outgoing agent's conversation identity, as far as it was
	// known. Its Agent field is the outgoing agent name even when no
	// conversation id was ever captured.
	From AgentConversationData `json:"from,omitempty"`
	// To is the incoming agent (a tmux.SupportedPrograms name).
	To          string `json:"to"`
	FromAccount string `json:"from_account,omitempty"`
	ToAccount   string `json:"to_account,omitempty"`
	// At is when the swap was recorded.
	At time.Time `json:"at"`
	// HeadSHA is the branch tip at swap time — everything at or before it is the
	// outgoing agent's work. Empty when the branch had no commits yet.
	HeadSHA string `json:"head_sha,omitempty"`
	// Reason is why the swap happened (HandoffReason*).
	Reason string `json:"reason,omitempty"`
	// Automatic is false for a user-confirmed handoff. Always false today: only
	// the prompted path is built (design D1). It is recorded anyway so a reviewer
	// reading a ledger written by a future af still learns whether a human was in
	// the loop, rather than inferring it from the af version.
	Automatic bool `json:"automatic,omitempty"`
}

// HandoffSwap is the process-local transaction token returned when the ledger
// and Program are rewritten. AgentHandoff is the durable completed-swap record;
// previousProgram is deliberately kept out of it because rollback is synchronous
// and a successful ledger entry must not retain transaction-only state forever.
// previousAccount/previousAuto are the same rollback-only state for the scope a
// cross-agent record drops when the target cannot carry it (#4428), and
// previousAccountAgent is the dropped selection's namespace with it (#4430).
type HandoffSwap struct {
	AgentHandoff
	previousProgram      string
	previousAccount      string
	previousAccountAgent string
	previousAuto         bool
	// effectiveTo is the agent the target's resolved command actually launches,
	// which program_overrides can set apart from the requested enum recorded in
	// To. Mission briefs render sameness against it; the enum stays the ledger
	// and retry identity (#4430 review round 5).
	effectiveTo string
}

// From/To agent names for display, e.g. "codex → claude".
func (h AgentHandoff) String() string {
	from := strings.TrimSpace(h.From.Agent)
	if from == "" {
		from = "unknown"
	}
	return from + " → " + h.To
}

// AgentProgram returns the instance's configured agent program enum under the
// instance lock.
//
// Program became mutable with #2013 (a handoff rewrites it in place), so the
// reads that can run concurrently with a swap must go through here. The write
// side holds i.mu; an unguarded read of the bare field races it. Construction
// and restore paths touch i.Program directly and are safe: the instance is not
// yet shared at that point.
func (i *Instance) AgentProgram() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.Program
}

// Handoffs returns a copy of the agent tab's handoff ledger, oldest first.
func (i *Instance) Handoffs() []AgentHandoff {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if len(i.Tabs) == 0 || len(i.Tabs[0].Handoffs) == 0 {
		return nil
	}
	out := make([]AgentHandoff, len(i.Tabs[0].Handoffs))
	copy(out, i.Tabs[0].Handoffs)
	return out
}

// LastHandoff returns the most recent ledger entry, if any.
func (i *Instance) LastHandoff() (AgentHandoff, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if len(i.Tabs) == 0 || len(i.Tabs[0].Handoffs) == 0 {
		return AgentHandoff{}, false
	}
	return i.Tabs[0].Handoffs[len(i.Tabs[0].Handoffs)-1], true
}

// ValidateHandoffTarget checks that target is a usable handoff destination for
// this instance, without mutating anything. It is the shared precondition for
// the CLI, the RPC, and the TUI so all three refuse the same inputs with the
// same words.
//
// The same-target guard is HandoffTargetIsCurrent: the target's identity is
// the agent its resolved command would LAUNCH when af can prove one, because an
// enum is not an identity (#4430 review) — program_overrides.aider = "codex"
// makes an aider request a self-handoff the enum compare would permit, and
// program_overrides.codex = "aider" makes a codex request a real cross-agent
// handoff the enum compare refused as "already running".
func (i *Instance) ValidateHandoffTarget(target string) error {
	target = strings.TrimSpace(target)
	// Resolve the target's effective agent BEFORE the lock — the resolution
	// does config I/O and must not run under i.mu.
	effective := ""
	if tmux.IsSupportedProgram(target) {
		effective = handoffEffectiveAgent(i, target)
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.validateHandoffTargetLocked(target, effective)
}

// validateHandoffTargetLocked is ValidateHandoffTarget's already-locked half.
// Keeping target identity and runtime eligibility checks in the same instance
// critical section lets SwapAgentProgram validate and mutate one state snapshot.
// effective is the agent target's resolved command launches ("" when it is not
// a provable agent invocation); callers that already resolved it — a frozen
// plan or a pre-lock handoffEffectiveAgent — pass it rather than re-resolve.
func (i *Instance) validateHandoffTargetLocked(target, effective string) error {
	target = strings.TrimSpace(target)
	if target == "" {
		return fmt.Errorf("handoff target agent is required (one of %s)", tmux.SupportedProgramsString())
	}
	if !tmux.IsSupportedProgram(target) {
		return fmt.Errorf("unknown agent %q: handoff target must be one of %s", target, tmux.SupportedProgramsString())
	}
	if current := i.currentAgentNameLocked(); HandoffTargetIsCurrent(current, target, effective, i.Program) {
		return fmt.Errorf("session is already running %s", current)
	}
	return nil
}

// HandoffTargetIsCurrent reports whether a handoff to target would relaunch
// the agent this session already runs. It is the one same-target predicate the
// guard and every picker share, so a row a picker offers is a row the daemon
// accepts.
//
// The target's identity is the agent its resolved command provably launches
// (effective) when there is one. When the command is not provable — a wrapper
// or an arbitrary tool — the only honest sameness evidence is the RECORDED
// enum: a request naming the session's own program enum resolves the same
// override the pane launched from. current cannot fill that role: it is
// token-scanned from the running command and a wrapper's arguments can name
// a different agent entirely (`./collect codex` under aider's enum), which
// would admit a self-handoff that stops the working process and restarts the
// same command with no conversation (#4430 review round 6).
//
// The opaque branch decides SAMENESS only. Whether the target can carry an
// account is still judged on effective, where "" stays non-scopable.
func HandoffTargetIsCurrent(current, target, effective, recorded string) bool {
	if effective != "" {
		return current != "" && current == effective
	}
	return recorded != "" && recorded == strings.TrimSpace(target)
}

// CurrentAgentName reports which agent enum this session should be treated AS.
// It returns "" only when that is genuinely unknowable, and every handoff
// surface — the picker's filter, the same-agent guard, the confirmation copy,
// the ledger's outgoing entry — resolves it through here so they cannot
// disagree about who is being replaced.
//
// It is deliberately NOT ResolvedAgent, and the difference is load-bearing.
// ResolvedAgent answers a different question — "which binary is this pane
// actually running" — for decisions like claude-only flag injection and
// readiness detection (#1116). For those, a wrapper script that af cannot
// identify SHOULD come back empty: injecting claude's flags into an unknown
// command would break it. configuration.md documents that contract ("if you
// wrap an agent in a script, name the script after the agent").
//
// Identity is not that question. A session created as claude is claude even
// when it launches through ~/bin/my-claude-wrapper, and answering "" there is
// not conservative — it is what let the picker offer claude as a handoff target
// for a session already running claude, and let the same-agent guard pass it.
// A self-handoff kills a working agent and restarts it with no conversation, so
// the empty answer authorized the destructive path rather than blocking it.
//
// Precedence runs from most to least direct evidence:
//  1. the running command, when af can identify it — it beats any record,
//     because an override pointing "claude" at codex really is running codex;
//  2. the conversation the agent actually opened, captured at runtime;
//  3. the configured enum, which is what the user asked for and what a handoff
//     rewrites — this is the one that rescues the wrapper-script case.
func (i *Instance) CurrentAgentName() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.currentAgentNameLocked()
}

// canHandoffLocked reports whether this session's agent can be handed off in
// place right now (#2013), reading the SAME two predicates the TUI's handoff gate
// uses (app/handle_handoff.go handleHandoff): the backend's Handoff capability and
// a runtime state that admits the swap. It is projected as InstanceData.CanHandoff
// so browser clients — which cannot run these Go predicates — render the daemon's
// decision rather than re-deriving the rule and drifting from it. Caller holds
// i.mu (ToInstanceData reads it inside toInstanceDataLocked); the two predicates
// resolve under that one hold, so the capability cannot disagree with the liveness
// axes it was read beside.
func (i *Instance) canHandoffLocked() bool {
	return i.capabilitiesLocked().Handoff &&
		i.lifecycleViewLocked().ValidateRuntimeAction(RuntimeActionHandoff) == nil
}

// currentAgentNameLocked is CurrentAgentName's already-locked half, for callers
// holding i.mu (SwapAgentProgram builds the ledger entry inside its write lock,
// and sync.RWMutex is not reentrant). TmuxSession.Program takes only the tmux
// session's own programMu and never calls back into Instance, so reading it
// under i.mu introduces no lock cycle.
func (i *Instance) currentAgentNameLocked() string {
	if ts := i.tmuxLocked(); ts != nil {
		if agent := tmux.DetectAgentFromCommand(ts.Program()); agent != "" {
			return agent
		}
	}
	if len(i.Tabs) > 0 {
		if recorded := strings.TrimSpace(i.Tabs[0].Conversation.Agent); tmux.IsSupportedProgram(recorded) {
			return recorded
		}
	}
	return tmux.DetectAgentFromCommand(i.Program)
}

// SwapAgentProgram rewrites the instance's agent program in place and appends
// the handoff to the tab's ledger. It mutates state only — the caller re-spawns
// the pane and delivers the mission — so it is safe to call before any
// irreversible teardown and cheap to test on its own.
//
// Clearing Tab.Conversation is load-bearing, not tidiness. respawn feeds the
// program through prepareResumeConversation, which would otherwise hand the
// INCOMING agent the outgoing agent's recorded conversation id. That specific
// call is already guarded (ResumeProgramWithConversationID refuses a
// provider mismatch), but leaving a stale codex id on a tab now running claude
// is a lie in the record that the next reader has to re-derive the guard for.
// The ledger keeps the id; the live slot describes the live agent.
//
// The caller must hold whatever serialization the daemon requires; this method
// takes only the instance lock.
func (i *Instance) SwapAgentProgram(target, reason, headSHA string, automatic bool) (HandoffSwap, error) {
	target = strings.TrimSpace(target)
	// Resolve the effective agent before the lock — resolveProgramForAgent does
	// config I/O and must not run under i.mu.
	effectiveAgent := handoffEffectiveAgent(i, target)

	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.validateHandoffTargetLocked(target, effectiveAgent); err != nil {
		return HandoffSwap{}, err
	}
	if err := i.lifecycleViewLocked().ValidateRuntimeAction(RuntimeActionHandoff); err != nil {
		return HandoffSwap{}, err
	}
	// The guard just refused the same-agent case, so this is a cross-agent swap.
	return i.recordHandoffSwapLocked(target, effectiveAgent, true, reason, headSHA, automatic)
}

// RecordHandoffSwap is the transaction-owned mutation used by the daemon after
// BeginHandoff has raised OpReplacing. Keeping it separate from
// SwapAgentProgram makes both legal orderings explicit: ordinary state-only
// tests require a settled live row, while production replacement requires the
// fence and cannot accidentally validate itself as "busy".
//
// effectiveAgent is the agent the swap's frozen command will actually run —
// the AgentSwapPlan's EffectiveAgent — because the scope-drop decision
// belongs to the process that launches, not the enum it was requested under
// (#4430 review).
func (i *Instance) RecordHandoffSwap(target, effectiveAgent, reason, headSHA string, automatic bool) (HandoffSwap, error) {
	target = strings.TrimSpace(target)

	i.mu.Lock()
	defer i.mu.Unlock()
	if i.inFlightOp != OpReplacing {
		return HandoffSwap{}, fmt.Errorf("session %q has no agent replacement in flight", i.Title)
	}
	if err := i.validateHandoffTargetLocked(target, effectiveAgent); err != nil {
		return HandoffSwap{}, err
	}
	return i.recordHandoffSwapLocked(target, effectiveAgent, true, reason, headSHA, automatic)
}

// handoffEffectiveAgent resolves the agent identity of the command a handoff
// to target would launch — the credential-boundary parse of the resolved
// program_overrides command — answering "" when the command is not a provable
// agent invocation (a wrapper or an arbitrary tool, which af cannot scope
// either way). It resolves configuration OUTSIDE the instance lock; callers
// holding i.mu must not invoke it. Where a frozen AgentSwapPlan exists its
// EffectiveAgent is the same answer computed once, and preferred: a
// re-resolution could see a different config than the plan already froze.
func handoffEffectiveAgent(i *Instance, target string) string {
	return HandoffEffectiveAgentForPath(i.Path, target)
}

// HandoffEffectiveAgentForPath is the Instance-free half of
// handoffEffectiveAgent: the same resolution over a path whose repo (or the
// global config, when the path is not one) supplies program_overrides. The
// daemon's account-list response answers with it for clients that hold no
// Instance, so a picker classifies a target by the agent its command launches,
// never by the enum the request happened to name (#4430 review).
//
// The answer comes from the credential-boundary parser — the same
// AgentForCommand the account selection and launch checks use — because every
// consumer of this value decides whether an ACCOUNT can follow the target, and
// the boundary can only grant one to a provable literal invocation. A command
// that merely mentions an agent (`./collect codex`) or runs no agent at all
// (`bash`) answers "": the target is non-scopable, and callers must not fall
// back to the enum — an enum answer would offer --account remedies that can
// never succeed (#4430 review round 3).
func HandoffEffectiveAgentForPath(path, target string) string {
	return sessionenv.AgentForCommand(resolveProgramForPath(path, target))
}

// HandoffEffectiveAgentsForPathInspection answers HandoffEffectiveAgentForPath
// for every target through ONE inspection-scope config read. List endpoints —
// the account and handoff pickers — must use it rather than per-target calls
// to the single-agent helper: that one goes through ResolveConfigForRepo,
// which records the durable in-repo load observation, so a read-only picker
// would emit the runtime-load log and write the inrepo-config-hash marker the
// mutating operation is supposed to announce (#4430 review round 2). Same
// answer per target: the credential-boundary agent of the resolved command, or
// "" when the command is not a provable agent invocation — a picker must treat
// "" as non-scopable rather than falling back to the enum.
func HandoffEffectiveAgentsForPathInspection(path string, targets []string) map[string]string {
	cfg := resolveConfigForPathInspection(path)
	resolved := make(map[string]string, len(targets))
	for _, target := range targets {
		resolved[target] = sessionenv.AgentForCommand(config.ResolveProgram(cfg, target))
	}
	return resolved
}

// EffectiveAgent is the agent identity of the plan's frozen launch command —
// the answer computed on the command preflight actually froze, so it cannot
// see a different configuration than the swap will run. Capability decisions
// (does this incoming process have an account namespace?) must read it rather
// than the requested target enum.
//
// It is parsed by the credential-boundary parser, not a token scan: a command
// like `./collect codex` detects Codex as an argument while AgentForCommand
// proves no literal invocation, so the scan would claim an account namespace
// the launch can never apply. An unprovable command answers "" — every
// SupportsAccounts check on it reads non-scopable, which is the honest answer
// for a launch the account boundary cannot prove (#4430 review round 3).
func (p AgentSwapPlan) EffectiveAgent() string {
	return sessionenv.AgentForCommand(p.program)
}

// handoffStorageCheckpoint projects a runtime swap that has completed while its
// in-memory delivery fence is still raised. Disk cannot retain process-local
// operations, but it must not keep claiming the outgoing agent either: a daemon
// crash during readiness would then restore the wrong Program over a pane that
// already runs the target. The persisted recovery posture is the same one a
// generic post-swap delivery failure takes — incoming agent, LiveRunning, no
// outgoing-provider limit metadata — while memory remains OpReplacing until the
// mission is delivered or explicitly parked. The caller adds the rendered
// PendingHandoffMission before writing this value so a crash cannot erase the
// still-undelivered takeover context.
func (i *Instance) handoffStorageCheckpoint() InstanceData {
	i.mu.RLock()
	defer i.mu.RUnlock()
	data := i.toInstanceDataLocked()
	data.Status = Running
	data.Liveness = LiveRunning
	data.InFlightOp = OpNone
	data.LimitResetAt = time.Time{}
	return data
}

// recordHandoffSwapLocked takes crossAgent from its caller rather than
// re-deriving it: the decision belongs to the admission that froze the launch
// plan. An account-only request's target is the current agent's IDENTITY, not
// an enum whose override produced the pane, so re-resolving it here could turn
// "change the account" into a Program rewrite (#4430 review).
func (i *Instance) recordHandoffSwapLocked(target, effectiveAgent string, crossAgent bool, reason, headSHA string, automatic bool) (HandoffSwap, error) {

	if len(i.Tabs) == 0 {
		return HandoffSwap{}, fmt.Errorf("session %q has no agent tab to hand off", i.Title)
	}
	// A cross-agent swap may drop the session's account pin only for a KNOWN
	// agent with no account namespace. An empty effectiveAgent means af could
	// not classify the resolved command at all — a wrapper such as
	// `npx codex` may launch a scopable agent underneath — and a durable,
	// operator-set pin is never destroyed on an unproven answer: the
	// --account path refuses the same command for the mirror-image reason
	// (#4430 review, D1).
	if crossAgent && i.Account != "" && effectiveAgent == "" {
		return HandoffSwap{}, fmt.Errorf(
			"session %q is scoped to account %q and %s resolves to a command af cannot classify as an agent; refusing to drop the pin on an unproven target",
			i.Title, i.Account, target)
	}

	// Record the outgoing agent through the shared identity resolver, so the
	// ledger names the same agent the guard compared and the confirmation
	// showed. The conversation id alongside it is what makes a hand-back able to
	// re-enter the outgoing agent's own thread, so it is kept as captured.
	outgoing := i.Tabs[0].Conversation
	if strings.TrimSpace(outgoing.Agent) == "" {
		outgoing.Agent = i.currentAgentNameLocked()
	}

	entry := AgentHandoff{
		From:        outgoing,
		To:          target,
		FromAccount: i.Account,
		At:          time.Now(),
		HeadSHA:     strings.TrimSpace(headSHA),
		Reason:      strings.TrimSpace(reason),
		Automatic:   automatic,
	}
	swap := HandoffSwap{
		AgentHandoff:         entry,
		previousProgram:      i.Program,
		previousAccount:      i.Account,
		previousAccountAgent: i.accountAgent,
		previousAuto:         i.accountAutoSelected,
		effectiveTo:          effectiveAgent,
	}
	i.Tabs[0].Handoffs = append(i.Tabs[0].Handoffs, entry)
	i.touchLocked()
	i.Tabs[0].Conversation = AgentConversationData{}
	// Same-agent (account-only) handoffs retain the exact configured command for
	// subsequent restarts — its override still produces the running command, and
	// the detected agent enum is only the ledger's identity. A cross-agent swap
	// rewrites Program so the respawn launches the target's command, even when
	// the enum is merely NAMED like the running agent (#4430 review).
	if crossAgent && i.Program != target {
		i.Program = target
		i.touchLocked()
	}
	// An incoming agent with no account namespace cannot carry the session's
	// scope (#4428): an account names one identity of one agent, and nothing
	// about the incoming agent can resolve the outgoing name. The capability is
	// judged on effectiveAgent — the agent the resolved command actually
	// launches, not the enum it was requested under — because a
	// program_overrides command like `program_overrides.aider = "codex"` runs
	// Codex, which IS scopable: dropping the scope here would launch it with
	// ambient credentials (#4430 review). Dropping the scope inside the same
	// locked mutation that rewrites Program makes ambient the only environment
	// a later refresh can derive — a still-scoped record at the runtime
	// boundary is the violation handoffUnsettledAccountError refuses on.
	// Effectively-scopable commands keep the recorded scope so their swap can
	// name the incoming account explicitly or refuse; selectAccountLocked
	// replaces it on the --account path.
	if crossAgent && i.Account != "" {
		if _, scopable := sessionenv.SupportsAccounts(effectiveAgent); !scopable {
			i.Account = ""
			i.accountAgent = ""
			i.accountAutoSelected = false
			i.touchLocked()
		}
	}
	// Invalidate outgoing-runtime capture BEFORE its pane is torn down. A capture
	// already waiting on a rollout must not refill the live slot after this record
	// has been rewritten for the incoming agent.
	i.agentRuntimeGeneration++

	return swap, nil
}

// RevertHandoff undoes a SwapAgentProgram whose runtime swap then failed,
// restoring the exact outgoing Program value and putting its conversation id
// back. The exact value matters for free-form commands that have no provider
// identity from which a launch command could be reconstructed.
//
// This exists because a failed replacement did not establish the incoming
// runtime. Leaving Program set to that unconfirmed agent would make every later
// decision — respawn flag injection, readiness heuristics, the next handoff's
// same-agent check — act as though the swap committed. A stale ledger entry for
// a swap that never completed is the same class of lie, so the entry comes off
// too.
//
// It removes only the trailing entry, and only when it is the one passed in: if
// anything else has appended since, this is no longer an unwind and refusing is
// safer than truncating someone else's record.
func (i *Instance) RevertHandoff(swap HandoffSwap) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	if len(i.Tabs) == 0 {
		return fmt.Errorf("session %q has no agent tab", i.Title)
	}
	n := len(i.Tabs[0].Handoffs)
	if n == 0 {
		return fmt.Errorf("session %q has no handoff to revert", i.Title)
	}
	if last := i.Tabs[0].Handoffs[n-1]; last != swap.AgentHandoff {
		return fmt.Errorf("session %q: the last handoff is not the one being reverted", i.Title)
	}

	i.Tabs[0].Handoffs = i.Tabs[0].Handoffs[:n-1]
	i.touchLocked()
	i.Tabs[0].Conversation = swap.From
	if i.Program != swap.previousProgram {
		i.Program = swap.previousProgram
		i.touchLocked()
	}
	// Restore the scope a non-scopable target dropped: the runtime swap never
	// completed, so the session is still the outgoing agent's and still owns its
	// account.
	if i.Account != swap.previousAccount {
		i.Account = swap.previousAccount
		i.touchLocked()
	}
	if i.accountAgent != swap.previousAccountAgent {
		i.accountAgent = swap.previousAccountAgent
		i.touchLocked()
	}
	if i.accountAutoSelected != swap.previousAuto {
		i.accountAutoSelected = swap.previousAuto
		i.touchLocked()
	}
	// Generations are monotonic even on rollback. Reusing the old number would
	// make a token from the abandoned target indistinguishable from the restored
	// outgoing runtime.
	i.agentRuntimeGeneration++
	return nil
}

// handoffUnsettledAccountError refuses a runtime swap whose session record still
// carries an account. The record transaction (#4428) settles the scope BEFORE the
// runtime changes — dropped for an incoming agent with no account namespace,
// replaced by an explicit --account for one that has it — so a non-empty Account
// here means a caller skipped that transaction. Letting it through would let the
// environment refresh reapply the name in the INCOMING agent's namespace, where
// it collides with an identity the user never selected. The namespace is judged
// on the plan's effective agent — the command it will actually run — not the
// requested enum (#4430 review). Refusing before any pane is touched is the only
// honest answer, and each class's message names its way through.
func (i *Instance) handoffUnsettledAccountError(plan AgentSwapPlan) error {
	account, _ := i.AccountSelection()
	if account == "" {
		return nil
	}
	effectiveAgent := plan.EffectiveAgent()
	if _, scopable := sessionenv.SupportsAccounts(effectiveAgent); scopable {
		return fmt.Errorf(
			"session %q is scoped to account %q, and an account belongs to one agent — "+
				"af cannot know which %s identity you meant; hand it off with --account to name the %s account",
			i.Title, account, effectiveAgent, effectiveAgent)
	}
	// The message names what the user asked for when the resolved command is
	// not a provable agent invocation — "a handoff to " with an empty agent
	// would read as a rendering bug.
	incoming := effectiveAgent
	if incoming == "" {
		incoming = plan.target
	}
	return fmt.Errorf(
		"session %q still records account %q on a handoff to %s, which cannot carry an account scope — "+
			"the session record must drop the scope before the runtime changes",
		i.Title, account, incoming)
}
