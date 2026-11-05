package commands

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/printsafety"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/quota"
)

// `af accounts handoff` is the operator surface for the automatic account
// handoff's durable rails (#6776 slice 5): `status` reads them, `reset`
// clears the one record the operator names.
//
// Both verbs are daemon calls, never direct file access: the state file is
// the daemon's authority — only it knows whether the engine is engaged — and
// a CLI that edited it behind the daemon's back would race the report worker
// and the mover's own bookkeeping. With --daemon-url (or AF_DAEMON_URL) the
// call routes to the targeted daemon over the authenticated HTTP API, so the
// answer describes — and the reset lands on — the machine the flag names.

var accountsHandoffCmd = &cobra.Command{
	Use:   "handoff",
	Short: "Inspect and reset the automatic account-handoff rails",
	Long: `Inspect and reset the durable rails the automatic account handoff keeps.

status shows the resolved mode, whether the daemon's engine is observing, the
recorded halt, refusal backoffs, and the recent-move ledger the switch-loop
rail and move credits count. reset clears the ONE record you name — halt,
backoff, or moves — and nothing else, so clearing a halt can never silently
take the loop history or a held report with it.

The rails live in the daemon host's af home; both verbs go through the daemon
API. With --daemon-url (or AF_DAEMON_URL) they act on the targeted daemon.`,
	// No RunE: bare `af accounts handoff` prints help like the other group
	// commands. It is also the flag-error envelope route for the group.
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return cmd.Help()
	},
}

var accountsHandoffStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the automatic account-handoff rails state",
	Long: `Show the durable rails the automatic account handoff keeps:

the resolved account_handoff.mode (absent table ≡ off ≡ no machinery), whether
the daemon's engine is observing, the recorded mover halt, refusal backoffs,
and the recent-move ledger — the records the switch-loop rail (max_moves inside
max_moves_window) and the anti-pile-on move credits count.

The records are keyed by stable session identity, never by title; a live
session's title is attached only as a label.`,
	Args: cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		if len(args) != 0 {
			return jsonWrapError(cmd, accountsJSONFlag,
				fmt.Errorf("af accounts handoff status takes no arguments (got %d)", len(args)))
		}
		resp, err := handoffStatusViaDaemon()
		if err != nil {
			return jsonWrapError(cmd, accountsJSONFlag, err)
		}
		if accountsJSONFlag {
			return apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(resp))
		}
		printHandoffStatus(cmd, resp)
		return nil
	},
}

var accountsHandoffResetSessionFlag string

var accountsHandoffResetCmd = &cobra.Command{
	Use:   "reset <halt|backoff|moves>",
	Short: "Clear one named automatic handoff record",
	Long: `Clear one durable rail record, and only the one named:

  af accounts handoff reset halt                    clear the recorded mover halt
  af accounts handoff reset backoff [--session K]   clear refusal backoffs
  af accounts handoff reset moves   [--session K]   clear recent-move entries

--session scopes backoff and moves to one stable session key (see
` + "`af accounts handoff status`" + `); halt is mover-level and takes none.
A reset that matches nothing reports so — it is not an error. The change lands
through the daemon, on the daemon the command targets.`,
	Args: cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		if len(args) != 1 {
			return jsonWrapError(cmd, accountsJSONFlag,
				fmt.Errorf("af accounts handoff reset takes exactly one record — halt, backoff, or moves (got %d)", len(args)))
		}
		resp, err := handoffResetViaDaemon(daemon.HandoffResetRequest{
			Record:     args[0],
			SessionKey: accountsHandoffResetSessionFlag,
		})
		if err != nil {
			return jsonWrapError(cmd, accountsJSONFlag, err)
		}
		if accountsJSONFlag {
			return apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(resp))
		}
		fmt.Fprintln(cmd.OutOrStdout(), resp.Summary)
		return nil
	},
}

// handoffStatusViaDaemon routes the read: the local control socket for this
// host, the authenticated HTTP API for a --daemon-url/AF_DAEMON_URL target —
// the same fork `af quota` makes, so the answer always describes the daemon
// the command is pointed at.
func handoffStatusViaDaemon() (daemon.HandoffStatusResponse, error) {
	if apiclient.IsRemoteTarget() {
		client, err := apiclient.NewTargeted()
		if err != nil {
			return daemon.HandoffStatusResponse{}, err
		}
		defer client.CloseIdleConnections()
		return client.HandoffStatus()
	}
	return daemon.HandoffStatus(daemon.HandoffStatusRequest{})
}

