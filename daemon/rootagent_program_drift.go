package daemon

import (
	"fmt"
	"sort"
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
// one-second ensure sweep. The resolving bit belongs to the asynchronous
// reader's actual lifetime: if an uncancellable file read stalls, no replacement
// worker can start until that reader exits. The result is cached by repository,
// workspace, registered-checkout marker, profile, and ApplyConfig epoch, then
// the complete read-only repository config resolution is periodically rerun
// because checked-in and personal files can change without advancing that
// epoch. A cached command is never compared until that refresh succeeds.
func (m *Manager) checkAdoptedRootProgramDrift(repo *config.RepoContext, key, workspace string, st *rootEnsureState, profile config.RootAgent, inst *session.Instance, identity *resolvedProjectRoot) {
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
	checkoutID := rootProgramDriftCheckoutID(identity)
	inputsMatch := rootProgramDriftResolutionInputsMatch(st, resolutionEpoch, repoID, workspace, checkoutID, profile)
	cacheMatches := rootProgramDriftCacheMatches(st, resolutionEpoch, repoID, workspace, checkoutID, profile)
	if cacheMatches && st.programDriftLatchPending && !st.programDriftResolving {
		configuredProgram := st.programDriftConfiguredProgram
		st.programDriftLatchPending = false
		m.mu.Unlock()
		m.latchOrRetryAdoptedRootProgramDrift(repoID, key, workspace, st, profile,
			resolutionEpoch, checkoutID, configuredProgram, inst, evidence)
		return
	}
	if st.programDriftResolving || (inputsMatch && time.Now().Before(st.programDriftNextConfigCheck)) {
		m.mu.Unlock()
		return
	}
	if cacheMatches {
		configuredProgram := st.programDriftConfiguredProgram
		if !rootProgramDriftNeedsInspection(profile, identity) {
			m.mu.Unlock()
			m.latchOrRetryAdoptedRootProgramDrift(repoID, key, workspace, st, profile,
				resolutionEpoch, checkoutID, configuredProgram, inst, evidence)
			return
		}
		st.programDriftResolving = true
		st.programDriftResolvingEpoch = resolutionEpoch
		st.programDriftLatchPending = false
		global := m.Config()
		m.mu.Unlock()
		m.launchAdoptedRootProgramResolution(repo, repoID, key, workspace, st, profile, identity,
			resolutionEpoch, global, inst, evidence)
		return
	}
	st.programDriftResolving = true
	st.programDriftResolvingEpoch = resolutionEpoch
	st.programDriftLatchPending = false
	st.programDriftResolved = false
	st.programDriftResolvedEpoch = resolutionEpoch
	st.programDriftResolvedRepoID = repoID
	st.programDriftResolvedWorkspace = workspace
	st.programDriftResolvedCheckoutID = checkoutID
	st.programDriftResolvedProfile = profile
	st.programDriftConfiguredProgram = ""
	st.programDriftNextConfigCheck = time.Time{}
	m.mu.Unlock()

	if !rootProgramDriftNeedsInspection(profile, identity) {
		m.finishAdoptedRootProgramDrift(repoID, key, workspace, st, profile,
			resolutionEpoch, checkoutID, profile.Program, nil, inst, evidence)
		return
	}
	global := m.Config()
	m.launchAdoptedRootProgramResolution(repo, repoID, key, workspace, st, profile, identity,
		resolutionEpoch, global, inst, evidence)
}

// launchAdoptedRootProgramResolution gives every asynchronous inspection one
// owner: the Manager. A normal read may finish after its ensure pass returns,
// while an uncancellable filesystem read may remain parked indefinitely; both
// stay joinable until their actual worker exits.
func (m *Manager) launchAdoptedRootProgramResolution(
	repo *config.RepoContext,
	repoID, key, workspace string,
	st *rootEnsureState,
	profile config.RootAgent,
	identity *resolvedProjectRoot,
	resolutionEpoch uint64,
	global *config.Config,
	inst *session.Instance,
	evidence session.RuntimeProgramEvidence,
) {
	m.mu.Lock()
	if m.rootProgramDriftInFlight == nil {
		m.rootProgramDriftInFlight = make(map[string]int)
	}
	m.rootProgramDriftInFlight[workspace]++
	m.rootProgramDriftWG.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.finishAdoptedRootProgramResolution(workspace)
		m.resolveAndFinishAdoptedRootProgram(repo, repoID, key, workspace, st, profile, identity,
			resolutionEpoch, global, inst, evidence)
	}()
}

func (m *Manager) finishAdoptedRootProgramResolution(workspace string) {
	m.mu.Lock()
	if m.rootProgramDriftInFlight[workspace] <= 1 {
		delete(m.rootProgramDriftInFlight, workspace)
	} else {
		m.rootProgramDriftInFlight[workspace]--
	}
	m.mu.Unlock()
	m.rootProgramDriftWG.Done()
}

