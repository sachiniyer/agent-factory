package daemon

import "github.com/sachiniyer/agent-factory/session"

// Account-swap routes of the in-sandbox agent-server (#5571 W7.6).

// SwapAccount rotates the staged account scope in and relaunches the agent. The
// request names nothing: the scope is derived from this server's own environment.
func (hs *headlessServer) SwapAccount(_ struct{}, resp *agentSwapResponse) error {
	sw, ok := hs.as.(session.AccountSwapper)
	if !ok {
		return session.ErrAccountSwapUnsupported
	}
	program, err := sw.SwapAccountInPlace()
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
}

// SwapAccountStatus reports how the last account swap ended, after any running one
// has. The daemon asks it when a swap's response was lost.
func (hs *headlessServer) SwapAccountStatus(_ struct{}, resp *accountSwapStatusResponse) error {
	sw, ok := hs.as.(session.AccountSwapper)
	if !ok {
		return session.ErrAccountSwapUnsupported
	}
	outcome, program, err := sw.AccountSwapStatus()
	if err != nil {
		return err
	}
	resp.OK, resp.Outcome, resp.Program = true, outcome, program
	return nil
}
