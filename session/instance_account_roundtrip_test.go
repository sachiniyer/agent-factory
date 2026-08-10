package session

import (
	"encoding/json"
	"testing"

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

	// An agent nobody has established the boundary can verify still refuses, and the
	// error names what does work rather than only what does not.
	unproven := base
	unproven.Program = "gemini"
	err := refuseUnsupportedAccountAgent(unproven, unproven.Path)
	require.Error(t, err, "an unproven agent must refuse rather than start on the ambient identity")
	require.Contains(t, err.Error(), "claude and codex", "the error must name what does work")

	// No account selected leaves every path untouched.
	plain := InstanceOptions{Title: "t", Path: t.TempDir(), Program: "claude", Backend: BackendDocker}
	require.NoError(t, refuseOffBoxAccount(plain))
	require.NoError(t, refuseUnsupportedAccountAgent(plain, plain.Path))
}

// An account-scoped session must REFUSE a handoff (#3083 review, P1).
//
// Clearing the generated-args declaration was not enough: refreshSessionEnvironment
// reapplies the unchanged Account, so handing a claude session scoped to "work" to
// codex would launch codex under a codex account also named "work" — a different
// identity, selected by a name collision rather than by the user. Bare codex needs
// no declaration, so nothing downstream refuses it.
func TestSwapAgent_RefusesAnAccountScopedSession(t *testing.T) {
	backend := &LocalBackend{}
	inst := &Instance{Title: "scoped", Path: t.TempDir(), Program: "claude", Account: "work"}

	err := backend.SwapAgent(inst, AgentSwapPlan{target: "codex", program: "codex"})

	require.Error(t, err, "an account-scoped handoff must refuse rather than reuse the account name")
	require.Contains(t, err.Error(), "belongs to one agent")
	require.Contains(t, err.Error(), "work", "the refusal must name the account it is protecting")
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
