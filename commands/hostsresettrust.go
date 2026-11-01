package commands

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/daemon"
)

// `af hosts reset-trust` (#5571 W7.8, design steer on #6433): the operator's
// attestation that a host user was recreated or wiped, which is the only thing
// that clears the push-credential taint code the operator didn't place leaves on
// a machine.

var (
	hostsResetTrustAll   bool
	hostsResetTrustEntry string
	hostsResetTrust      = daemon.ResetPushTrust
)

var hostsResetTrustCmd = &cobra.Command{
	Use:   "reset-trust <name>",
	Short: "Clear a host's push-credential taint after recreating or wiping its users",
	Long: `Clear the push-credential taint on <name>'s machine.

A machine that has run code you didn't place (a session started from inside
another session, a repository's own [ssh] target, a create that failed after
its hooks ran) gives no push credentials, because a process that code left
behind can outlive every session, and any login on the machine able to sudo can
reach every other login's files. Run this only after you recreated or wiped the
machine's users. It clears the taints known to be only on that machine (every
address they recorded is one of its own), and prints the identities it reset.
<name> is a listed host, or sandbox.ssh for the free-form target. --entry <id>
clears one taint by the id ` + "`af hosts check`" + ` shows; --all clears every taint. See
"Push credentials" in docs/backends.md.`,
	Example: `  af hosts reset-trust build
  af hosts reset-trust sandbox.ssh
  af hosts reset-trust --entry 3f9a1c0b2d4e
  af hosts reset-trust --all`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := requireLocalTarget("af hosts reset-trust", "clears this machine's daemon's push-credential taints"); err != nil {
			return hostsFailJSON(cmd, err)
		}
		req := daemon.ResetPushTrustRequest{All: hostsResetTrustAll, Entry: strings.TrimSpace(hostsResetTrustEntry)}
		if len(args) == 1 {
			req.Host = strings.TrimSpace(args[0])
		}
		chosen := 0
		for _, set := range []bool{req.Host != "", req.Entry != "", req.All} {
			if set {
				chosen++
			}
		}
		if chosen != 1 {
			return hostsFailJSON(cmd, errors.New("name exactly one of: the host whose users you recreated or wiped, --entry <id>, or --all"))
		}
		resp, err := hostsResetTrust(req)
		if err != nil {
			return hostsFailJSON(cmd, err)
		}
		if hostsJSONFlag {
			return apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(resp))
		}
		renderResetTrust(cmd.OutOrStdout(), req, resp)
		return nil
	},
}

func renderResetTrust(w io.Writer, req daemon.ResetPushTrustRequest, resp daemon.ResetPushTrustResponse) {
	target := req.Host
	switch {
	case req.All:
		target = "every host"
	case req.Entry != "":
		target = "entry " + req.Entry
	default:
		fmt.Fprintf(w, "reset trust in %s: names %s; addresses %s\n", target, listOrNone(resp.Identity.Names), listOrNone(resp.Identity.Addrs))
	}
	if len(resp.Cleared) == 0 {
		fmt.Fprintf(w, "no taint was recorded on %s\n", target)
		return
	}
	for _, t := range resp.Cleared {
		fmt.Fprintf(w, "  cleared entry %s: session %q (%s), %s, tainted since %s\n", t.ID, t.Title, t.SessionID, t.Why, t.Since.UTC().Format("2006-01-02T15:04:05Z"))
	}
}

func listOrNone(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, ", ")
}

func init() {
	hostsResetTrustCmd.Flags().BoolVar(&hostsResetTrustAll, "all", false, "Clear every taint (only once every machine they could have reached is reset)")
	hostsResetTrustCmd.Flags().StringVar(&hostsResetTrustEntry, "entry", "", "Clear one taint by the id `af hosts check` shows")
	hostsCmd.AddCommand(hostsResetTrustCmd)
}
