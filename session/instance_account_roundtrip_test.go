package session

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// The account must survive the ACTUAL persistence round trip.
//
// This test exists because the first version of this feature added a
// `json:"account"` tag to Instance and I reported it as persisted. That tag is
// inert: instances serialize through ToInstanceData().ForStorage(), so a field
// absent from InstanceData is dropped no matter what Instance's tags say. A
// restart or archive/restore then relaunched on the AMBIENT identity while the
// UI still showed the selected account — silent wrong identity, which is the
// one outcome this feature exists to prevent (#3051 review).
//
// So it asserts the round trip rather than the tag: marshal what is actually
// written to disk, read it back, and check the value arrives.
func TestInstanceAccount_SurvivesTheStorageRoundTrip(t *testing.T) {
	original := &Instance{
		Title:   "scoped",
		Path:    t.TempDir(),
		Program: "codex",
		Account: "work",
	}

	data := original.ToInstanceData()
	require.Equal(t, "work", data.Account,
		"ToInstanceData must copy the account; a field absent here is dropped regardless of Instance's tags")

	// Through JSON, which is what storage actually writes.
	encoded, err := json.Marshal(data.ForStorage())
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"account":"work"`,
		"the account must reach the bytes on disk, not merely the in-memory struct")

	var decoded InstanceData
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.Equal(t, "work", decoded.Account)

	// The Instance-rebuild half (FromInstanceData) is NOT exercised here: it
	// attaches to a real tmux session, which this host must not spawn. What is
	// asserted is the half that was actually broken — InstanceData had no account
	// field at all, so the value never reached disk regardless of what happened on
	// the way back. instance_data.go copies it in both directions and CI's session
	// suite drives the rebuild path.
}

// An unscoped session must round-trip as unscoped — the omitempty tag must not
// turn "no account" into something else.
func TestInstanceAccount_EmptyStaysEmpty(t *testing.T) {
	original := &Instance{Title: "plain", Path: t.TempDir(), Program: "codex"}
	data := original.ToInstanceData()
	require.Empty(t, data.Account)

	encoded, err := json.Marshal(data.ForStorage())
	require.NoError(t, err)
	require.NotContains(t, string(encoded), `"account"`,
		"an unscoped session must not persist an account key at all")
}

// The namespace the account was selected in must survive the same round trip.
// A cross-agent handoff can pin work in codex's registry while Program records
// the requested aider label; without a persisted account_agent a restart under
// an edited program_overrides would re-derive the namespace from the label and
// look the label up in the wrong registry (#4430 review round 4).
func TestInstanceAccount_SelectionNamespaceSurvivesTheStorageRoundTrip(t *testing.T) {
	original := &Instance{
		Title:        "scoped",
		Path:         t.TempDir(),
		Program:      "aider",
		Account:      "work",
		accountAgent: "codex",
	}

	encoded, err := json.Marshal(original.ToInstanceData().ForStorage())
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"account_agent":"codex"`)

	var decoded InstanceData
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	// The rebuild half runs on an archived record so no runtime is touched.
	decoded.BackendType = "docker"
	decoded.Liveness = LiveArchived
	restored, err := FromInstanceData(decoded)
	require.NoError(t, err)
	require.Equal(t, "codex", restored.AccountAgent(),
		"the selection namespace must survive restart — a program_overrides edit must not move it")

	// And an ambient record writes neither field.
	ambient, err := json.Marshal((&Instance{Title: "ambient"}).ToInstanceData().ForStorage())
	require.NoError(t, err)
	require.NotContains(t, string(ambient), `"account_agent"`)
}

// The daemon's snapshot reconciliation must carry the namespace with the name:
// a client-side projection that only mirrored Account would refresh under the
// wrong registry after an override edit (#4430 review round 4).
func TestInstanceAccount_ReconcileSnapshotCarriesTheNamespace(t *testing.T) {
	inst := &Instance{Title: "reconciled", Program: "aider"}
	require.True(t, inst.ReconcileAccountHandoffSnapshot("work", "codex", false, nil))
	account, _ := inst.AccountSelection()
	require.Equal(t, "work", account)
	require.Equal(t, "codex", inst.AccountAgent())

	require.False(t, inst.ReconcileAccountHandoffSnapshot("work", "codex", false, nil),
		"an identical snapshot is not a change")
	require.True(t, inst.ReconcileAccountHandoffSnapshot("", "", false, nil),
		"ambient reconciliation clears the namespace with the account")
	require.Empty(t, inst.AccountAgent())
}

