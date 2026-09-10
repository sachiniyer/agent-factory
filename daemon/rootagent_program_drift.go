package daemon

import (
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/programprivacy"
	"github.com/sachiniyer/agent-factory/session"
)

var resolveRootProgramConfigForInspection = config.ResolveConfigForRepoInspectionWithGlobal

// checkAdoptedRootProgramDrift compares a live adopted root with the command
// its frozen profile produces. Bare agent names and the empty/default form need
// repository config resolution, so that work is single-flighted off the
// one-second ensure sweep. The result is cached by repository, workspace,
// profile, and ApplyConfig epoch, then the complete read-only repository config
// resolution is periodically rerun because checked-in and personal files can
// change without advancing that epoch. A cached command is never compared until
// that refresh succeeds.
func (m *Manager) checkAdoptedRootProgramDrift(repo *config.RepoContext, key, workspace string, st *rootEnsureState, profile config.RootAgent, inst *session.Instance) {
	evidence := inst.ObserveRuntimeProgram()
	runningProgram := evidence.Program()
	if strings.TrimSpace(runningProgram) == "" || inst.GetInFlightOp() != session.OpNone ||
		inst.StartupStateUnknown() || inst.UserKilled() {
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
	resolutionEpoch := m.rootProgramDriftConfigEpoch
	inputsMatch := rootProgramDriftResolutionInputsMatch(st, resolutionEpoch, repoID, workspace, profile)
	if st.programDriftResolving || (inputsMatch && time.Now().Before(st.programDriftNextConfigCheck)) {
		m.mu.Unlock()
		return
	}
	if rootProgramDriftCacheMatches(st, resolutionEpoch, repoID, workspace, profile) {
		configuredProgram := st.programDriftConfiguredProgram
		if !RootAgentProfileNeedsRepoConfig(profile) {
			m.mu.Unlock()
			m.latchAdoptedRootProgramDrift(repoID, key, workspace, st, profile,
				resolutionEpoch, configuredProgram, inst, evidence)
			return
		}
		st.programDriftResolving = true
		st.programDriftResolvingEpoch = resolutionEpoch
		global := m.Config()
		m.mu.Unlock()
		go m.resolveAndFinishAdoptedRootProgram(repo, repoID, key, workspace, st, profile,
			resolutionEpoch, global, inst, evidence)
		return
	}
	st.programDriftResolving = true
	st.programDriftResolvingEpoch = resolutionEpoch
	st.programDriftResolved = false
	st.programDriftResolvedEpoch = resolutionEpoch
	st.programDriftResolvedRepoID = repoID
	st.programDriftResolvedWorkspace = workspace
	st.programDriftResolvedProfile = profile
	st.programDriftConfiguredProgram = ""
	st.programDriftNextConfigCheck = time.Time{}
	m.mu.Unlock()

	if !RootAgentProfileNeedsRepoConfig(profile) {
		m.finishAdoptedRootProgramDrift(repoID, key, workspace, st, profile, resolutionEpoch, profile.Program, nil, inst, evidence)
		return
	}
	global := m.Config()
	go m.resolveAndFinishAdoptedRootProgram(repo, repoID, key, workspace, st, profile,
		resolutionEpoch, global, inst, evidence)
}

func rootProgramDriftCacheMatches(st *rootEnsureState, epoch uint64, repoID, workspace string, profile config.RootAgent) bool {
	return st.programDriftResolved && rootProgramDriftResolutionInputsMatch(st, epoch, repoID, workspace, profile)
}

func rootProgramDriftResolutionInputsMatch(st *rootEnsureState, epoch uint64, repoID, workspace string, profile config.RootAgent) bool {
	return st.programDriftResolvedEpoch == epoch &&
		st.programDriftResolvedRepoID == repoID &&
		st.programDriftResolvedWorkspace == workspace &&
		st.programDriftResolvedProfile == profile
}

func (m *Manager) resolveAndFinishAdoptedRootProgram(
	repo *config.RepoContext,
	repoID, key, workspace string,
	st *rootEnsureState,
	profile config.RootAgent,
	resolutionEpoch uint64,
	global *config.Config,
	inst *session.Instance,
	evidence session.RuntimeProgramEvidence,
) {
	resolve := func(repo *config.RepoContext) (*config.ResolvedConfig, error) {
		return resolveRootProgramConfigForInspection(repo, global)
	}
	configuredProgram, err := rootAgentProgramForResolvedRepo(repo, profile, resolve)
	m.finishAdoptedRootProgramDrift(repoID, key, workspace, st, profile,
		resolutionEpoch, configuredProgram, err, inst, evidence)
}

func (m *Manager) finishAdoptedRootProgramDrift(repoID, key, workspace string, st *rootEnsureState, profile config.RootAgent, resolutionEpoch uint64, configuredProgram string, resolveErr error, inst *session.Instance, evidence session.RuntimeProgramEvidence) {
	m.mu.Lock()
	if resolutionEpoch != m.rootProgramDriftConfigEpoch || st.programDriftResolvingEpoch != resolutionEpoch {
		m.mu.Unlock()
		return
	}
	st.programDriftResolving = false
	if resolveErr != nil {
		// The bounded caller may outlive an uncancellable filesystem reader. Keep
		// subsequent ensure sweeps from spawning another one every second.
		st.programDriftNextConfigCheck = time.Now().Add(rootProgramDriftConfigInspectionInterval)
		m.mu.Unlock()
		return
	}
	st.programDriftResolved = true
	st.programDriftResolvedEpoch = resolutionEpoch
	st.programDriftResolvedRepoID = repoID
	st.programDriftResolvedWorkspace = workspace
	st.programDriftResolvedProfile = profile
	st.programDriftConfiguredProgram = configuredProgram
	st.programDriftNextConfigCheck = time.Time{}
	if RootAgentProfileNeedsRepoConfig(profile) {
		st.programDriftNextConfigCheck = time.Now().Add(rootProgramDriftConfigInspectionInterval)
	}
	m.mu.Unlock()
	m.latchAdoptedRootProgramDrift(repoID, key, workspace, st, profile,
		resolutionEpoch, configuredProgram, inst, evidence)
}

// latchAdoptedRootProgramDrift commits a warning only while every fact it rests
// on is still current. Manager state is checked and tentatively written under
// m.mu. Runtime evidence is checked on both sides of that write; its monotonic
// generation advances independently, so an overlapping lifecycle/runtime
// replacement forces a rollback before the tentative latch becomes visible.
func (m *Manager) latchAdoptedRootProgramDrift(repoID, key, workspace string, st *rootEnsureState, profile config.RootAgent, resolutionEpoch uint64, configuredProgram string, inst *session.Instance, evidence session.RuntimeProgramEvidence) {
	runningProgram := evidence.Program()
	status := inst.GetStatus()
	if status == session.Dead || status == session.Lost || status == session.Archived ||
		strings.TrimSpace(runningProgram) == "" || configuredProgram == runningProgram {
		return
	}

	m.mu.Lock()
	if !rootProgramDriftCacheMatches(st, resolutionEpoch, repoID, workspace, profile) ||
		st.programDriftConfiguredProgram != configuredProgram ||
		(st.programDriftLogged && st.programDriftLoggedRepoID == repoID) ||
		m.rootProgramDriftLogged[repoID] || m.instances[key] != inst ||
		!inst.RuntimeProgramEvidenceCurrent(evidence) {
		m.mu.Unlock()
		return
	}
	if st.programDriftBeforeLatchForTest != nil {
		st.programDriftBeforeLatchForTest()
	}
	st.programDriftLogged = true
	st.programDriftLoggedRepoID = repoID
	m.rootProgramDriftLogged[repoID] = true
	if !inst.RuntimeProgramEvidenceCurrent(evidence) {
		st.programDriftLogged = false
		st.programDriftLoggedRepoID = ""
		delete(m.rootProgramDriftLogged, repoID)
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	m.logAdoptedRootProgramDrift(workspace, configuredProgram, runningProgram)
}

// invalidateRootProgramDriftResolutions is the ApplyConfig rebuild hook for the
// only root-program cache that reads applied-live config. It runs on every apply,
// not only when the global program_overrides map differs. Personal project
// writes bypass ApplyConfig and are covered by the periodic complete resolution
// above. The epoch also rejects an older asynchronous resolver that finishes
// after invalidation, so it cannot repopulate the cache with a superseded global
// snapshot.
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
		st.programDriftNextConfigCheck = time.Time{}
	}
}

func (m *Manager) logAdoptedRootProgramDrift(workspace, configuredProgram, runningProgram string) {
	m.warn().Printf("root agent program drift for %s: configured command %q · running command %q · the live root was adopted as-is; restart the daemon, then kill the root", workspace, programprivacy.Redact(configuredProgram), programprivacy.Redact(runningProgram))
}
