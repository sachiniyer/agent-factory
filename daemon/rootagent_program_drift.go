package daemon

import (
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/programprivacy"
	"github.com/sachiniyer/agent-factory/session"
)

// checkAdoptedRootProgramDrift compares a live adopted root with the command
// its frozen profile produces. Bare agent names and the empty/default form need
// repository config resolution, so that work is single-flighted off the
// one-second ensure sweep and cached per repository/workspace/profile input and
// ApplyConfig epoch.
func (m *Manager) checkAdoptedRootProgramDrift(repo *config.RepoContext, key, workspace string, st *rootEnsureState, profile config.RootAgent, inst *session.Instance) {
	evidence := inst.ObserveRuntimeProgram()
	runningProgram := evidence.Program()
	if strings.TrimSpace(runningProgram) == "" || inst.GetInFlightOp() != session.OpNone {
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
		st.programDriftResolvedEpoch == m.rootProgramDriftConfigEpoch &&
		st.programDriftResolvedRepoID == repoID &&
		st.programDriftResolvedWorkspace == workspace &&
		st.programDriftResolvedProfile == profile {
		configuredProgram := st.programDriftConfiguredProgram
		logDrift := inst.RuntimeProgramEvidenceCurrent(evidence) && configuredProgram != runningProgram
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
	st.programDriftResolvingEpoch = m.rootProgramDriftConfigEpoch
	st.programDriftResolved = false
	resolutionEpoch := m.rootProgramDriftConfigEpoch
	m.mu.Unlock()

	if !RootAgentProfileNeedsRepoConfig(profile) {
		m.finishAdoptedRootProgramDrift(repoID, key, workspace, st, profile, resolutionEpoch, profile.Program, nil, inst, evidence)
		return
	}
	global := m.Config()
	go func() {
		resolve := func(repo *config.RepoContext) (*config.ResolvedConfig, error) {
			return config.ResolveConfigForRepoInspectionWithGlobal(repo, global)
		}
		configuredProgram, err := rootAgentProgramForResolvedRepo(repo, profile, resolve)
		m.finishAdoptedRootProgramDrift(repoID, key, workspace, st, profile, resolutionEpoch, configuredProgram, err, inst, evidence)
	}()
}

func (m *Manager) finishAdoptedRootProgramDrift(repoID, key, workspace string, st *rootEnsureState, profile config.RootAgent, resolutionEpoch uint64, configuredProgram string, resolveErr error, inst *session.Instance, evidence session.RuntimeProgramEvidence) {
	runningProgram := evidence.Program()
	status := inst.GetStatus()
	m.mu.Lock()
	if resolutionEpoch != m.rootProgramDriftConfigEpoch || st.programDriftResolvingEpoch != resolutionEpoch {
		m.mu.Unlock()
		return
	}
	st.programDriftResolving = false
	if resolveErr != nil {
		m.mu.Unlock()
		return
	}
	st.programDriftResolved = true
	st.programDriftResolvedEpoch = resolutionEpoch
	st.programDriftResolvedRepoID = repoID
	st.programDriftResolvedWorkspace = workspace
	st.programDriftResolvedProfile = profile
	st.programDriftConfiguredProgram = configuredProgram
	stateLogged := st.programDriftLogged && st.programDriftLoggedRepoID == repoID
	logDrift := !stateLogged && !m.rootProgramDriftLogged[repoID] && m.instances[key] == inst &&
		inst.RuntimeProgramEvidenceCurrent(evidence) &&
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

// invalidateRootProgramDriftResolutions is the ApplyConfig rebuild hook for the
// only root-program cache that reads applied-live config. It runs on every apply,
// not only when the global program_overrides map differs: a project-scoped save
// reaches the same ApplyConfig boundary while leaving the global diff unchanged.
// The epoch also rejects an older asynchronous resolver that finishes after the
// invalidation, so it cannot repopulate the cache with the superseded snapshot.
func (m *Manager) invalidateRootProgramDriftResolutions() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rootProgramDriftConfigEpoch++
	for _, st := range m.rootEnsureStates {
		st.programDriftResolving = false
		st.programDriftResolvingEpoch = 0
		st.programDriftResolved = false
		st.programDriftResolvedEpoch = 0
		st.programDriftResolvedRepoID = ""
		st.programDriftResolvedWorkspace = ""
		st.programDriftResolvedProfile = config.RootAgent{}
		st.programDriftConfiguredProgram = ""
	}
}

func (m *Manager) logAdoptedRootProgramDrift(workspace, configuredProgram, runningProgram string) {
	m.warn().Printf("root agent program drift for %s: configured command %q · running command %q · the live root was adopted as-is; kill the root, then restart the daemon", workspace, programprivacy.Redact(configuredProgram), programprivacy.Redact(runningProgram))
}
