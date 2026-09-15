package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// #4404: create-time account routing. These drive routeCreateAccount — the
// function CreateSession calls in place of a bare default lookup — because the
// whole contract is decided there: which registered account a session launches
// under, what evidence excludes one, and when the create refuses outright.

// stubAccountLimitEvidence replaces the durable-evidence loader the router and
// the swap path share, so a test can say exactly which accounts are walled.
func stubAccountLimitEvidence(t *testing.T, observations []session.AccountLimitObservationData) {
	t.Helper()
	previous := loadAccountLimitEvidenceForSwap
	loadAccountLimitEvidenceForSwap = func() ([]session.AccountLimitObservationData, error) {
		return observations, nil
	}
	t.Cleanup(func() { loadAccountLimitEvidenceForSwap = previous })
}

func registerAccounts(t *testing.T, home, agent string, names ...string) {
	t.Helper()
	for _, name := range names {
		require.NoError(t, os.MkdirAll(filepath.Join(home, "accounts", agent, name), 0o700))
	}
}

func TestRouteCreateAccountPicksAHealthyAccountOverAWalledDefault(t *testing.T) {
	home, repoPath, project := defaultAccountFixture(t, "codex", "codex1")
	registerAccounts(t, home, "codex", "codex2")
	writeProjectAccounts(t, project, "[default_accounts]\ncodex = \"codex1\"\n")
	stubAccountLimitEvidence(t, []session.AccountLimitObservationData{
		{Agent: "codex", Account: "codex1", ResetAt: time.Now().Add(time.Hour)},
	})

	req := CreateSessionRequest{Title: "routed", RepoPath: repoPath, Program: "codex"}
	require.NoError(t, (&Manager{}).routeCreateAccount(&config.Config{}, &req))
	assert.Equal(t, "codex2", req.Account,
		"a walled default is a preference, not a pin — the healthy registered account wins")
	assert.True(t, req.accountAutoSelected, "a router pick is marked as chosen, not pinned")
}

func TestRouteCreateAccountKeepsAHealthyDefault(t *testing.T) {
	home, repoPath, project := defaultAccountFixture(t, "codex", "codex1")
	registerAccounts(t, home, "codex", "codex2")
	writeProjectAccounts(t, project, "[default_accounts]\ncodex = \"codex1\"\n")
	stubAccountLimitEvidence(t, nil)

	req := CreateSessionRequest{Title: "defaulted", RepoPath: repoPath, Program: "codex"}
	require.NoError(t, (&Manager{}).routeCreateAccount(&config.Config{}, &req))
	assert.Equal(t, "codex1", req.Account, "the configured default is honored while it is healthy")
	assert.True(t, req.accountAutoSelected)
	assert.Contains(t, req.AccountSource, "default_accounts.codex")
}

func TestRouteCreateAccountRoutesWithNoDefaultConfigured(t *testing.T) {
	home, repoPath, _ := defaultAccountFixture(t, "codex", "codex1")
	registerAccounts(t, home, "codex", "codex2")
	stubAccountLimitEvidence(t, nil)

	req := CreateSessionRequest{Title: "pooled", RepoPath: repoPath, Program: "codex"}
	require.NoError(t, (&Manager{}).routeCreateAccount(&config.Config{}, &req))
	assert.Equal(t, "codex1", req.Account, "equally healthy accounts spread in registration order")
	assert.True(t, req.accountAutoSelected)
}

func TestRouteCreateAccountSpreadsAwayFromBusierAccounts(t *testing.T) {
	home, repoPath, _ := defaultAccountFixture(t, "codex", "codex1")
	registerAccounts(t, home, "codex", "codex2")
	stubAccountLimitEvidence(t, nil)

	busy, err := session.NewInstance(session.InstanceOptions{
		Title: "busy", Path: repoPath, Program: "codex", Account: "codex1",
	})
	require.NoError(t, err)
	m := &Manager{instances: map[string]*session.Instance{"k": busy}}

	req := CreateSessionRequest{Title: "spread", RepoPath: repoPath, Program: "codex"}
	require.NoError(t, m.routeCreateAccount(&config.Config{}, &req))
	assert.Equal(t, "codex2", req.Account, "the session running under codex1 counts against it")
}

func TestRouteCreateAccountRefusesWhenEveryAccountIsWalled(t *testing.T) {
	earliest := time.Now().Add(time.Hour)
	home, repoPath, _ := defaultAccountFixture(t, "codex", "codex1")
	registerAccounts(t, home, "codex", "codex2")
	stubAccountLimitEvidence(t, []session.AccountLimitObservationData{
		{Agent: "codex", Account: "codex1", ResetAt: earliest},
		{Agent: "codex", Account: "codex2", ResetAt: earliest.Add(48 * time.Hour)},
	})

	req := CreateSessionRequest{Title: "walled", RepoPath: repoPath, Program: "codex"}
	err := (&Manager{}).routeCreateAccount(&config.Config{}, &req)
	require.Error(t, err, "launching into a known wall is worse than failing the create")
	assert.Contains(t, err.Error(), "codex1", "the refusal reports the earliest reset's account")
	assert.Empty(t, req.Account, "nothing is applied when no account can be honored")
}

