//go:build !linux

package tmux

// probeSocketOwners is the candidacy fallback: without a kernel-socket-table
// census (the Linux sweep's mechanism), ownership stays #7689's question —
// liveTmuxServerPIDs names the candidates by task title and each is asked
// its binding. Renaming still defeats the title, so a renamed server is
// admitted only if its task name still reads like one, and a process whose
// binding cannot be read stays a possible owner — the fallback's own
// declared safe direction.
func probeSocketOwners(socketPath string) socketSweep {
	return sweepCandidatePIDs(socketPath, tmuxServerProcessPIDs())
}