func TestInstanceAccount_ExplicitPinAndAutomaticSelectionStayDistinctOnDisk(t *testing.T) {
	explicit := (&Instance{Title: "explicit", Account: "work"}).ToInstanceData()
	require.False(t, explicit.AccountAutoSelected,
		"the zero value must preserve legacy non-empty accounts as explicit pins")

	automatic := &Instance{Title: "automatic", Account: "personal", accountAutoSelected: true}
	stored := automatic.ToInstanceData().ForStorage()
	require.True(t, stored.AccountAutoSelected)
	raw, err := json.Marshal(stored)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"account_auto_selected":true`)

	restored, err := FromInstanceData(InstanceData{
		Title:               "automatic",
		Account:             "personal",
		AccountAutoSelected: true,
		BackendType:         "docker",
		Liveness:            LiveArchived,
	})
	require.NoError(t, err)
	account, restoredAutomatic := restored.AccountSelection()
	require.Equal(t, "personal", account)
	require.True(t, restoredAutomatic, "a daemon restart must not turn an automatic selection into an explicit pin")
}

func TestInstanceAccount_PendingAutomaticSwapSurvivesRestartInert(t *testing.T) {
	original := registeredAccountSwapTestInstance(t, tmux.ProgramClaude, "claude")
	original.Title = "pending-swap"
	original.Prompt = "finish the migration"
	require.NoError(t, original.ValidateAccountSwap("work"))
	_, err := original.SelectAccountAutomatically("", "work")
	require.NoError(t, err)
	original.EndLimitResume()

	stored := original.ToInstanceData().ForStorage()
	stored.Worktree = GitWorktreeData{
		RepoPath: original.Path, WorktreePath: original.Path,
		SessionName: original.Title, BranchName: "main", ExternalWorktree: true,
	}
	require.NotNil(t, stored.PendingAccountSwap)
	require.NotEmpty(t, stored.PendingAccountSwap.ConversationID)
	raw, err := json.Marshal(stored)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"conversation_id"`)

	var decoded InstanceData
	require.NoError(t, json.Unmarshal(raw, &decoded))
	restored, err := FromInstanceData(decoded)
	require.NoError(t, err)
	from, to, pending := restored.PendingAccountSwap()
	require.True(t, pending)
	require.Empty(t, from)
	require.Equal(t, "work", to)
	require.Equal(t, stored.PendingAccountSwap.ConversationID,
		restored.ToInstanceData().PendingAccountSwap.ConversationID)
	require.True(t, restored.Started(), "the inert row must remain eligible for the limit scheduler")
	require.Equal(t, LiveLimitReached, restored.GetLiveness(),
		"ordinary status/lost recovery must not consume the pending swap")
}

type preAccountSwapInstanceData struct {
	ID                  string          `json:"id,omitempty"`
	Title               string          `json:"title"`
	Path                string          `json:"path"`
	Branch              string          `json:"branch"`
	Status              Status          `json:"status"`
	Liveness            Liveness        `json:"liveness,omitempty"`
	Program             string          `json:"program"`
	Account             string          `json:"account,omitempty"`
	BackendType         string          `json:"backend_type,omitempty"`
	StartupStateUnknown bool            `json:"startup_state_unknown,omitempty"`
	Worktree            GitWorktreeData `json:"worktree"`
	Tabs                []TabData       `json:"tabs,omitempty"`
}

func TestPendingAccountSwapStorageProjectionKeepsPreviousReleaseInert(t *testing.T) {
	original := registeredAccountSwapTestInstance(t, tmux.ProgramClaude, "claude")
	original.Title = "pending-swap-rollback"
	original.Prompt = "finish the migration"
	require.NoError(t, original.ValidateAccountSwap("work"))
	_, err := original.SelectAccountAutomatically("", "work")
	require.NoError(t, err)
	original.EndLimitResume()

	stored := original.ToInstanceData().ForStorage()
	stored.Worktree = GitWorktreeData{
		RepoPath: original.Path, WorktreePath: original.Path,
		SessionName: original.Title, BranchName: "main", ExternalWorktree: true,
	}
	require.True(t, stored.StartupStateUnknown,
		"the immediately previous release must see a lifecycle fence it understands")
	require.NotNil(t, stored.PendingAccountSwap.OriginalStartupStateUnknown)
	require.False(t, *stored.PendingAccountSwap.OriginalStartupStateUnknown)
	reprojected := stored.ForStorage()
	require.False(t, *reprojected.PendingAccountSwap.OriginalStartupStateUnknown,
		"rewriting a projected row must retain the real lifecycle value")
	clientView := stored.ForClientRead()
	require.False(t, clientView.StartupStateUnknown,
		"disk fallback clients must see the pending-swap lifecycle, not its old-reader fence")
	require.Nil(t, clientView.PendingAccountSwap.OriginalStartupStateUnknown)
	payload, err := json.Marshal(stored)
	require.NoError(t, err)
	var previousWire preAccountSwapInstanceData
	require.NoError(t, json.Unmarshal(payload, &previousWire),
		"the previous release ignored additive account-swap fields")
	previousPayload, err := json.Marshal(previousWire)
	require.NoError(t, err)
	var previousView InstanceData
	require.NoError(t, json.Unmarshal(previousPayload, &previousView))
	previous, err := FromInstanceData(previousView)
	require.NoError(t, err)
	require.True(t, previous.StartupStateUnknown(),
		"rollback must not resume an unrelated conversation under the replacement account")

	current, err := FromInstanceData(stored)
	require.NoError(t, err)
	require.False(t, current.StartupStateUnknown(),
		"the current reader must remove its compatibility projection")
	_, to, pending := current.PendingAccountSwap()
	require.True(t, pending)
	require.Equal(t, "work", to)
	require.True(t, current.Started(),
		"the current scheduler must retain the pending delivery obligation")
}

