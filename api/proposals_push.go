package api

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/proposal"
)

var proposalPushModeCmd = &cobra.Command{
	Use:   "push-mode auto|manual",
	Short: "Set whether the Proposal tab sends the user's edits to this session by itself",
	Long: "Set the push mode of this session's Proposal tab. In auto mode, af sends the user's " +
		"edits to the agent as a prompt 10 s after the last save, when the agent is idle. In " +
		"manual mode, af sends them only on \"af proposal send\" or the tab's Send now button. " +
		"Prints the push status.",
	Args:      cobra.ExactArgs(1),
	ValidArgs: []string{string(proposal.PushAuto), string(proposal.PushManual)},
	RunE: func(cmd *cobra.Command, args []string) error {
		return runProposal(func() (any, error) {
			if args[0] != string(proposal.PushAuto) && args[0] != string(proposal.PushManual) {
				return nil, fmt.Errorf("invalid push mode %q: use auto or manual", args[0])
			}
			c, err := requireProposalSession("push-mode")
			if err != nil {
				return nil, err
			}
			var resp daemon.SetProposalPushModeResponse
			err = callProposalRoute("SetProposalPushMode", daemon.SetProposalPushModeRequest{SessionID: c.sessionID, Mode: args[0]}, &resp)
			return resp, err
		})
	},
}

var proposalSendCmd = &cobra.Command{
	Use:   "send",
	Short: "Send the user's unsent edits to this session now",
	Long: "Send this session the edits to its proposal that it has not seen, as one prompt, " +
		"the same as the Proposal tab's Send now button. When the agent is busy, af sends them " +
		"when it is next idle. Prints the push status.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runProposal(func() (any, error) {
			c, err := requireProposalSession("send")
			if err != nil {
				return nil, err
			}
			var resp daemon.SendProposalChangesResponse
			err = callProposalRoute("SendProposalChanges", daemon.SendProposalChangesRequest{SessionID: c.sessionID}, &resp)
			return resp, err
		})
	},
}

func init() {
	for _, c := range []*cobra.Command{proposalPushModeCmd, proposalSendCmd} {
		c.Flags().StringVar(&proposalSessionFlag, "session", "", "Act for the session with this `title` instead of the calling session")
	}
	ProposalCmd.AddCommand(proposalPushModeCmd, proposalSendCmd)
}