func TestRouteCreateAccountLeavesAnExplicitAccountAlone(t *testing.T) {
	home, repoPath, project := defaultAccountFixture(t, "codex", "codex1")
	registerAccounts(t, home, "codex", "codex2")
	writeProjectAccounts(t, project, "[default_accounts]\ncodex = \"codex1\"\n")
	stubAccountLimitEvidence(t, []session.AccountLimitObservationData{
		{Agent: "codex", Account: "chosen", ResetAt: time.Now().Add(time.Hour)},
	})

	req := CreateSessionRequest{Title: "explicit", RepoPath: repoPath, Program: "codex", Account: "chosen"}
	require.NoError(t, (&Manager{}).routeCreateAccount(&config.Config{}, &req))
	assert.Equal(t, "chosen", req.Account,
		"an explicit --account is a pin: the router neither rewrites nor second-guesses it")
	assert.False(t, req.accountAutoSelected)
}

func TestRouteCreateAccountHonorsTheAmbientOptOut(t *testing.T) {
	home, repoPath, project := defaultAccountFixture(t, "codex", "codex1")
	registerAccounts(t, home, "codex", "codex2")
	writeProjectAccounts(t, project, "[default_accounts]\ncodex = \"\"\n")
	stubAccountLimitEvidence(t, nil)

	req := CreateSessionRequest{Title: "opted-out", RepoPath: repoPath, Program: "codex"}
	require.NoError(t, (&Manager{}).routeCreateAccount(&config.Config{}, &req))
	assert.Empty(t, req.Account,
		"a present-but-empty entry means this project runs on the ambient identity")
}

func TestRouteCreateAccountKeepsAmbientWhenNoAccountsAreRegistered(t *testing.T) {
	_, repoPath, _ := defaultAccountFixture(t, "codex", "")
	stubAccountLimitEvidence(t, nil)

	req := CreateSessionRequest{Title: "ambient", RepoPath: repoPath, Program: "codex"}
	require.NoError(t, (&Manager{}).routeCreateAccount(&config.Config{}, &req))
	assert.Empty(t, req.Account, "no registry means no pool to route across")
}

// The web-selftest shape: program_overrides points the agent's label at a shim
// command, so the session does not actually RUN that agent — and the launch
// boundary refuses any account in the label's namespace. The router must see
// the same resolution and leave the create ambient.
func TestRouteCreateAccountDoesNotRouteACrossAgentOverride(t *testing.T) {
	home, repoPath, project := defaultAccountFixture(t, "codex", "codex1")
	registerAccounts(t, home, "codex", "codex2")
	writeProjectAccounts(t, project, "[program_overrides]\ncodex = \"/bin/fake-agent\"\n")
	stubAccountLimitEvidence(t, nil)

	req := CreateSessionRequest{Title: "shimmed", RepoPath: repoPath, Program: "codex"}
	require.NoError(t, (&Manager{}).routeCreateAccount(&config.Config{}, &req))
	assert.Empty(t, req.Account,
		"the label resolves to a non-codex command — no codex account can ride this launch")
	assert.False(t, req.accountAutoSelected)
}

func TestRouteCreateAccountAppliesTheDefaultOnANonCarryingBackend(t *testing.T) {
	_, repoPath, project := defaultAccountFixture(t, "codex", "codex1")
	writeProjectAccounts(t, project, "[default_accounts]\ncodex = \"codex1\"\n")

	req := CreateSessionRequest{Title: "remote", RepoPath: repoPath, Program: "codex", Backend: "ssh"}
	require.NoError(t, (&Manager{}).routeCreateAccount(&config.Config{}, &req))
	assert.Equal(t, "codex1", req.Account,
		"the configured default still applies — NewInstance's off-box refusal reports it by name")
	assert.False(t, req.accountAutoSelected,
		"and routing never ran, so the selection is not marked automatic")
}

// The stale-evidence half of #4404: a session answering under an account clears
// that account's stored observation — including the retained-ledger copy a
// deleted session left behind.
func TestRefuteAccountLimitEvidenceRetractsTheLedgerEntry(t *testing.T) {
	_, repoPath, _ := defaultAccountFixture(t, "codex", "codex4")
	reset := time.Now().Add(72 * time.Hour)
	require.NoError(t, retainAccountLimitObservations([]session.AccountLimitObservationData{
		{Agent: "codex", Account: "codex4", ResetAt: reset},
		{Agent: "codex", Account: "codex9", ResetAt: reset},
	}))

	instance, err := session.NewInstance(session.InstanceOptions{
		Title: "answered", Path: repoPath, Program: "codex", Account: "codex4",
	})
	require.NoError(t, err)
	instance.SetLimitReached(reset)
	instance.ClearLimitReached()
	require.Len(t, instance.AccountLimitObservations(), 1, "precondition: the stale wall is stored")

	_, epoch := instance.InFlightOpAndEpoch()
	m := &Manager{}
	require.True(t, m.refuteAccountLimitEvidence(instance, epoch),
		"the session's own row must drop the entry its success contradicts")
	require.Empty(t, instance.AccountLimitObservations())

	remaining, err := loadAccountLimitLedger()
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	require.Equal(t, "codex9", remaining[0].Account,
		"the retained ledger entry for the answered account is retracted; unrelated evidence stays")

	// A second affirmative tick does not rewrite the ledger.
	require.False(t, m.refuteAccountLimitEvidence(instance, epoch))
}
