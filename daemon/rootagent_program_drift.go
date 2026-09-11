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
// one-second ensure sweep. The daemon bounds its wait but owns the synchronous
// reader's lifetime: if an uncancellable file read outlives the budget, no
// replacement worker can start until that reader exits. The result is cached by
// repository, workspace, profile, and ApplyConfig epoch, then the complete
// read-only repository config resolution is periodically rerun because
// checked-in and personal files can
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
	if st.programDriftResolving && st.programDriftResolverDone != nil {
		select {
		case <-st.programDriftResolverDone:
			st.programDriftResolving = false
			st.programDriftResolvingEpoch = 0
			st.programDriftResolverDone = nil
			st.programDriftNextConfigCheck = time.Now().Add(rootProgramDriftConfigInspectionInterval)
		default:
			m.mu.Unlock()
			return
		}
	}
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
		st.programDriftResolverDone = nil
		global := m.Config()
		m.mu.Unlock()
		go m.resolveAndFinishAdoptedRootProgram(repo, repoID, key, workspace, st, profile,
			resolutionEpoch, global, inst, evidence)
		return
	}
	st.programDriftResolving = true
	st.programDriftResolvingEpoch = resolutionEpoch
	st.programDriftResolverDone = nil
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
	type result struct {
		configuredProgram string
		err               error
	}
	resultCh := make(chan result, 1)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		resolve := func(repo *config.RepoContext) (*config.ResolvedConfig, error) {
			return resolveRootProgramConfigForInspection(repo, global)
		}
		configuredProgram, err := rootAgentProgramForResolvedRepo(repo, profile, resolve)
		resultCh <- result{configuredProgram: configuredProgram, err: err}
	}()
	timer := time.NewTimer(rootRepoProbeBudget)
	defer timer.Stop()
	select {
	case outcome := <-resultCh:
		m.finishAdoptedRootProgramDrift(repoID, key, workspace, st, profile,
			resolutionEpoch, outcome.configuredProgram, outcome.err, inst, evidence)
	case <-timer.C:
		// Prefer a completed read when the timer and result become ready together.
		select {
		case outcome := <-resultCh:
			m.finishAdoptedRootProgramDrift(repoID, key, workspace, st, profile,
				resolutionEpoch, outcome.configuredProgram, outcome.err, inst, evidence)
		default:
			m.keepAdoptedRootProgramResolutionSingleFlight(st, resolutionEpoch, workerDone)
		}
	}
}

// keepAdoptedRootProgramResolutionSingleFlight records the lifetime of a
// synchronous reader after the caller's wait budget expires. The ensure sweep
// observes this channel under m.mu and cannot start another reader until it
// closes, regardless of elapsed backoff time.
func (m *Manager) keepAdoptedRootProgramResolutionSingleFlight(st *rootEnsureState, resolutionEpoch uint64, workerDone <-chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st.programDriftResolving && st.programDriftResolvingEpoch == resolutionEpoch {
		st.programDriftResolverDone = workerDone
	}
}

func (m *Manager) finishAdoptedRootProgramDrift(repoID, key, workspace string, st *rootEnsureState, profile config.RootAgent, resolutionEpoch uint64, configuredProgram string, resolveErr error, inst *session.Instance, evidence session.RuntimeProgramEvidence) {
	m.mu.Lock()
	if st.programDriftResolvingEpoch != resolutionEpoch {
		m.mu.Unlock()
		return
	}
	st.programDriftResolving = false
	st.programDriftResolvingEpoch = 0
	st.programDriftResolverDone = nil
	if resolutionEpoch != m.rootProgramDriftConfigEpoch {
		m.mu.Unlock()
		return
	}
	if resolveErr != nil {
		// A reader that returned an error before the wait budget expired is safe to
		// retry on the ordinary cadence. Readers still alive at the deadline take
		// the retained single-flight path above and never reach this completion.
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
// on is still current. Manager state and the config epoch are checked while
// m.mu excludes ApplyConfig's live-config/epoch commit; the instance then owns
// the final evidence-to-warning boundary. The locks follow the manager-before-
// instance order already used by daemon lifecycle bookkeeping, so config apply
// and runtime invalidation either land first and suppress the warning, or wait
// until the warning has been emitted.
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
	if inst.CommitRuntimeProgramEvidence(evidence, func() {
		st.programDriftLogged = true
		st.programDriftLoggedRepoID = repoID
		m.rootProgramDriftLogged[repoID] = true
		m.logAdoptedRootProgramDrift(workspace, configuredProgram, runningProgram)
	}) {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
}

// invalidateRootProgramDriftResolutions is the ApplyConfig rebuild hook for the
// only root-program cache that reads applied-live config. It runs on every apply,
// not only when the global program_overrides map differs. Personal project
// writes bypass ApplyConfig and are covered by the periodic complete resolution
// above. The epoch also rejects an older asynchronous resolver that finishes
// after invalidation, so it cannot repopulate the cache with a superseded global
// snapshot.
func (m *Manager) applyLiveConfigAndInvalidateRootProgramDrift(newCfg *config.Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// The live snapshot and its drift-cache epoch are one publication. A warning
	// commit holding m.mu therefore observes either the complete old generation
	// or the complete new one, never new config with an old cache epoch.
	m.live.Store(newCfg)
	m.rootProgramDriftConfigEpoch++
	for _, st := range m.rootEnsureStates {
		// The epoch invalidates what an in-flight resolver may return, but it must
		// not claim that resolver exited. A timed-out os.ReadFile is uncancellable;
		// completion owns the resolving bit so ApplyConfig cannot admit a second
		// worker over the first one.
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
