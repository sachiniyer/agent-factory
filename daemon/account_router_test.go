package daemon

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
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

// registerAccounts creates registry directories WITH a credential artifact, so
// each name is a logged-in account — the state an automatic route may pick.
// Tests needing registered-but-not-logged-in accounts make the bare directory
// themselves (os.MkdirAll), which is exactly what LoggedIn reports false for.
func registerAccounts(t *testing.T, home, agent string, names ...string) {
	t.Helper()
	artifacts := agentaccount.AccountCredentialArtifacts(agent)
	require.NotEmpty(t, artifacts, "test agent %q has no known credential artifact", agent)
	for _, name := range names {
		dir := filepath.Join(home, "accounts", agent, name)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, artifacts[0]), []byte("{}"), 0o600))
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

// #4404 review: an explicit ambient choice is not an invitation to route. The
// picker's "use the agent's own login" row sends Account "" — which the router
// used to read as routable — so AccountAmbient carries the one bit the wire
// could not: "this empty was chosen".
func TestRouteCreateAccountHonorsAnExplicitAmbientPick(t *testing.T) {
	home, repoPath, project := defaultAccountFixture(t, "codex", "codex1")
	registerAccounts(t, home, "codex", "codex2")
	writeProjectAccounts(t, project, "[default_accounts]\ncodex = \"codex1\"\n")
	stubAccountLimitEvidence(t, nil)

	req := CreateSessionRequest{Title: "chose-ambient", RepoPath: repoPath, Program: "codex", AccountAmbient: true}
	require.NoError(t, (&Manager{}).routeCreateAccount(&config.Config{}, &req))
	assert.Empty(t, req.Account,
		"an ambient pick keeps the ambient identity — neither routed nor defaulted")
	assert.False(t, req.accountAutoSelected)
}

// #4404 review: registration is a directory name, not a launchable identity.
// Automatic routing onto an account with no credential in it would put the new
// session on an identity that cannot authenticate — silently, in place of the
// ambient login that always worked.
func TestRouteCreateAccountSkipsAccountsWithNoCredential(t *testing.T) {
	home, repoPath, _ := defaultAccountFixture(t, "codex", "codex1")
	// codex2 is registered but never logged in — a bare directory, exactly what
	// `af accounts add` leaves before `af accounts login` runs.
	require.NoError(t, os.MkdirAll(filepath.Join(home, "accounts", "codex", "codex2"), 0o700))
	stubAccountLimitEvidence(t, nil)

	req := CreateSessionRequest{Title: "logged-in-only", RepoPath: repoPath, Program: "codex"}
	require.NoError(t, (&Manager{}).routeCreateAccount(&config.Config{}, &req))
	assert.Equal(t, "codex1", req.Account,
		"the only account with a completed login is the only one an automatic pick may land on")
}

func TestRouteCreateAccountKeepsAnUnloggedInConfiguredDefault(t *testing.T) {
	home, repoPath, project := defaultAccountFixture(t, "codex", "")
	// The configured default is registered but has no credential yet — the
	// "runs as it until you log in" state its picker rows document. It is the
	// user's own pin, so it stays a candidate where an automatic pick would not.
	require.NoError(t, os.MkdirAll(filepath.Join(home, "accounts", "codex", "work"), 0o700))
	registerAccounts(t, home, "codex", "play")
	writeProjectAccounts(t, project, "[default_accounts]\ncodex = \"work\"\n")
	stubAccountLimitEvidence(t, nil)

	req := CreateSessionRequest{Title: "unlogged-default", RepoPath: repoPath, Program: "codex"}
	require.NoError(t, (&Manager{}).routeCreateAccount(&config.Config{}, &req))
	assert.Equal(t, "work", req.Account,
		"a configured default is an explicit pin — honored while healthy even before login")
}

func TestRouteCreateAccountStaysAmbientWhenNoAccountIsLoggedIn(t *testing.T) {
	home, repoPath, _ := defaultAccountFixture(t, "codex", "")
	require.NoError(t, os.MkdirAll(filepath.Join(home, "accounts", "codex", "work"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(home, "accounts", "codex", "play"), 0o700))
	stubAccountLimitEvidence(t, nil)

	req := CreateSessionRequest{Title: "all-unlogged", RepoPath: repoPath, Program: "codex"}
	require.NoError(t, (&Manager{}).routeCreateAccount(&config.Config{}, &req))
	assert.Empty(t, req.Account,
		"no credential anywhere means no automatic pick can produce a working session")
}

// #4404 review: the pick and its reservation must be one critical section.
// Before the claim existed, two concurrent creates both read the same
// least-loaded snapshot and both took the same account, because the pending row
// that would have counted the first pick published long after.
func TestClaimCreateAccountSpreadsConcurrentCreates(t *testing.T) {
	m := &Manager{}
	limited := map[string]time.Time{}
	pool := []string{"codex1", "codex2"}

	first, claim1, err := m.claimCreateAccount("codex", pool, limited, "")
	require.NoError(t, err)
	second, claim2, err := m.claimCreateAccount("codex", pool, limited, "")
	require.NoError(t, err)
	assert.NotEqual(t, first, second,
		"the first claim counts against its account before the second pick runs")

	m.releaseCreateAccountClaim(claim1)
	m.releaseCreateAccountClaim(claim2)
	third, _, err := m.claimCreateAccount("codex", pool, limited, "")
	require.NoError(t, err)
	assert.Equal(t, pool[0], third,
		"released claims stop counting — the pool is equally loaded again")
}

func TestRefuteAccountLimitEvidenceRetiresSiblingRowsToo(t *testing.T) {
	_, repoPath, project := defaultAccountFixture(t, "codex", "codex4")
	reset := time.Now().Add(72 * time.Hour)

	answered, err := session.NewInstance(session.InstanceOptions{
		Title: "answered", Path: repoPath, Program: "codex", Account: "codex4",
	})
	require.NoError(t, err)
	answered.SetLimitReached(reset)
	answered.ClearLimitReached()
	require.Len(t, answered.AccountLimitObservations(), 1, "precondition: the stale wall is stored")

	// A SIBLING session carries the same {agent, account} wall evidence — the
	// shape a deleted session's row used to leave behind, and the one that kept
	// the account walled after the reporting session's own row was cleared.
	sibling, err := session.NewInstance(session.InstanceOptions{
		Title: "sibling", Path: repoPath, Program: "codex", Account: "codex4",
	})
	require.NoError(t, err)
	sibling.SetLimitReached(reset)
	require.Len(t, sibling.AccountLimitObservations(), 1)
	m := &Manager{
		instances:      map[string]*session.Instance{daemonInstanceKey(project.ID, "sibling"): sibling},
		repoStartLocks: map[string]*sync.Mutex{},
	}

	_, epoch := answered.InFlightOpAndEpoch()
	require.True(t, m.refuteAccountLimitEvidence(answered, epoch))
	assert.Empty(t, answered.AccountLimitObservations())
	assert.Empty(t, sibling.AccountLimitObservations(),
		"the refuted identity's evidence retires on every session that carries it, not just the reporter's")
	assert.True(t, sibling.LimitReached(),
		"but a sibling's LIVE wall is its own claim — it clears on the sibling's own work, not another's")
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
