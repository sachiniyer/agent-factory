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

// limitAgentFromData resolves the provider namespace for the current wall.
// New rows store it directly because account labels are agent-scoped. During a
// rolling upgrade, rows written before limit_agent can recover it from the
// durable observation that matches the current account and reset; only older
// rows without that evidence fall back to their recorded program.
func limitAgentFromData(data InstanceData, account string, observations []AccountLimitObservationData) string {
	if EffectiveLiveness(data) != LiveLimitReached {
		return ""
	}
	if tmux.IsSupportedProgram(data.LimitAgent) {
		return data.LimitAgent
	}
	matched := ""
	for _, observation := range observations {
		if !tmux.IsSupportedProgram(observation.Agent) || observation.Account != account ||
			!observation.ResetAt.Equal(data.LimitResetAt) {
			continue
		}
		if matched != "" && matched != observation.Agent {
			matched = ""
			break
		}
		matched = observation.Agent
	}
	if matched != "" {
		return matched
	}
	return tmux.DetectAgentFromCommand(data.Program)
}