// Unsupported combinations must REFUSE at create time with an actionable error,
// not start a session that dies in the pane or silently uses another identity
// (#3051 review).
func TestAccountScoping_RefusesUnsupportedCombinations(t *testing.T) {
	base := InstanceOptions{Title: "t", Path: t.TempDir(), Program: "codex", Account: "work"}

	// Local + codex is the supported combination and must stay allowed.
	require.NoError(t, refuseOffBoxAccount(base))
	require.NoError(t, refuseUnsupportedAccountAgent(base, base.Path))

	// Docker CARRIES the account now (#3082): the runtime already bind-mounts host
	// paths into the container, so the account's directory is mounted where the
	// in-container agent reads its home and the ambient credential mounts are
	// dropped. Asserted explicitly rather than by removing it from the loop below,
	// so a reader who finds this list does not "restore" docker to it.
	docker := base
	docker.Backend = BackendDocker
	require.NoError(t, refuseOffBoxAccount(docker),
		"docker can place the account, so an account-scoped create there must be allowed")

	// The rest must refuse rather than run on that host's own credentials — but
	// each for ITS OWN reason (#3103). "af cannot place a credential account" was
	// the blanket wording, and it was false for ssh: that runtime already streams
	// af's binary to the remote and creates a per-session directory there. The
	// per-kind reasons are asserted in account_offbox_refusal_test.go; this loop
	// pins only that every one of them refuses and names itself and the way out.
	for _, kind := range []BackendKind{BackendSSH, BackendSandbox, BackendHook} {
		scoped := base
		scoped.Backend = kind
		err := refuseOffBoxAccount(scoped)
		require.Error(t, err, "backend %s must refuse an account-scoped create", kind)
		require.Contains(t, err.Error(), string(kind))
		require.Contains(t, err.Error(), "--account", "the refusal must name the way out")
		require.NotContains(t, err.Error(), "cannot place a credential account",
			"that blanket wording was false for ssh and invited the operator to read a decision as unfinished wiring")
	}

	// claude is ADMITTED now (#3083): af declares the arguments it appends to that
	// launch, so the boundary verifies the rewritten command instead of refusing it.
	// This assertion is inverted from what it was, and deliberately — the refusal it
	// used to guard existed only because the launch was unprovable, and it is the
	// front door to every generated-args path in this package. Left as a refusal, all
	// of that wiring is unreachable to users no matter how well it works (#3083 review).
	claude := base
	claude.Program = "claude"
	require.NoError(t, refuseUnsupportedAccountAgent(claude, claude.Path),
		"an account-scoped claude create must reach the launch that carries its declaration")

	// gemini is ADMITTED now (#3639), and like claude's admission above this
	// assertion is inverted from what it was. It was refused while nobody had run
	// the check; the check was then run rather than waived. af hands a gemini pane
	// the resolved command unchanged, so ValidateAccountCommand proves it without a
	// declaration — see TestGeminiLaunch_ProducesACommandTheAccountBoundaryAccepts,
	// which pins the property this admission rests on. Left as a refusal, `af
	// accounts add gemini` would keep creating accounts no session can use.
	gemini := base
	gemini.Program = "gemini"
	require.NoError(t, refuseUnsupportedAccountAgent(gemini, gemini.Path),
		"an account-scoped gemini create must reach the launch its command was proven against")

	// An agent that is not on the roster at all still refuses, and names the
	// alternatives rather than only what does not work.
	unrostered := base
	unrostered.Program = "aider"
	err := refuseUnsupportedAccountAgent(unrostered, unrostered.Path)
	require.Error(t, err, "an agent with no verified credential root must refuse")
	require.Contains(t, err.Error(), "account scoping supports claude, codex, gemini",
		"the error must name what does work, with each agent's state")

	// No account selected leaves every path untouched.
	plain := InstanceOptions{Title: "t", Path: t.TempDir(), Program: "claude", Backend: BackendDocker}
	require.NoError(t, refuseOffBoxAccount(plain))
	require.NoError(t, refuseUnsupportedAccountAgent(plain, plain.Path))
}

