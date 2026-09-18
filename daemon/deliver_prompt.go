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
	if err := callDaemon("DeliverPrompt", req, &resp); err != nil {
		return taskPromptDeliveryResult{}, err
	}
	if !resp.DeliveryStatus.Valid() {
		resp.DeliveryStatus = session.PromptCouldNotConfirm
	}
	return taskPromptDeliveryResult{
		status: resp.Status, deliveryStatus: resp.DeliveryStatus,
		promptRetained: resp.PromptRetained,
	}, nil
}