func (m *Manager) waitRootProgramDriftInspections() {
	m.rootProgramDriftWG.Wait()
}

func (m *Manager) waitRootProgramDriftInspectionsForShutdown() {
	m.mu.Lock()
	pending := make([]string, 0, len(m.rootProgramDriftInFlight))
	count := 0
	for workspace, inFlight := range m.rootProgramDriftInFlight {
		pending = append(pending, workspace)
		count += inFlight
	}
	m.mu.Unlock()
	if count > 0 {
		sort.Strings(pending)
		m.info().Printf("waiting for %d in-flight root-agent program inspection(s) before shutting down (%s); an inspection is never abandoned — one stalled on a checkout that does not answer will hold shutdown until it does (#4087)",
			count, strings.Join(pending, ", "))
	}
	m.waitRootProgramDriftInspections()
}

func rootProgramDriftNeedsInspection(profile config.RootAgent, identity *resolvedProjectRoot) bool {
	return RootAgentProfileNeedsRepoConfig(profile) || identity != nil
}

func rootProgramDriftCheckoutID(identity *resolvedProjectRoot) string {
	if identity == nil {
		return ""
	}
	return identity.checkoutID
}

func rootProgramDriftCacheMatches(st *rootEnsureState, epoch uint64, repoID, workspace, checkoutID string, profile config.RootAgent) bool {
	return st.programDriftResolved && rootProgramDriftResolutionInputsMatch(st, epoch, repoID, workspace, checkoutID, profile)
}

func rootProgramDriftResolutionInputsMatch(st *rootEnsureState, epoch uint64, repoID, workspace, checkoutID string, profile config.RootAgent) bool {
	return st.programDriftResolvedEpoch == epoch &&
		st.programDriftResolvedRepoID == repoID &&
		st.programDriftResolvedWorkspace == workspace &&
		st.programDriftResolvedCheckoutID == checkoutID &&
		st.programDriftResolvedProfile == profile
}

func (m *Manager) resolveAndFinishAdoptedRootProgram(
	repo *config.RepoContext,
	repoID, key, workspace string,
	st *rootEnsureState,
	profile config.RootAgent,
	identity *resolvedProjectRoot,
	resolutionEpoch uint64,
	global *config.Config,
	inst *session.Instance,
	evidence session.RuntimeProgramEvidence,
) {
	resolve := func(repo *config.RepoContext) (*config.ResolvedConfig, error) {
		return resolveRootProgramConfigForInspection(repo, global)
	}
	configuredProgram, err := resolveAdoptedRootProgramForDrift(repo, profile, identity, resolve)
	if completion := config.CheckoutMarkerProbeCompletion(err); completion != nil {
		// This function already owns the per-root asynchronous worker. Keep that
		// worker (and therefore programDriftResolving) alive until the
		// uncancellable marker read actually exits; its deadline only bounded the
		// nested caller's wait, not the os.ReadFile itself.
		<-completion
	}
	m.finishAdoptedRootProgramDrift(repoID, key, workspace, st, profile,
		resolutionEpoch, rootProgramDriftCheckoutID(identity), configuredProgram, err, inst, evidence)
}

func resolveAdoptedRootProgramForDrift(repo *config.RepoContext, profile config.RootAgent, identity *resolvedProjectRoot, resolve func(*config.RepoContext) (*config.ResolvedConfig, error)) (string, error) {
	// The frozen personal root-agent profile and the command-bearing filesystem
	// layers are one diagnostic only while the registered checkout identity holds
	// across the latter read. A check on either side alone leaves a replacement
	// window in which documents from two checkouts can be combined.
	if err := verifyAdoptedRootProgramCheckout(identity); err != nil {
		return "", err
	}
	configuredProgram, err := rootAgentProgramForResolvedRepo(repo, profile, resolve)
	if err != nil {
		return "", err
	}
	if err := verifyAdoptedRootProgramCheckout(identity); err != nil {
		return "", err
	}
	return configuredProgram, nil
}

func verifyAdoptedRootProgramCheckout(identity *resolvedProjectRoot) error {
	if identity == nil {
		return nil
	}
	matches, err := config.ProjectCheckoutMatches(identity.root, identity.checkoutID)
	if err != nil {
		return fmt.Errorf("verify registered checkout for root program drift: %w", err)
	}
	if !matches {
		return fmt.Errorf("verify registered checkout for root program drift: checkout at %s no longer carries project %s marker %s", identity.root, identity.projectID, identity.checkoutID)
	}
	return nil
}

