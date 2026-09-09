package daemon

import (
	"fmt"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/programprivacy"
	"github.com/sachiniyer/agent-factory/session"
)

// checkAdoptedRootProgramDrift compares a live adopted root with the command
// its frozen profile produces. Bare agent names and the empty/default form need
// repository config resolution, so that work is single-flighted off the
// one-second ensure sweep. The expensive result is cached by repository,
// workspace, profile, ApplyConfig epoch, and the checked-in config content that
// contributed to it. The last dependency is periodically revalidated
// asynchronously because a branch switch has no ApplyConfig boundary, and the
// cached command is not compared until that validation succeeds.
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
	resolutionEpoch := m.rootProgramDriftConfigEpoch
	if rootProgramDriftCacheMatches(st, resolutionEpoch, repoID, workspace, profile) {
		configuredProgram := st.programDriftConfiguredProgram
		if !RootAgentProfileNeedsRepoConfig(profile) {
			m.mu.Unlock()
			m.latchAdoptedRootProgramDrift(repoID, key, workspace, st, profile,
				resolutionEpoch, configuredProgram, inst, evidence)
			return
		}
		if st.programDriftResolving || time.Now().Before(st.programDriftNextConfigCheck) {
			m.mu.Unlock()
			return
		}
		st.programDriftResolving = true
		st.programDriftResolvingEpoch = resolutionEpoch
		cachedFingerprint := st.programDriftInRepoFingerprint
		global := m.Config()
		m.mu.Unlock()
		go m.revalidateAdoptedRootProgram(repo, repoID, key, workspace, st, profile,
			resolutionEpoch, configuredProgram, cachedFingerprint, global, inst, evidence)
		return
	}
	if st.programDriftResolving {
		m.mu.Unlock()
		return
	}
	st.programDriftResolving = true
	st.programDriftResolvingEpoch = resolutionEpoch
	st.programDriftResolved = false
	m.mu.Unlock()

	if !RootAgentProfileNeedsRepoConfig(profile) {
		m.finishAdoptedRootProgramDrift(repoID, key, workspace, st, profile, resolutionEpoch, profile.Program, nil, inst, evidence)
		return
	}
	global := m.Config()
	go func() {
		configuredProgram, fingerprint, err := resolveAdoptedRootProgram(repo, profile, global)
		m.finishAdoptedRootProgramDriftWithFingerprint(repoID, key, workspace, st, profile,
			resolutionEpoch, configuredProgram, fingerprint, err, inst, evidence)
	}()
}

func rootProgramDriftCacheMatches(st *rootEnsureState, epoch uint64, repoID, workspace string, profile config.RootAgent) bool {
	return st.programDriftResolved &&
		st.programDriftResolvedEpoch == epoch &&
		st.programDriftResolvedRepoID == repoID &&
		st.programDriftResolvedWorkspace == workspace &&
		st.programDriftResolvedProfile == profile
}

func resolveAdoptedRootProgram(repo *config.RepoContext, profile config.RootAgent, global *config.Config) (string, string, error) {
	fingerprint := ""
	resolve := func(repo *config.RepoContext) (*config.ResolvedConfig, error) {
		resolved, err := config.ResolveConfigForRepoInspectionWithGlobal(repo, global)
		if err == nil {
			fingerprint = resolved.InRepoConfigFingerprint
		}
		return resolved, err
	}
	configuredProgram, err := rootAgentProgramForResolvedRepo(repo, profile, resolve)
	if err != nil {
		return "", "", err
	}
	currentFingerprint, err := config.InRepoConfigFingerprint(repo.WorkspacePath())
	if err != nil {
		return "", "", err
	}
	if currentFingerprint != fingerprint {
		return "", "", fmt.Errorf("checked-in config changed while resolving the root-agent command")
	}
	return configuredProgram, fingerprint, nil
}

func (m *Manager) revalidateAdoptedRootProgram(
	repo *config.RepoContext,
	repoID, key, workspace string,
	st *rootEnsureState,
	profile config.RootAgent,
	resolutionEpoch uint64,
	configuredProgram, cachedFingerprint string,
	global *config.Config,
	inst *session.Instance,
	evidence session.RuntimeProgramEvidence,
) {
	currentFingerprint, err := config.InRepoConfigFingerprint(repo.WorkspacePath())
	if err == nil && currentFingerprint != cachedFingerprint {
		configuredProgram, currentFingerprint, err = resolveAdoptedRootProgram(repo, profile, global)
	}
	m.finishAdoptedRootProgramDriftWithFingerprint(repoID, key, workspace, st, profile,
		resolutionEpoch, configuredProgram, currentFingerprint, err, inst, evidence)
}

func (m *Manager) finishAdoptedRootProgramDrift(repoID, key, workspace string, st *rootEnsureState, profile config.RootAgent, resolutionEpoch uint64, configuredProgram string, resolveErr error, inst *session.Instance, evidence session.RuntimeProgramEvidence) {
	m.finishAdoptedRootProgramDriftWithFingerprint(repoID, key, workspace, st, profile,
		resolutionEpoch, configuredProgram, "", resolveErr, inst, evidence)
}

func (m *Manager) finishAdoptedRootProgramDriftWithFingerprint(repoID, key, workspace string, st *rootEnsureState, profile config.RootAgent, resolutionEpoch uint64, configuredProgram, fingerprint string, resolveErr error, inst *session.Instance, evidence session.RuntimeProgramEvidence) {
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
	st.programDriftInRepoFingerprint = fingerprint
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
		st.programDriftInRepoFingerprint = ""
		st.programDriftNextConfigCheck = time.Time{}
	}
}

func (m *Manager) logAdoptedRootProgramDrift(workspace, configuredProgram, runningProgram string) {
	m.warn().Printf("root agent program drift for %s: configured command %q · running command %q · the live root was adopted as-is; kill the root, then restart the daemon", workspace, programprivacy.Redact(configuredProgram), programprivacy.Redact(runningProgram))
}