// handoffResetViaDaemon routes the mutation the same way as the read. A
// remote call is ONE request — the API client does not replay a transport
// that died mid-answer, so an uncertain outcome is surfaced to the operator
// rather than silently resent.
func handoffResetViaDaemon(req daemon.HandoffResetRequest) (daemon.HandoffResetResponse, error) {
	if apiclient.IsRemoteTarget() {
		client, err := apiclient.NewTargeted()
		if err != nil {
			return daemon.HandoffResetResponse{}, err
		}
		defer client.CloseIdleConnections()
		return client.HandoffReset(req)
	}
	return daemon.HandoffReset(req)
}

// printHandoffStatus renders the served response. The mover line reuses
// quota.MoverStatus's vocabulary so `af accounts handoff status` and
// `af quota` can never word the same halt differently.
func printHandoffStatus(cmd *cobra.Command, resp daemon.HandoffStatusResponse) {
	mover := quota.MoverStatus{
		Mode:        resp.Mode,
		ModeUnknown: resp.ModeUnknown,
		Halted:      resp.Halted,
		HaltReason:  resp.HaltReason,
	}
	if resp.HaltAt != nil {
		mover.HaltAt = *resp.HaltAt
	}
	if resp.ReportsHeldSince != nil {
		mover.ReportsHeldSince = *resp.ReportsHeldSince
	}
	fmt.Fprintf(cmd.OutOrStdout(), "mover: %s\n", mover)
	if resp.Engaged {
		fmt.Fprintln(cmd.OutOrStdout(), "engine: observing")
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), "engine: not observing")
	}
	if len(resp.Backoffs) > 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "refusal backoffs:")
		for _, b := range resp.Backoffs {
			fmt.Fprintf(cmd.OutOrStdout(), "  %s  until %s\n",
				handoffSessionLabel(b.SessionKey, b.Title), b.Until.UTC().Format(time.RFC3339))
		}
	}
	if len(resp.RecentMoves) > 0 {
		header := "recent moves"
		if resp.MaxMoves > 0 && resp.MaxMovesWindow != "" {
			header = fmt.Sprintf("recent moves (max %d in %s)", resp.MaxMoves, resp.MaxMovesWindow)
		}
		fmt.Fprintln(cmd.OutOrStdout(), header+":")
		for _, mv := range resp.RecentMoves {
			target := printsafety.Field(mv.Account)
			if mv.Agent != "" {
				target = printsafety.Field(mv.Agent) + "/" + target
			}
			fmt.Fprintf(cmd.OutOrStdout(), "  %s  → %s  at %s\n",
				handoffSessionLabel(mv.SessionKey, mv.Title), target, mv.At.UTC().Format(time.RFC3339))
		}
	}
	for _, warning := range resp.Warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warning)
	}
}

// handoffSessionLabel prints the stable key with the live session's title as
// a label — never the reverse: the key is the identity the rail recorded.
// Both fields are stored strings, so they pass printsafety on the way out —
// the local print path applies the same strip the daemon applies
// server-side.
func handoffSessionLabel(key, title string) string {
	key = printsafety.Field(key)
	if title == "" {
		return key
	}
	return fmt.Sprintf("%s (%s)", key, printsafety.Field(title))
}

func init() {
	accountsHandoffStatusCmd.Flags().BoolVar(&accountsJSONFlag, "json", false, "Output the {data,error} JSON envelope")
	accountsHandoffResetCmd.Flags().BoolVar(&accountsJSONFlag, "json", false, "Output the {data,error} JSON envelope")
	accountsHandoffResetCmd.Flags().StringVar(&accountsHandoffResetSessionFlag, "session", "",
		"Scope backoff/moves to one stable session key")
	accountsHandoffStatusCmd.SetFlagErrorFunc(accountsFlagError)
	accountsHandoffResetCmd.SetFlagErrorFunc(accountsFlagError)
	accountsHandoffCmd.SetFlagErrorFunc(accountsFlagError)
	accountsHandoffCmd.AddCommand(accountsHandoffStatusCmd)
	accountsHandoffCmd.AddCommand(accountsHandoffResetCmd)
	accountsCmd.AddCommand(accountsHandoffCmd)
}