// A handoff that reaches the runtime boundary with an account still on the
// record must refuse (#3083 review, #4428).
//
// Clearing the generated-args declaration was not enough: refreshSessionEnvironment
// reapplies the unchanged Account, so handing a claude session scoped to "work" to
// codex would launch codex under a codex account also named "work" — a different
// identity, selected by a name collision rather than by the user. Bare codex needs
// no declaration, so nothing downstream refuses it.
//
// Since #4428 the honest answers differ by target: a scopable one takes an
// explicit --account, a non-scopable one has the record transaction drop the
// scope. A direct backend call bypasses that transaction, so the account still
// being recorded is itself the violation both cases refuse on.
func TestSwapAgent_RefusesAnAccountScopedSession(t *testing.T) {
	backend := &LocalBackend{}

	scopable := &Instance{Title: "scoped", Path: t.TempDir(), Program: "claude", Account: "work"}
	err := backend.SwapAgent(scopable, AgentSwapPlan{target: "codex", program: "codex"})

	require.Error(t, err, "an account-scoped handoff must refuse rather than reuse the account name")
	require.Contains(t, err.Error(), "belongs to one agent")
	require.Contains(t, err.Error(), "work", "the refusal must name the account it is protecting")
	require.Contains(t, err.Error(), "--account", "the refusal must name the explicit remedy")
	require.NotContains(t, err.Error(), "new session", "creating a session is no longer the remedy")

	unscopable := &Instance{Title: "scoped", Path: t.TempDir(), Program: "claude", Account: "work"}
	err = backend.SwapAgent(unscopable, AgentSwapPlan{target: "aider", program: "aider"})

	require.Error(t, err, "a still-scoped record must not reach the runtime swap, "+
		"even for a target that cannot carry the scope")
	require.Contains(t, err.Error(), "work")
}

// A cross-agent program_overrides must refuse BEFORE launch (#3083 review, P1).
//
// The account name is validated in the REQUESTED agent's namespace, but the launch
// derives the agent from the RESOLVED command. So Program=claude with
// program_overrides.claude=codex validates "work" as a claude account and then runs
// codex under CODEX's "work" — two namespaces, one name, and the session silently
// authenticates as someone the user never selected. A gate reading the label cannot
// see it, which is why this one resolves first.
func TestAccountScoping_RefusesACrossAgentOverride(t *testing.T) {
	// A GLOBAL override, deliberately: it is the case a repo-only lookup would miss
	// while the launch still applied it.
	saveOverrideConfig(t, "codex")
	path := t.TempDir()

	err := refuseUnsupportedAccountAgent(
		InstanceOptions{Title: "t", Path: path, Program: "claude", Account: "work"}, path)

	require.Error(t, err, "a cross-agent override must refuse: the validated namespace is not the one that would be used")
	require.Contains(t, err.Error(), "namespaces are separate")
	require.Contains(t, err.Error(), "work")
}

// The provision-side twin of the refusal above (#4430 review, D2). The local
// create boundary refuses a cross-agent override through
// refuseUnsupportedAccountAgent, but resolveAccountForProvision — the helper
// docker provisioning, off-box re-provision, and the committed-swap retry all
// reach — resolved the account under the RESOLVED namespace instead, silently
// spending codex's "work" for `--program claude --account work`. Both backends
// must give the same answer to the same command, and the codex registration
// below is what makes the test prove the refusal rather than a lookup miss.
func TestResolveAccountForProvision_RefusesACrossAgentOverride(t *testing.T) {
	saveOverrideConfig(t, "codex")
	home := os.Getenv("AGENT_FACTORY_HOME")
	_, err := agentaccount.Register(home, tmux.ProgramClaude, "work")
	require.NoError(t, err)
	_, err = agentaccount.Register(home, tmux.ProgramCodex, "work")
	require.NoError(t, err, "the colliding name exists in the resolved namespace — only the refusal prevents silently spending it")

	_, err = resolveAccountForProvision(initTempGitRepo(t), tmux.ProgramClaude, "work")

	require.Error(t, err, "the provision helper must refuse the same drift the local boundary refuses")
	require.Contains(t, err.Error(), "namespaces are separate")
	require.Contains(t, err.Error(), "work")
}