func (m *Manager) finishAdoptedRootProgramDrift(repoID, key, workspace string, st *rootEnsureState, profile config.RootAgent, resolutionEpoch uint64, checkoutID, configuredProgram string, resolveErr error, inst *session.Instance, evidence session.RuntimeProgramEvidence) {
	m.mu.Lock()
	if st.programDriftResolvingEpoch != resolutionEpoch {
		m.mu.Unlock()
		return
	}
	st.programDriftResolving = false
	st.programDriftResolvingEpoch = 0
	st.programDriftLatchPending = false
	if resolutionEpoch != m.rootProgramDriftConfigEpoch {
		m.mu.Unlock()
		return
	}
	if resolveErr != nil {
		// A reader that returned an error has exited and is safe to retry on the
		// ordinary cadence. A reader still parked in the filesystem cannot reach
		// this completion and continues to own the single-flight bit.
		st.programDriftNextConfigCheck = time.Now().Add(rootProgramDriftConfigInspectionInterval)
		m.mu.Unlock()
		return
	}
	st.programDriftResolved = true
	st.programDriftResolvedEpoch = resolutionEpoch
	st.programDriftResolvedRepoID = repoID
	st.programDriftResolvedWorkspace = workspace
	st.programDriftResolvedCheckoutID = checkoutID
	st.programDriftResolvedProfile = profile
	st.programDriftConfiguredProgram = configuredProgram
	st.programDriftNextConfigCheck = time.Time{}
	if RootAgentProfileNeedsRepoConfig(profile) || checkoutID != "" {
		st.programDriftNextConfigCheck = time.Now().Add(rootProgramDriftConfigInspectionInterval)
	}
	m.mu.Unlock()
	m.latchOrRetryAdoptedRootProgramDrift(repoID, key, workspace, st, profile,
		resolutionEpoch, checkoutID, configuredProgram, inst, evidence)
}

func (m *Manager) latchOrRetryAdoptedRootProgramDrift(repoID, key, workspace string, st *rootEnsureState, profile config.RootAgent, resolutionEpoch uint64, checkoutID, configuredProgram string, inst *session.Instance, evidence session.RuntimeProgramEvidence) {
	if !m.latchAdoptedRootProgramDrift(repoID, key, workspace, st, profile,
		resolutionEpoch, checkoutID, configuredProgram, inst, evidence) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if rootProgramDriftCacheMatches(st, resolutionEpoch, repoID, workspace, checkoutID, profile) &&
		st.programDriftConfiguredProgram == configuredProgram &&
		!(st.programDriftLogged && st.programDriftLoggedRepoID == repoID) &&
		!m.rootProgramDriftLogged[repoID] && m.instances[key] == inst {
		st.programDriftLatchPending = true
	}
}

// latchAdoptedRootProgramDrift commits a warning only while every fact it rests
// on is still current. Manager state and the config epoch are checked while
// m.mu excludes ApplyConfig's live-config/epoch commit; the instance then owns
// the final evidence-to-warning boundary. The locks follow the manager-before-
// instance order already used by daemon lifecycle bookkeeping, so config apply
// and runtime invalidation either land first and suppress the warning, or wait
// until the warning has been emitted.
func (m *Manager) latchAdoptedRootProgramDrift(repoID, key, workspace string, st *rootEnsureState, profile config.RootAgent, resolutionEpoch uint64, checkoutID, configuredProgram string, inst *session.Instance, evidence session.RuntimeProgramEvidence) bool {
	runningProgram := evidence.Program()
	status := inst.GetStatus()
	if status == session.Dead || status == session.Lost || status == session.Archived {
		return false
	}

	m.mu.Lock()
	if !rootProgramDriftCacheMatches(st, resolutionEpoch, repoID, workspace, checkoutID, profile) ||
		st.programDriftConfiguredProgram != configuredProgram ||
		(st.programDriftLogged && st.programDriftLoggedRepoID == repoID) ||
		m.rootProgramDriftLogged[repoID] || m.instances[key] != inst {
		m.mu.Unlock()
		return false
	}
	if !inst.RuntimeProgramEvidenceCurrent(evidence) {
		m.mu.Unlock()
		return true
	}
	if strings.TrimSpace(runningProgram) == "" || configuredProgram == runningProgram {
		m.mu.Unlock()
		return false
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
		return false
	}
	m.mu.Unlock()
	return true
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
		st.programDriftResolvedCheckoutID = ""
		st.programDriftResolvedProfile = config.RootAgent{}
		st.programDriftConfiguredProgram = ""
		st.programDriftNextConfigCheck = time.Time{}
		st.programDriftLatchPending = false
	}
}

func (m *Manager) logAdoptedRootProgramDrift(workspace, configuredProgram, runningProgram string) {
	m.warn().Printf("root agent program drift for %s: configured command %q · running command %q · the live root was adopted as-is; restart the daemon, then kill the root", workspace, programprivacy.Redact(configuredProgram), programprivacy.Redact(runningProgram))
}
