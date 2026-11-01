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
	hostsResetTrustAll bool
	hostsResetTrust    = daemon.ResetPushTrust
)

var hostsResetTrustCmd = &cobra.Command{
	Use:   "reset-trust <name>",
	Short: "Clear a host's push-credential taint after recreating or wiping its user",
	Long: `Clear the push-credential taint on <name>'s machine and ssh user.

A host whose user has run code you didn't place (a session started from inside
another session, a repository's own [ssh] target, a create that failed after
its hooks ran) gives no push credentials, because a process that code left
behind can outlive every session. Run this only after you recreated or wiped
that host user. It clears the taints known to be on that machine and user, and
prints the identities it reset. <name> is a listed host, or sandbox.ssh for the
free-form target. --all clears every taint, including ones af couldn't place on
any machine: use it only once you have reset every host user they could have
reached. See "Push credentials" in docs/backends.md.`,
	Example: `  af hosts reset-trust build
  af hosts reset-trust sandbox.ssh
  af hosts reset-trust --all`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := requireLocalTarget("af hosts reset-trust", "clears this machine's daemon's push-credential taints"); err != nil {
			return hostsFailJSON(cmd, err)
		}
		req := daemon.ResetPushTrustRequest{All: hostsResetTrustAll}
		switch {
		case len(args) == 1 && hostsResetTrustAll:
			return hostsFailJSON(cmd, errors.New("name a host or pass --all, not both"))
		case len(args) == 1:
			req.Host = strings.TrimSpace(args[0])
		case !hostsResetTrustAll:
			return hostsFailJSON(cmd, errors.New("name the host whose user you recreated or wiped, or pass --all"))
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
	if req.All {
		target = "every host"
	} else {
		fmt.Fprintf(w, "reset trust in %s: names %s; addresses %s; user %s\n", target,
			listOrNone(resp.Identity.Names), listOrNone(resp.Identity.Addrs), orAny(resp.Identity.User))
	}
	if len(resp.Cleared) == 0 {
		fmt.Fprintf(w, "no taint was recorded on %s\n", target)
		return
	}
	for _, t := range resp.Cleared {
		fmt.Fprintf(w, "  cleared: session %q (%s), %s, tainted since %s\n", t.Title, t.SessionID, t.Why, t.Since.UTC().Format("2006-01-02T15:04:05Z"))
	}
}

func listOrNone(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, ", ")
}

func orAny(user string) string {
	if user == "" {
		return "any"
	}
	return user
}

func init() {
	hostsResetTrustCmd.Flags().BoolVar(&hostsResetTrustAll, "all", false, "Clear every taint (only once every host user they could have reached is reset)")
	hostsCmd.AddCommand(hostsResetTrustCmd)
}
