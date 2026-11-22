package daemon

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/session"
)

// The verdict-based unfence for an in-flight off-box account swap (#7793).
//
// A daemon that dies between sending a swap request and reconciling it leaves
// the row fenced behind the durable in-flight record. This verb is the
// operator's way out short of `af sessions kill`: it re-asks the host for the
// RECORDED swap id's terminal verdict — never re-sending the request — and
// the fence lifts exactly when that produces a definite answer. Pane liveness
// alone never clears it: a reachable pane says nothing about which account
// the host is running, which is the whole reason the row was fenced.

// ReconcileOffBoxSwapRequest names the session whose in-flight swap record
// the daemon re-reconciles. ID is the session's stable id; see
// ResumeFromLimitRequest.ID — the verb settles a durable credential fence, so
// a misroute would unfence the wrong session.
type ReconcileOffBoxSwapRequest struct {
	Title  string `json:"title"`
	RepoID string `json:"repo_id"`
	ID     string `json:"id"`
}

// ReconcileOffBoxSwapResponse is the outcome: OK only when the host answered
// the recorded swap's verdict definitely — either way — and the fence lifted.
// Account is the account the settled row now records.
type ReconcileOffBoxSwapResponse struct {
	OK      bool   `json:"ok"`
	Reason  string `json:"reason,omitempty"`
	Account string `json:"account,omitempty"`
	MutationOutcome
}

func (s *controlServer) ReconcileOffBoxSwap(req ReconcileOffBoxSwapRequest, resp *ReconcileOffBoxSwapResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	resolved, err := s.manager.reconcileOffBoxSwap(req, resp)
	resp.OK = resolved
	if !resp.MutationOutcome.record(err) {
		return err
	}
	if !resp.OK && resp.Reason == "" {
		resp.Reason = "the session changed or another operation owns it"
	}
	return nil
}

// reconcileOffBoxSwap re-runs the lost-response reconcile against the
// recorded swap id (#7793). Locking mirrors the other settle verbs: per-
// (repo,title) target lock first, then the per-session op lock, with identity
// and eligibility re-verified under both. The settle the reconcile performs
// is a whole-row write: the op lock this verb holds is the ordering fence
// persistSettlementUnderAdmission names — the same shape
// prepareRuntimeReplacement relies on for a non-local backend's
// operation-fence-only callers.
func (m *Manager) reconcileOffBoxSwap(req ReconcileOffBoxSwapRequest, resp *ReconcileOffBoxSwapResponse) (bool, error) {
	instance, repoID, title, _, _, err := m.resolveActionSession(req.ID, req.Title, req.RepoID)
	if err != nil {
		return false, err
	}
	if instance == nil {
		return false, fmt.Errorf("session %q not found", title)
	}

	rec := instance.InFlightOffBoxSwap()
	if rec == nil {
		if instance.StartupStateUnknown() {
			return false, fmt.Errorf("session %q is fenced but carries no in-flight swap id a verdict could settle — `af sessions kill %s` removes the row", title, title)
		}
		return false, fmt.Errorf("session %q has no in-flight account swap to reconcile", title)
	}

	key := daemonInstanceKey(repoID, instance.Title)
	m.mu.Lock()
	if _, killing := m.killsInFlight[key]; killing {
		m.mu.Unlock()
		return false, nil
	}
	m.mu.Unlock()

	unlock := m.lockTarget(repoID, instance.Title)
	defer unlock()

	opLock := m.opLockFor(key)
	if !opLock.TryLock() {
		return false, nil
	}
	defer opLock.Unlock()

	// Re-verify under the locks: a kill or another operation may have moved
	// the session, and its record may have settled while the verb waited.
	m.mu.Lock()
	current := m.instances[key]
	_, killing := m.killsInFlight[key]
	m.mu.Unlock()
	if killing || current != instance || instance.IsTearingDown() {
		return false, nil
	}
	if instance.InFlightOffBoxSwap() == nil {
		return false, fmt.Errorf("session %q's in-flight swap already settled while the verb waited", title)
	}

	instance.SetOffBoxSwapPersist(func(i *session.Instance) error {
		return m.persistSettlementUnderAdmission(repoID, key, i)
	})
	settled, rerr := instance.ReconcileOffBoxSwapFence()
	if rerr != nil {
		if settled {
			// The verdict landed but its checkpoint did not: the row settled
			// in memory and the settlement retry owns the write — a committed
			// outcome with a warning, never a refusal (#3036).
			acct, _ := instance.AccountSelection()
			resp.Account = acct
			return true, &mutationCommittedError{err: fmt.Errorf("the swap of %q settled, but its durable checkpoint did not land: %w", title, rerr)}
		}
		return false, rerr
	}
	acct, _ := instance.AccountSelection()
	resp.Account = acct
	m.info().Printf("reconcile-swap: session %q's in-flight swap to %q (id %s) settled; the row records account %q", title, rec.To, rec.SwapID, acct)
	return true, nil
}
