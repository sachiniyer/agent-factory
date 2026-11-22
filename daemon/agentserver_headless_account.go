package daemon

import "github.com/sachiniyer/agent-factory/session"

// Account-swap routes of the in-sandbox agent-server (#5571 W7.6).

// agentSwapAccountRequest carries the swap's daemon-minted identity (#7773):
// this server's terminal record binds its outcome to it, which is what lets the
// daemon reconcile a lost response against THIS swap instead of an earlier
// one's stale outcome. A pre-identity daemon sends none; the record then echoes
// an empty id, which a newer daemon reconciles as indeterminate — never a
// commit it cannot attribute. ScopedStage says which staging path the daemon
// used (#7793): this build stages and rotates account.next.<swap-id> only when
// the pair negotiated "account-swap-scoped-stage"; a pre-protocol daemon sends
// nothing and the rotation reads the shared account.next spelling exactly as
// before.
type agentSwapAccountRequest struct {
	SwapID      string `json:"swap_id,omitempty"`
	ScopedStage bool   `json:"scoped_stage,omitempty"`
}

// SwapAccount rotates the staged account scope in and relaunches the agent. The
// request names no path: the scope is derived from this server's own
// environment, and ScopedStage only selects which spelling of that derived
// staging path the rename consumes.
func (hs *headlessServer) SwapAccount(req agentSwapAccountRequest, resp *agentSwapResponse) error {
	sw, ok := hs.as.(session.AccountSwapper)
	if !ok {
		return session.ErrAccountSwapUnsupported
	}
	program, err := sw.SwapAccountInPlace(req.SwapID, req.ScopedStage)
	if err != nil {
		return err
	}
	resp.OK = true
	resp.Program = program
	return nil
}

type accountSwapStatusResponse struct {
	OK      bool   `json:"ok"`
	Outcome string `json:"outcome"`
	Program string `json:"program"`
	// SwapID echoes the id the recorded swap arrived with — empty when no swap
	// has run, or when the swap predates identity (its request carried none).
	SwapID string `json:"swap_id,omitempty"`
}

// SwapAccountStatus reports how the last account swap ended, after any running one
// has, and which swap that outcome belongs to. The daemon asks it when a swap's
// response was lost.
func (hs *headlessServer) SwapAccountStatus(_ struct{}, resp *accountSwapStatusResponse) error {
	sw, ok := hs.as.(session.AccountSwapper)
	if !ok {
		return session.ErrAccountSwapUnsupported
	}
	outcome, program, swapID, err := sw.AccountSwapStatus()
	if err != nil {
		return err
	}
	resp.OK, resp.Outcome, resp.Program, resp.SwapID = true, outcome, program, swapID
	return nil
}

// DiscardAccountNext drops the staged account scope under the same
// serialization the swap runs in, and tombstones the swap's id (#7773 R2):
// a swap request that already ran left its own terminal record; one still
// queued on this server can never commit. The daemon calls it while
// reconciling a lost response, and after any outcome it knows left no request
// in flight — a definite refusal the server answered, or a stage whose
// request was never sent. Only on a server that lacks this route, and only in
// those same no-request-in-flight cases, does the daemon fall back to an
// ordered remote rm (#7788): an unordered one could empty a scope under a
// rotation in flight.
func (hs *headlessServer) DiscardAccountNext(req agentSwapAccountRequest, resp *agentSwapResponse) error {
	sw, ok := hs.as.(session.AccountSwapper)
	if !ok {
		return session.ErrAccountSwapUnsupported
	}
	if err := sw.DiscardAccountNext(req.SwapID); err != nil {
		return err
	}
	resp.OK = true
	return nil
}
