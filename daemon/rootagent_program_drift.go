package daemon

import (
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// checkAdoptedRootProgramDrift compares a live adopted root with the command
// its frozen profile produces. Bare agent names and the empty/default form need
// repository config resolution, so that work is single-flighted off the
// one-second ensure sweep and cached per repository/workspace/profile input.
func (m *Manager) checkAdoptedRootProgramDrift(repo *config.RepoContext, key, workspace string, st *rootEnsureState, profile config.RootAgent, inst *session.Instance) {
	runningProgram := inst.RuntimeProgram()
	if strings.TrimSpace(runningProgram) == "" {
		return
	}
	repoID := repo.ID
	m.mu.Lock()
	if m.rootProgramDriftLogged == nil {
		m.rootProgramDriftLogged = make(map[string]bool)
	}
	if (st.programDriftLogged && st.programDriftLoggedRepoID == repoID) || m.rootProgramDriftLogged[repoID] {
		m.mu.Unlock()
		return
	}
	if st.programDriftResolved &&
		st.programDriftResolvedRepoID == repoID &&
		st.programDriftResolvedWorkspace == workspace &&
		st.programDriftResolvedProfile == profile {
		configuredProgram := st.programDriftConfiguredProgram
		logDrift := configuredProgram != runningProgram
		if logDrift {
			st.programDriftLogged = true
			st.programDriftLoggedRepoID = repoID
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
	st.programDriftResolved = false
	m.mu.Unlock()

	if !RootAgentProfileNeedsRepoConfig(profile) {
		m.finishAdoptedRootProgramDrift(repoID, key, workspace, st, profile, profile.Program, nil, inst)
		return
	}
	go func() {
		configuredProgram, err := rootAgentProgramForResolvedRepo(repo, profile, config.ResolveConfigForRepo)
		m.finishAdoptedRootProgramDrift(repoID, key, workspace, st, profile, configuredProgram, err, inst)
	}()
}

func (m *Manager) finishAdoptedRootProgramDrift(repoID, key, workspace string, st *rootEnsureState, profile config.RootAgent, configuredProgram string, resolveErr error, inst *session.Instance) {
	runningProgram := inst.RuntimeProgram()
	status := inst.GetStatus()
	m.mu.Lock()
	st.programDriftResolving = false
	if resolveErr != nil {
		m.mu.Unlock()
		return
	}
	st.programDriftResolved = true
	st.programDriftResolvedRepoID = repoID
	st.programDriftResolvedWorkspace = workspace
	st.programDriftResolvedProfile = profile
	st.programDriftConfiguredProgram = configuredProgram
	stateLogged := st.programDriftLogged && st.programDriftLoggedRepoID == repoID
	logDrift := !stateLogged && !m.rootProgramDriftLogged[repoID] && m.instances[key] == inst &&
		status != session.Dead && status != session.Lost && status != session.Archived &&
		strings.TrimSpace(runningProgram) != "" &&
		configuredProgram != runningProgram
	if logDrift {
		st.programDriftLogged = true
		st.programDriftLoggedRepoID = repoID
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
