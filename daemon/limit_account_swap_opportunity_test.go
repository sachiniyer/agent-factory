package daemon

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

func TestAccountSwapOpportunity_UsesThePollsFrozenGlobalConfig(t *testing.T) {
	manager, _, inst, _ := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
	home, err := config.GetConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second"} {
		if _, err := agentaccount.Register(home, "claude", name); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	frozen := manager.Config()
	frozen.LimitAccountCandidates = []string{"first"}
	writeLimitAccountCandidates(t, "limit_account_candidates = [\"second\"]\n")

	swap, err := manager.accountSwapOpportunityFromFacts(inst, frozen)
	if err != nil {
		t.Fatal(err)
	}
	if swap == nil || swap.to != "first" {
		t.Fatalf("swap from frozen config = %#v, want first", swap)
	}
}

// The account NAMESPACE a swap decision belongs to is the resolved, live agent,
// not the configured enum (#3174 review).
//
// setLimitReachedLocked files its observation under currentAgentNameLocked,
// which reads the RUNNING tmux command, so a session configured as claude but
// resolved to codex by program_overrides records its wall in the CODEX
// namespace. Deriving candidates from i.Program would scan the claude namespace
// — where that observation does not exist — find every claude account
// "unlimited", and hand each one to a picker that refuses the drifted
// session's candidates as agent drift (#3082/#3108). No wrong identity is ever
// selected, but the scan is wasted and its refusal names the wrong thing.
//
// The second half is the anti-vacuity witness, and it is not optional: with the
// same fixture, same accounts and same candidate list, only the agent
// disagreement removed, this fixture DOES produce a swap. Without it, a nil here
// would be satisfied by any unrelated ineligibility — no registered account, an
// empty candidate list, a config that failed to load.
func TestAccountSwapOpportunity_MakesNoDecisionWhenTheResolvedAgentDiffers(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	manager, _, inst, backend := newAutoResumeManager(t, "", true, "keep going", base.Add(time.Hour))
	configureLimitAccountCandidate(t, manager, "work")
	// The live runtime is codex while the record still says claude: the shape
	// program_overrides.claude = "codex" produces.
	inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, tmux.ProgramCodex))

	swap, err := manager.accountSwapOpportunityFromFacts(inst, manager.Config())
	require.NoError(t, err,
		"an ineligible session is not a failure: this runs on every poll of a parked row, and the caller logs each error with no backoff")
	require.Nil(t, swap, "candidates must not be scanned in the configured agent's namespace")

	// Inert: a whole scheduler pass changes no identity and delivers no prompt.
	advance(time.Second)
	manager.ResumeLimitedSessions()
	if _, _, prompts := backend.snapshot(); len(prompts) != 0 {
		t.Fatalf("a session whose namespace could not be decided still resumed: %v", prompts)
	}
	if account, automatic := inst.AccountSelection(); account != "" || automatic {
		t.Fatalf("account selection = (%q, %v), want the identity untouched", account, automatic)
	}

	// Anti-vacuity: agreement on the same fixture DOES yield a candidate.
	inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, tmux.ProgramClaude))
	agreed, err := manager.accountSwapOpportunityFromFacts(inst, manager.Config())
	require.NoError(t, err)
	require.NotNil(t, agreed, "the fixture must be able to produce a swap, or the nil above proves nothing")
	require.Equal(t, "work", agreed.to)
}

