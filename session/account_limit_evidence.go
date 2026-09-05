package session

import "github.com/sachiniyer/agent-factory/session/tmux"

// AccountLimitEvidenceFromData returns the canonical current-wall identity and
// retained quota observations represented by a durable row. It includes the
// roll-forward for rows written before limit_account and the per-account
// observation list existed, so raw-row readers and full Instance restoration
// cannot disagree about whether a legacy account is known limited.
func AccountLimitEvidenceFromData(data InstanceData) (string, []AccountLimitObservationData) {
	account := data.LimitAccount
	observations := append([]AccountLimitObservationData(nil), data.AccountLimitObservations...)
	if EffectiveLiveness(data) != LiveLimitReached {
		return account, observations
	}
	if account == "" && data.Account != "" && !data.AccountAutoSelected {
		// A pre-limit_account row with a named account can only be an explicit pin,
		// so that account produced the persisted current wall.
		account = data.Account
	}
	if account == "" || len(observations) > 0 {
		return account, observations
	}
	agent := tmux.DetectAgentFromCommand(data.Program)
	if agent == "" {
		return account, observations
	}
	return account, []AccountLimitObservationData{{
		Agent: agent, Account: account, ResetAt: data.LimitResetAt,
	}}
}
