package commands

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"

	"github.com/spf13/cobra"
)

// `af token` manages the daemon's bearer token — the optional credential for the
// direct-TCP HTTP API surface (#1592 Phase 3). Under the locked auth model one
// token = full access, single-owner. The token authenticates the HTTP TCP
// listener (enabled with the listen_addr config key); the material can be
// generated, inspected, and rotated independently of enabling the listener. The
// local unix socket is never affected — its filesystem 0600 perms are the local
// auth (#1029). The listener is plain HTTP (no TLS): the token travels over the
// connection, so put the listener behind a reverse proxy or private network if
// you expose it. See docs/remote-http-auth.md for the end-to-end flow.

// tokenJSONFlag switches `af token show/rotate` from human output to the shared
// {data,error} envelope, matching `af config`'s --json.
var tokenJSONFlag bool

// tokenResult is the `af token show`/`rotate` payload: the bearer token clients
// present to the HTTP listener.
type tokenResult struct {
	Token string `json:"token"`
}

// runTokenCommand performs the shared token command work. Both show and rotate
// differ only in how they obtain the token; their output and error contracts are
// intentionally identical.
func runTokenCommand(cmd *cobra.Command, loadToken func(string) (string, error)) error {
	log.Initialize(false)
	defer log.Close()

	tokenPath, err := daemon.TokenPath()
	if err != nil {
		return jsonWrapError(cmd, tokenJSONFlag, err)
	}
	token, err := loadToken(tokenPath)
	if err != nil {
		return jsonWrapError(cmd, tokenJSONFlag, err)
	}

	result := tokenResult{Token: token}
	if tokenJSONFlag {
		return apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(result))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "token: %s\n", result.Token)
	return nil
}

var tokenCmd = &cobra.Command{
	Use:   "token",
	Short: "Manage the daemon's bearer token for the direct-TCP API",
	Long: `Manage the bearer token that authenticates the daemon's direct-TCP HTTP API.

The token grants full access under the single-owner auth model. It is only used
by the TCP listener (enabled with the listen_addr config key); the local unix
socket stays unauthenticated (its 0600 filesystem perms are the local auth).
The token is stored in the af home (~/.agent-factory) with 0600 permissions.

The listener serves plain HTTP — af terminates no TLS of its own. The token
travels over the connection, so expose the listener only behind a reverse proxy
(nginx/caddy) or on a private network (Tailscale/VPN/SSH tunnel).`,
}

var tokenShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Print the bearer token (generating it if absent)",
	Long: `Print the daemon's bearer token.

It is generated on first access if it does not yet exist, so this is safe to run
before the TCP listener is ever enabled. Present it to a remote daemon with the
--token flag (or the AF_DAEMON_TOKEN env var).`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runTokenCommand(cmd, daemon.EnsureToken)
	},
}

var tokenRotateCmd = &cobra.Command{
	Use:   "rotate",
	Short: "Replace the bearer token with a fresh one and print it",
	Long: `Generate a new bearer token, persist it (overwriting the old one), and print it.

Rotation takes effect for new connections immediately — the auth gate re-reads
the token file per request — while any in-flight streams keep running until they
reconnect.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runTokenCommand(cmd, daemon.RotateToken)
	},
}

func init() {
	const jsonUsage = "Emit the result as JSON wrapped in the {data,error} envelope"
	tokenShowCmd.Flags().BoolVar(&tokenJSONFlag, "json", false, jsonUsage)
	tokenRotateCmd.Flags().BoolVar(&tokenJSONFlag, "json", false, jsonUsage)
	tokenCmd.AddCommand(tokenShowCmd)
	tokenCmd.AddCommand(tokenRotateCmd)
}
