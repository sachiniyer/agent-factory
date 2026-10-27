package daemon

import (
	"errors"
	"fmt"
	"strings"
)

// ErrAccountHandoffUnsupported is returned before a handoff mutation is sent
// when the responding daemon does not affirm the account-aware protocol. The
// daemon, not the client, owns admission against live account-pin state; an
// older daemon cannot safely make that decision and must not receive the call.
var ErrAccountHandoffUnsupported = errors.New("daemon does not serve the version-bound account-aware handoff endpoint (likely an older daemon — upgrade it); the handoff was not sent")

// ResumeFromLimit asks the daemon to retry a session parked at a usage-limit
// wall. The TUI and web reach the identical controlServer handler over HTTP;
// this wrapper gives the CLI its existing gob-control-socket transport without
// duplicating the recovery action.
func ResumeFromLimit(req ResumeFromLimitRequest) error {
	var resp ResumeFromLimitResponse
	if err := callDaemon("ResumeFromLimit", req, &resp); err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("resume was not performed: %s", resp.Reason)
	}
	return nil
}

// ConfirmHandoffDelivery asks the daemon to retire a pending handoff mission
// on the operator's attestation that it already landed (#4429) — the "mark
// delivered" exit for a sent-unverified or could-not-confirm verdict. A
// pre-verb daemon cannot serve the method; isRPCMethodMissing surfaces that as
// a clean refusal rather than a silent retry.
func ConfirmHandoffDelivery(req ConfirmHandoffDeliveryRequest) error {
	var resp ConfirmHandoffDeliveryResponse
	err := callDaemon("ConfirmHandoffDelivery", req, &resp)
	if isRPCMethodMissing(err) {
		return fmt.Errorf("this daemon does not support confirming a handoff delivery (upgrade the daemon), so the pending mission was left untouched")
	}
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("delivery was not confirmed: %s", resp.Reason)
	}
	return nil
}

// HandoffSession asks the daemon to continue a session under a different agent,
// in place (#2013): swap the agent program, keep the worktree and branch, and
// deliver a mission brief to the incoming agent.
func HandoffSession(req HandoffSessionRequest) (HandoffSessionResponse, error) {
	var resp HandoffSessionResponse
	err := callDaemon(AccountAwareHandoffMethod, req, &resp)
	if isRPCMethodMissing(err) {
		return HandoffSessionResponse{}, ErrAccountHandoffUnsupported
	}
	if err != nil && !isMutationCommitted(err) {
		return HandoffSessionResponse{}, err
	}
	requestedAccount := strings.TrimSpace(req.Account)
	if requestedAccount != "" && resp.ToAccount != requestedAccount {
		mismatch := &rpcMutationCommittedError{err: fmt.Errorf("daemon did not honor the requested account %q (likely an older daemon — upgrade it); the runtime was already restarted, but the resulting credential identity is unknown because the source account label may have carried across agent namespaces", requestedAccount)}
		return resp, errors.Join(err, mismatch)
	}
	return resp, err
}
