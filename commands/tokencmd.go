package commands

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"

	"github.com/spf13/cobra"
)

// `af token` manages the daemon's bearer token — the credential for the
// direct-TCP/TLS API surface (#1592 Phase 3). Under the locked auth model one
// token = full access, single-owner. The token authenticates the TLS TCP
// listener (enabled with the listen_addr config key); the material can be
// generated, inspected, and rotated independently of enabling the listener. The
// local unix socket is never affected — its filesystem 0600 perms are the local
// auth (#1029). See docs/remote-tcp-auth.md for the end-to-end flow.

// tokenJSONFlag switches `af token show/rotate` from human output to the shared
// {data,error} envelope, matching `af config`'s --json.
var tokenJSONFlag bool

// tokenShowResult is the `af token show` payload: the bearer token plus the TLS
// fingerprint a client TOFU-pins for a self-signed daemon cert (§1.2).
type tokenShowResult struct {
	Token          string `json:"token"`
	TLSFingerprint string `json:"tls_fingerprint"`
}

// tokenRotateResult is the `af token rotate` payload: the freshly generated
// token. The fingerprint is unchanged by rotation (it depends on the cert, not
// the token), so rotate does not reprint it.
type tokenRotateResult struct {
	Token string `json:"token"`
}

// resolveTLSFingerprint resolves the daemon's TLS material (user-provided cert
// via tls_cert/tls_key, else the self-generated self-signed cert) and returns
// its SHA-256 fingerprint. Resolving self-generates the cert on first use, so
// `af token show` materializes both the token and the cert even before the
// listener is enabled.
func resolveTLSFingerprint() (string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	cfg, err := config.LoadConfig()
	if err != nil {
		return "", err
	}
	material, err := daemon.ResolveTLSMaterial(dir, cfg.TLSCert, cfg.TLSKey)
	if err != nil {
		return "", err
	}
	return daemon.CertFingerprint(material.CertPath)
}

var tokenCmd = &cobra.Command{
	Use:   "token",
	Short: "Manage the daemon's bearer token for the direct-TCP API",
	Long: `Manage the bearer token that authenticates the daemon's direct-TCP/TLS API.

The token grants full access under the single-owner auth model. It is only used
by the TCP listener (enabled with the listen_addr config key); the local unix
socket stays unauthenticated (its 0600 filesystem perms are the local auth).
The token and the self-signed TLS cert are stored in the af home
(~/.agent-factory) with 0600 permissions on the secret files.`,
}

var tokenShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Print the bearer token and TLS fingerprint (generating them if absent)",
	Long: `Print the daemon's bearer token and its TLS certificate fingerprint.

Both are generated on first access if they do not yet exist, so this is safe to
run before the TCP listener is ever enabled. The fingerprint is the SHA-256 a
remote client pins (TOFU) when the daemon uses its self-signed certificate; when
a CA-issued certificate is configured via tls_cert/tls_key it is that
certificate's fingerprint (clients verify it against system roots instead).`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		tokenPath, err := daemon.TokenPath()
		if err != nil {
			return jsonWrapError(cmd, tokenJSONFlag, err)
		}
		token, err := daemon.EnsureToken(tokenPath)
		if err != nil {
			return jsonWrapError(cmd, tokenJSONFlag, err)
		}
		fingerprint, err := resolveTLSFingerprint()
		if err != nil {
			return jsonWrapError(cmd, tokenJSONFlag, err)
		}

		result := tokenShowResult{Token: token, TLSFingerprint: fingerprint}
		if tokenJSONFlag {
			return apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(result))
		}
		fmt.Fprintf(cmd.OutOrStdout(), "token:           %s\n", result.Token)
		fmt.Fprintf(cmd.OutOrStdout(), "tls_fingerprint: %s\n", result.TLSFingerprint)
		return nil
	},
}

var tokenRotateCmd = &cobra.Command{
	Use:   "rotate",
	Short: "Replace the bearer token with a fresh one and print it",
	Long: `Generate a new bearer token, persist it (overwriting the old one), and print it.

Rotation takes effect for new connections immediately — the auth gate re-reads
the token file per request — while any in-flight streams keep running until they
reconnect. The TLS fingerprint is unaffected (it depends on the certificate, not
the token).`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		tokenPath, err := daemon.TokenPath()
		if err != nil {
			return jsonWrapError(cmd, tokenJSONFlag, err)
		}
		token, err := daemon.RotateToken(tokenPath)
		if err != nil {
			return jsonWrapError(cmd, tokenJSONFlag, err)
		}

		result := tokenRotateResult{Token: token}
		if tokenJSONFlag {
			return apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(result))
		}
		fmt.Fprintf(cmd.OutOrStdout(), "token: %s\n", result.Token)
		return nil
	},
}

func init() {
	const jsonUsage = "Emit the result as JSON wrapped in the {data,error} envelope"
	tokenShowCmd.Flags().BoolVar(&tokenJSONFlag, "json", false, jsonUsage)
	tokenRotateCmd.Flags().BoolVar(&tokenJSONFlag, "json", false, jsonUsage)
	tokenCmd.AddCommand(tokenShowCmd)
	tokenCmd.AddCommand(tokenRotateCmd)
}