// A redirected handoff settles in a shape the configured/live check alone
// calls drift: a session whose account was auto-selected under codex and whose
// program is later redirected `--to aider` with aider resolving to codex keeps
// Program=aider while the running agent and the durable accountAgent pin are
// both codex. The pin was committed under the lock, so when it names the live
// agent's registry the wall — filed under that same live agent — may scan its
// candidates (#4430 review round 8). The account must be auto-selected:
// SelectAccountCandidates refuses a manually chosen identity before this code
// runs, so the redirected scheduler-owned account is the reachable shape. An
// UNPINNED mismatch stays refused by the test above, and a pin that disagrees
// with the live agent stays refused here: rotating either registry would spend
// an account the pin never named.
func TestAccountSwapOpportunity_UsesPinnedNamespaceForRedirectedSettledState(t *testing.T) {
	base := nowFunc()
	manager, _, inst, _ := newAutoResumeManager(t, "", true, "keep going", base.Add(time.Hour))
	home, err := config.GetConfigDir()
	require.NoError(t, err)
	for _, name := range []string{"work", "work2"} {
		_, err = agentaccount.Register(home, tmux.ProgramCodex, name)
		require.NoError(t, err)
	}
	writeLimitAccountCandidates(t, "limit_account_candidates = [\"work2\"]\n")
	manager.Config().LimitAccountCandidates = []string{"work2"}

	// Settled redirected state: requested enum aider, running agent codex,
	// durable pin codex, scheduler-owned account — then re-mark the wall so
	// its identity and account observation are filed under codex/work rather
	// than the fixture's claude/no-account.
	inst.Program = tmux.ProgramAider
	inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, tmux.ProgramCodex))
	inst.ReconcileAccountHandoffSnapshot("work", tmux.ProgramCodex, true, nil)
	inst.ClearLimitReached()
	inst.SetLimitReached(base.Add(time.Hour))

	swap, err := manager.accountSwapOpportunityFromFacts(inst, manager.Config())
	require.NoError(t, err)
	require.NotNil(t, swap,
		"a pin matching the live agent proves the redirect was committed; the codex registry must be scanned")
	require.Equal(t, tmux.ProgramCodex, swap.agent)
	require.Equal(t, "work2", swap.to)

	// A pin that disagrees with the live agent is contradiction, not proof:
	// same fixture, pin filed under claude — still no swap.
	inst.ReconcileAccountHandoffSnapshot("work", tmux.ProgramClaude, true, nil)
	drifted, err := manager.accountSwapOpportunityFromFacts(inst, manager.Config())
	require.NoError(t, err)
	require.Nil(t, drifted, "a pin naming a different registry than the live agent must still refuse")
}

// A committed AUTOMATIC swap restored after a restart under changed
// program_overrides faces the same evidence problem the manual path was fixed
// for: attach rewrites pane metadata to the new config's answer, so
// CurrentAgentName can name an agent the transaction was never committed
// under. Recovery launches the frozen program under the durable pin, and the
// notice and completion log must name that same registry — the namespace both
// accounts were actually selected in (#4430 review round 8).
func TestCommittedAccountSwap_AutomaticSwapNamesTheDurableNamespace(t *testing.T) {
	_, _, inst, _ := newAutoResumeManager(t, "", true, "continue", nowFunc().Add(time.Hour))
	// The scheduler committed work under codex; the restart's rewritten pane
	// metadata now answers gemini.
	inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, tmux.ProgramGemini))
	require.True(t, inst.ReconcileAccountHandoffSnapshot("work", tmux.ProgramCodex, true,
		&session.AccountSwapData{From: "old", To: "work"}))
	inst.ClearLimitReached()

	swap := committedAccountSwap(inst)
	require.NotNil(t, swap, "the committed transaction is owed its completion notice")
	require.Equal(t, tmux.ProgramCodex, swap.accountNamespace(),
		"the incoming identity must be labeled with the registry it was selected in")
	require.Equal(t, tmux.ProgramCodex, swap.agent,
		"the outgoing identity lived in the same committed namespace, not the drifted pane agent")
}

