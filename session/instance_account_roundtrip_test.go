package session

import (
	"encoding/json"
	"testing"

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

// Unsupported combinations must REFUSE at create time with an actionable error,
// not start a session that dies in the pane or silently uses another identity
// (#3051 review).
func TestAccountScoping_RefusesUnsupportedCombinations(t *testing.T) {
	base := InstanceOptions{Title: "t", Path: t.TempDir(), Program: "codex", Account: "work"}

	// Local + codex is the supported combination and must stay allowed.
	require.NoError(t, refuseOffBoxAccount(base))
	require.NoError(t, refuseUnsupportedAccountAgent(base))

	// Docker CARRIES the account now (#3082): the runtime already bind-mounts host
	// paths into the container, so the account's directory is mounted where the
	// in-container agent reads its home and the ambient credential mounts are
	// dropped. Asserted explicitly rather than by removing it from the loop below,
	// so a reader who finds this list does not "restore" docker to it.
	docker := base
	docker.Backend = BackendDocker
	require.NoError(t, refuseOffBoxAccount(docker),
		"docker can place the account, so an account-scoped create there must be allowed")

	// ssh, sandbox and hook place the account too now (#3082): all three provision
	// through the shared sandboxWorkspace, over a shell that already carries a
	// secret on stdin, so the account travels as a tar into the per-session dir
	// that teardown reaps. hook is the same code path, not a fourth one — it
	// delegates to the sandbox provisioner.
	for _, kind := range []BackendKind{BackendSSH, BackendSandbox, BackendHook} {
		scoped := base
		scoped.Backend = kind
		require.NoErrorf(t, refuseOffBoxAccount(scoped),
			"%s places the account through the shared workspace, so it must be allowed", kind)
	}

	// Nothing is left refusing today. The loop below keeps the SHAPE of the rule —
	// a kind that cannot place an account must refuse — so a backend added later
	// lands in it rather than defaulting into ambient credentials.
	for _, kind := range []BackendKind{} {
		scoped := base
		scoped.Backend = kind
		err := refuseOffBoxAccount(scoped)
		require.Error(t, err, "backend %s must refuse an account-scoped create", kind)
		require.Contains(t, err.Error(), string(kind))
		require.Contains(t, err.Error(), "ambient credentials")
		require.Contains(t, err.Error(), "cannot place a credential account",
			"the refusal must say af cannot PLACE the account there — the reason is now per-kind, not a blanket off-box rule")
	}

	// claude's launch is rewritten before the boundary sees it, so it exits 127
	// today. Refuse with the reason instead.
	claude := base
	claude.Program = "claude"
	err := refuseUnsupportedAccountAgent(claude)
	require.Error(t, err, "claude must refuse an account-scoped create until its launch is supported")
	require.Contains(t, err.Error(), "codex", "the error must name what does work")

	// No account selected leaves every path untouched.
	plain := InstanceOptions{Title: "t", Path: t.TempDir(), Program: "claude", Backend: BackendDocker}
	require.NoError(t, refuseOffBoxAccount(plain))
	require.NoError(t, refuseUnsupportedAccountAgent(plain))
}
