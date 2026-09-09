package daemon

import (
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// checkAdoptedRootProgramDrift compares a live adopted root with the command
// its frozen profile produces. An explicit profile program is already the final
// answer; the empty/default form needs repository and config resolution, so it
// is single-flighted off the one-second ensure sweep and cached per state.
func (m *Manager) checkAdoptedRootProgramDrift(repoID, key, workspace string, st *rootEnsureState, profile config.RootAgent, inst *session.Instance) {
	runningProgram := inst.AgentProgram()
	m.mu.Lock()
	if m.rootProgramDriftLogged == nil {
		m.rootProgramDriftLogged = make(map[string]bool)
	}
	if st.programDriftLogged || m.rootProgramDriftLogged[repoID] {
		m.mu.Unlock()
		return
	}
	if st.programDriftResolved &&
		st.programDriftResolvedWorkspace == workspace &&
		st.programDriftResolvedProfile == profile {
		configuredProgram := st.programDriftConfiguredProgram
		logDrift := configuredProgram != runningProgram
		if logDrift {
			st.programDriftLogged = true
			m.rootProgramDriftLogged[repoID] = true
		}
		m.mu.Unlock()
		if logDrift {
			m.logAdoptedRootProgramDrift(workspace, configuredProgram, runningProgram)
		}
		return
	}
	if st.programDriftResolving {
		m.mu.Unlock()
		return
	}
	st.programDriftResolving = true
	m.mu.Unlock()

	if strings.TrimSpace(profile.Program) != "" {
		m.finishAdoptedRootProgramDrift(repoID, key, workspace, st, profile, profile.Program, inst)
		return
	}
	go func() {
		configuredProgram := rootAgentProgramForProfile(workspace, profile)
		m.finishAdoptedRootProgramDrift(repoID, key, workspace, st, profile, configuredProgram, inst)
	}()
}

func (m *Manager) finishAdoptedRootProgramDrift(repoID, key, workspace string, st *rootEnsureState, profile config.RootAgent, configuredProgram string, inst *session.Instance) {
	runningProgram := inst.AgentProgram()
	status := inst.GetStatus()
	m.mu.Lock()
	st.programDriftResolving = false
	st.programDriftResolved = true
	st.programDriftResolvedWorkspace = workspace
	st.programDriftResolvedProfile = profile
	st.programDriftConfiguredProgram = configuredProgram
	logDrift := !st.programDriftLogged && !m.rootProgramDriftLogged[repoID] && m.instances[key] == inst &&
		status != session.Dead && status != session.Lost && status != session.Archived &&
		configuredProgram != runningProgram
	if logDrift {
		st.programDriftLogged = true
		m.rootProgramDriftLogged[repoID] = true
	}
	m.mu.Unlock()
	if logDrift {
		m.logAdoptedRootProgramDrift(workspace, configuredProgram, runningProgram)
	}
}

func (m *Manager) logAdoptedRootProgramDrift(workspace, configuredProgram, runningProgram string) {
	m.warn().Printf("root agent program drift for %s: configured command %q · running command %q · the live root was adopted as-is; kill the root, then restart the daemon", workspace, configuredProgram, runningProgram)
}
