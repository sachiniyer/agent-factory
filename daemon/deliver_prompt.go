package daemon

import (
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

type taskPromptDeliveryResult struct {
	status         string
	deliveryStatus session.PromptDeliveryStatus
	promptRetained bool
	// sessionID/sessionCreatedAt identify the session this delivery CREATED
	// (empty when it sent into an existing target), so deliverTaskPromptRun can
	// key its run-status write to the run (#5661). lastPromptDeliveryStatus is
	// the created session's own verdict on its initial prompt (#5278):
	// PromptNotDelivered means the create started a session whose prompt
	// provably never landed, which the caller records as a failed run rather
	// than "started". Zero unless a session was created.
	sessionID                string
	sessionCreatedAt         time.Time
	lastPromptDeliveryStatus session.PromptDeliveryStatus
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
		status:                   resp.Status,
		deliveryStatus:           resp.DeliveryStatus,
		promptRetained:           resp.PromptRetained,
		sessionID:                resp.SessionID,
		sessionCreatedAt:         resp.SessionCreatedAt,
		lastPromptDeliveryStatus: resp.LastPromptDeliveryStatus,
	}, err
}
