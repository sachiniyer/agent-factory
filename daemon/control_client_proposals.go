package daemon

// CallProposal calls one proposal route on the local daemon, starting it when
// needed. method is the route verb, for example "ListProposals".
func CallProposal(method string, req, resp any) error {
	if !proposalMethods[method] {
		return errUnknownProposalMethod(method)
	}
	return callDaemon(method, req, resp)
}

var proposalMethods = map[string]bool{
	"ListProposals": true, "GetProposal": true, "StartProposal": true, "AttachProposal": true,
	"DetachProposal": true, "AddProposalDoc": true, "RemoveProposalDoc": true, "ReadProposalDoc": true, "SaveProposalDoc": true,
	"WriteProposalDoc": true, "PullProposalChanges": true, "CheckpointProposal": true,
	"SetProposalPushMode": true, "HoldProposalPush": true, "SendProposalChanges": true,
}
