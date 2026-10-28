package daemon

import "github.com/sachiniyer/agent-factory/session"

type taskPromptDeliveryResult struct {
	status         string
	deliveryStatus session.PromptDeliveryStatus
	promptRetained bool
}

// deliverPromptForTaskRPC preserves the daemon's structural distinction between
// an existing limited target and a newly created parked target that already
// retained this prompt. Public send-prompt callers need only status + evidence;
// the watch path needs the retained bit to avoid queueing a duplicate.
func deliverPromptForTaskRPC(req DeliverPromptRequest) (taskPromptDeliveryResult, error) {
	var resp DeliverPromptResponse
	err := callDaemon("DeliverPrompt", req, &resp)
	// callDaemon classifies the committed outcome generically off the
	// embedded MutationOutcome; keep the result on that path — a retained
	// committed auto-create (#3357) durably recorded its session/workspace, so
	// the committed error must survive to deliverTaskPromptOutcome (and any
	// public caller) rather than be swallowed as nil. Only a clean failure has
	// no result to report. Mirrors CreateSession/KillSession's committed
	// preservation.
	if err != nil && !isMutationCommitted(err) {
		return taskPromptDeliveryResult{}, err
	}
	if !resp.DeliveryStatus.Valid() {
		resp.DeliveryStatus = session.PromptCouldNotConfirm
	}
	return taskPromptDeliveryResult{
		status: resp.Status, deliveryStatus: resp.DeliveryStatus,
		promptRetained: resp.PromptRetained,
	}, err
}