// TestLimitedAccountsForSwap_LeakAcrossConfiguredResolvedDivergence is the
// regression guard for the live-wall sibling loop in limitedAccountsForSwap: it
// keyed on each sibling's CONFIGURED enum (AgentProgram) while a live wall is
// filed under the RESOLVED agent (currentAgentNameLocked, honoring
// program_overrides). A divergent sibling (configured claude, running codex)
// leaked its codex wall into claude's limitedSet; the fix keys on
// LimitIdentity's agent. Exercises the divergent-sibling live-wall path the
// existing self-divergence and manual=true tests do not cover.
func TestLimitedAccountsForSwap_LeakAcrossConfiguredResolvedDivergence(t *testing.T) {
	base := nowFunc()
	m, repoID, inst, _ := newAutoResumeManager(t, "", true, "continue", base.Add(time.Hour))

	// Sibling B: stored Program="claude" (configured enum the buggy loop keyed
	// on), pane runs codex (resolved agent the wall is filed under), account
	// scoped to "work".
	bBackend := &limitResumeBackend{FakeBackend: session.NewFakeBackend(), alive: true}
	b := registerStarted(t, m, repoID, inst.Path, "drifted", bBackend, true, session.Running)
	b.Program = tmux.ProgramClaude
	b.Account = "work"
	b.SetTmuxSession(tmux.NewTmuxSession(b.Title, tmux.ProgramCodex))
	b.SetLimitReached(base.Add(time.Hour))

	// Pin the divergence the bug hinges on.
	require.Equal(t, tmux.ProgramClaude, b.AgentProgram())
	wallAgent, wallAccount, wallLive := b.LimitIdentity()
	require.True(t, wallLive)
	require.Equal(t, tmux.ProgramCodex, wallAgent)
	require.Equal(t, "work", wallAccount)

	// B's wall lives under codex, so "work" must NOT appear in the claude
	// namespace's limited set (the durable loops already keyed on codex and
	// skipped; with the fix the live-wall loop agrees).
	limited, err := m.limitedAccountsForSwap(tmux.ProgramClaude, loadAccountLimitEvidenceForSwap)
	require.NoError(t, err)
	require.NotContains(t, limited, "work",
		"a sibling wall filed under codex must not leak into the claude namespace")

	// Anti-vacuity: the codex namespace, where the wall was actually filed,
	// still sees "work" as limited, so the absence above is the keying fix.
	limited, err = m.limitedAccountsForSwap(tmux.ProgramCodex, loadAccountLimitEvidenceForSwap)
	require.NoError(t, err)
	require.Contains(t, limited, "work")
}

// TestLimitedAccountsForSwap_SameNamespaceSiblingWallIsStillLimited is the
// anti-vacuity companion: a sibling whose configured AND resolved agent both
// match the scanned namespace must still exclude the account, proving the
// resolved-key filter does not over-correct into ignoring a genuine
// same-namespace wall.
func TestLimitedAccountsForSwap_SameNamespaceSiblingWallIsStillLimited(t *testing.T) {
	base := nowFunc()
	m, repoID, inst, _ := newAutoResumeManager(t, "", true, "continue", base.Add(time.Hour))

	bBackend := &limitResumeBackend{FakeBackend: session.NewFakeBackend(), alive: true}
	b := registerStarted(t, m, repoID, inst.Path, "claude-walled", bBackend, true, session.Running)
	b.Program = tmux.ProgramClaude
	b.Account = "work"
	b.SetTmuxSession(tmux.NewTmuxSession(b.Title, tmux.ProgramClaude))
	b.SetLimitReached(base.Add(time.Hour))

	require.Equal(t, tmux.ProgramClaude, b.AgentProgram())
	wallAgent, _, wallLive := b.LimitIdentity()
	require.True(t, wallLive)
	require.Equal(t, tmux.ProgramClaude, wallAgent)

	limited, err := m.limitedAccountsForSwap(tmux.ProgramClaude, loadAccountLimitEvidenceForSwap)
	require.NoError(t, err)
	require.Contains(t, limited, "work",
		"a sibling wall filed under claude must still limit claude's work account")

	// And it does not leak into the codex namespace it has nothing to do with.
	limited, err = m.limitedAccountsForSwap(tmux.ProgramCodex, loadAccountLimitEvidenceForSwap)
	require.NoError(t, err)
	require.NotContains(t, limited, "work")
}
