package session

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

// OffBoxSwapFixture describes the "host" InstallOffBoxSwapBackendForTest has
// the instance impersonate: the session dir the provisioner's scripts run
// against, the synced account the workspace carries, and the listed host the
// workspace was provisioned under. It exists so a daemon-package test can
// drive the automatic account-swap route through the real production checks —
// the provisioner, the account workspace, the backend, and the cached
// agent-server feature set are all deliberately unexported — rather than
// stubbing each of them away.
type OffBoxSwapFixture struct {
	// Endpoint is the test's stand-in agent-server. The swap's agent-server
	// calls reach it over real HTTP, so its /v1/health must advertise the
	// feature tokens the swap route checks (account-swap and friends); the
	// installer warms the cached feature set idNative records.
	Endpoint AgentServerEndpoint
	// SessionDir is the per-session dir the host would hold. The fixture's
	// scripts run against it directly, so staged and live scopes land there.
	SessionDir string
	// Account is the synced account the workspace carries (spec.Account). Its
	// Dir must hold the registry credential files the incoming stage copies.
	Account sessionenv.Account
	// SyncHost names the LISTED host the workspace was provisioned on and
	// SyncTarget is that host's global-config entry — the same pair
	// ProvisionSpec.AccountSyncHost/AccountSyncTarget record at provision
	// time. The send licence re-resolves SyncHost in the global config and
	// compares it to the recorded target, so the test must save a config
	// whose [hosts.<SyncHost>] entry is SyncTarget.
	SyncHost   string
	SyncTarget config.HostConfig
	// AutoSelected is the row's accountAutoSelected bit: the scheduler never
	// overrides an operator's --account pin, so a limit-failover test needs
	// true for swap candidates to apply.
	AutoSelected bool
	// Run executes one remote step's script, the way the ssh transport's
	// runCommand would on the host. It is REQUIRED and lives in the caller's
	// _test.go file: a compiled fixture must not add a shell-invocation site
	// to production code (the shell-sites gate classifies every one).
	Run func(timeout time.Duration, script string, stdin io.Reader, combined bool) ([]byte, error)
}

// InstallOffBoxSwapBackendForTest puts the off-box shape a daemon test needs
// on i: an ssh backend whose provisioner carries an account workspace synced
// under a listed host, the recorded account identity fields, a remote agent
// tab, and the bound agent-server client with a warm feature cache. It is
// test scaffolding in the SetStartedForTest / SetOffBoxSwapInFlightForTest /
// SetLookPathForTest family — exported so the daemon package can reach the
// unexported pieces; production code must not call it.
func (i *Instance) InstallOffBoxSwapBackendForTest(fx OffBoxSwapFixture) error {
	if fx.SessionDir == "" || fx.Account.Dir == "" || strings.TrimSpace(fx.SyncHost) == "" {
		return fmt.Errorf("an off-box swap fixture needs a session dir, a synced account dir, and a listed-host name")
	}
	if fx.Run == nil {
		return fmt.Errorf("an off-box swap fixture needs the caller's remote-step runner")
	}
	p := &sandboxProvisioner{sessionDir: fx.SessionDir, runCommandFn: fx.Run}
	p.account = &sandboxWorkspace{
		shell:      p,
		SessionDir: fx.SessionDir,
		spec: ProvisionSpec{
			Account:           fx.Account,
			AccountSyncHost:   fx.SyncHost,
			AccountSyncTarget: accountSyncTargetOf(sshConfigForHost(fx.SyncTarget)),
			Program:           i.Program,
		},
	}
	// The shape provisioning leaves behind: the live scope synced from the
	// registry dir, the ssh backend carrying the workspace, and the bound
	// agent-server client the swap's calls ride.
	if err := p.account.syncAccountIn(time.Minute); err != nil {
		return fmt.Errorf("the fixture's account sync-in failed: %w", err)
	}
	i.SetBackend(&sshBackend{
		provisioner: p,
		cleanup: &SSHRuntimeCleanupData{
			SessionDir:  fx.SessionDir,
			HostName:    fx.SyncHost,
			DialAddress: fx.SyncTarget.Host,
		},
	})
	rc, err := newRemoteAgentClient(fx.Endpoint, i.Title)
	if err != nil {
		return fmt.Errorf("the fixture's agent-server endpoint: %w", err)
	}
	i.mu.Lock()
	i.remoteClient = rc
	i.Account = fx.Account.Name
	i.accountAgent = fx.Account.Agent
	i.accountAutoSelected = fx.AutoSelected
	if len(i.Tabs) == 0 {
		i.Tabs = []*Tab{newRemoteAgentTab()}
	}
	i.mu.Unlock()
	rs, ok := i.AgentServer().(*remoteAgentServer)
	if !ok {
		return fmt.Errorf("the fixture could not bind a remote agent-server for %q", i.Title)
	}
	// Warm the cached feature set: offBoxAccountSwapAvailable reads the cache
	// only, so a server whose first answer never arrived would read as
	// featureless even though the test's health route advertises the tokens.
	rs.idNative()
	return nil
}
