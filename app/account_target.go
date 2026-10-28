package app

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
)

// Accounts follows the config pane's attached daemon (#3708, #3950).
// Remote failures never fall back to the local control socket.
var (
	localListAccounts    = daemon.ListAccounts
	localRegisterAccount = daemon.RegisterAccount
	localAccountLogin    = daemon.AccountLogin
)

func targetedListAccountsForPane(req daemon.ListAccountsRequest) (daemon.ListAccountsResponse, error) {
	if !apiclient.IsRemoteTarget() {
		return localListAccounts(req)
	}
	client, err := apiclient.NewTargeted()
	if err != nil {
		return daemon.ListAccountsResponse{}, err
	}
	defer client.CloseIdleConnections()
	return client.ListAccounts(req.Agent, req.RepoPath)
}

func targetedRegisterAccount(req daemon.RegisterAccountRequest) (daemon.RegisterAccountResponse, error) {
	if !apiclient.IsRemoteTarget() {
		return localRegisterAccount(req)
	}
	client, err := apiclient.NewTargeted()
	if err != nil {
		return daemon.RegisterAccountResponse{}, err
	}
	defer client.CloseIdleConnections()
	return client.RegisterAccount(req.Agent, req.Name)
}

func targetedAccountLogin(req daemon.AccountLoginRequest) (daemon.AccountLoginResponse, error) {
	if reason := remoteAccountLoginRefusal(); reason != "" {
		return daemon.AccountLoginResponse{}, fmt.Errorf("%s", reason)
	}
	return localAccountLogin(req)
}

func remoteAccountLoginRefusal() string {
	if !apiclient.IsRemoteTarget() {
		return ""
	}
	// Keep the CLI's explanation: the tmux terminal lives on the daemon host.
	return fmt.Sprintf("af accounts login runs the agent's login flow on the machine whose agent-factory home holds "+
		"the account, and hands you that pane's terminal; it cannot do that against a remote daemon. "+
		"Unset --daemon-url/AF_DAEMON_URL to act on this host, or run af accounts login on the "+
		"daemon's host (%s)", apiclient.RemoteTargetURL())
}
