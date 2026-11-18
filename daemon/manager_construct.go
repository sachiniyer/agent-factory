package daemon

import (
	"fmt"
	stdlog "log"
	"net"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/accountlogin"
	"github.com/sachiniyer/agent-factory/task"
)

// managerOptions carries construction-time seams. Everything here has a
// production default, and production passes the zero value: these exist so a
// test can scope to one Manager what the daemon keeps process-global.
type managerOptions struct {
	// warnLog, infoLog and errorLog route this Manager's diagnostics away from
	// the process-global sinks. Only tests set them; see
	// newManagerCapturingWarnings and newManagerCapturingLogs.
	warnLog  *stdlog.Logger
	infoLog  *stdlog.Logger
	errorLog *stdlog.Logger
	// listenTCP overrides the bind factory the webListeners startHTTPServer
	// builds get — the constructor seam for fixtures whose listener bind
	// happens inside startHTTPServer (#7460). Production leaves it nil.
	listenTCP func(network, address string) (net.Listener, error)
}

// NewManager constructs a manager and synchronously restores all persisted
// instances into it, returning only once the manager is ready. RunDaemon
// deliberately does NOT use this: it builds the shell with newManagerShell,
// binds the control socket, and only then runs RestoreInstances so rehydrating
// persisted state cannot delay the bind (#829).
func NewManager(cfg *config.Config) (*Manager, error) {
	return newManagerWithOptions(cfg, managerOptions{})
}

// newManagerWithOptions is NewManager with the construction seams exposed. It is
// unexported because every option is a test seam: an external caller gets the
// production defaults and nothing else.
//
// The options have to be applied by the CONSTRUCTOR rather than assigned to the
// returned Manager, because warnings fire from the root-agent snapshot inside
// this call — the fail-closed warning the singleton tests assert on is emitted
// before NewManager returns, so a logger installed afterwards would miss it.
func newManagerWithOptions(cfg *config.Config, opts managerOptions) (*Manager, error) {
	manager, err := newManagerShellWithOptions(cfg, "", opts)
	if err != nil {
		return nil, err
	}
	if err := manager.RestoreInstances(); err != nil {
		return nil, err
	}
	// NewManager is used without RunDaemon's scheduler/watcher startup. For that
	// standalone manager, restore completion is the full readiness barrier.
	if err := manager.lifecycle.markReady(); err != nil {
		return nil, err
	}
	return manager, nil
}

// newManagerShell constructs a Manager with no instances loaded. The manager
// reports !Ready() until RestoreInstances completes.
func newManagerShell(cfg *config.Config) (*Manager, error) {
	return newManagerShellForDaemon(cfg, "")
}

// newManagerShellForDaemon constructs the same manager shell with an optional
// upgrade transaction identity. The transaction-bearing path stays unexported:
// an ordinary caller can construct only a normal shell, while runDaemon owns
// the probation selection.
func newManagerShellForDaemon(cfg *config.Config, transactionID string) (*Manager, error) {
	return newManagerShellWithOptions(cfg, transactionID, managerOptions{})
}

// newManagerShellWithOptions is the one place a Manager is actually built. The
// two wrappers above pass the zero options, which is production.
func newManagerShellWithOptions(cfg *config.Config, transactionID string, opts managerOptions) (*Manager, error) {
	lifecycle, err := newDaemonLifecycle(transactionID, cfg.ListenAddr, cfg.PreviewListenAddr)
	if err != nil {
		return nil, err
	}
	// Mint the ephemeral preview HMAC secret (#1856 step 3). In memory only, so it
	// never touches disk and rotates on restart; generateToken is the same 256-bit
	// crypto/rand source the daemon bearer uses. Per-tab tokens are derived from it
	// (previewTabToken); the secret itself is never vended or logged.
	previewSecret, err := generateToken()
	if err != nil {
		return nil, fmt.Errorf("mint preview secret: %w", err)
	}
	// The editor-origin secret is PERSISTED, unlike the one above (#2743): a session's
	// VS Code origin must survive a daemon restart or the browser hands the editor a
	// fresh, empty IndexedDB every time. Non-fatal — a daemon that cannot keep this
	// secret should still start and simply serve editors on the app origin as before,
	// rather than refusing to run over a preview feature that is off by default.
	editorSecret, err := ensureEditorOriginSecret()
	if err != nil {
		log.WarningLog.Printf("editor preview origins disabled: %v", err)
		editorSecret = ""
	}
	claimLaunchCapabilityOwnership() // before any restore: restoring a session re-establishes its default-command
	state := config.LoadState()
	storage, err := session.NewStorage(state, "")
	if err != nil {
		return nil, fmt.Errorf("failed to initialize storage: %w", err)
	}
	vscode := newVSCodeSupervisor()
	configAgents := newConfigAgentSupervisor()
	accountLogins := accountlogin.New()
	rootAgentLayers := buildRootAgentSnapshot(warnLoggerOr(opts.warnLog), errorLoggerOr(opts.errorLog), cfg)
	mgr := &Manager{
		cfg:                       cfg,
		warnLog:                   opts.warnLog,
		infoLog:                   opts.infoLog,
		errorLog:                  opts.errorLog,
		listenTCP:                 opts.listenTCP,
		previewSecret:             previewSecret,
		previewOrigins:            newPreviewOriginRegistry(),
		editorOriginSecret:        editorSecret,
		editorLabels:              newEditorLabelIndex(),
		editorPorts:               newEditorPortOrigins(),
		pollReloadCh:              make(chan struct{}, 1),
		ready:                     make(chan struct{}),
		lifecycle:                 lifecycle,
		storage:                   storage,
		instances:                 make(map[string]*session.Instance),
		pendingCreates:            make(map[string]session.InstanceData),
		reservedTitles:            make(map[string]struct{}),
		projectDeletes:            make(map[string]struct{}),
		projectDeleteLastSeq:      make(map[string]uint64),
		reservedTmuxNames:         make(map[string]string),
		reservedRemoteNames:       make(map[string]struct{}),
		reservedTaskRuns:          make(map[string]int),
		ghostTaskRuns:             make(map[string]int),
		repoStartLocks:            make(map[string]*sync.Mutex),
		worktreeAdmissionLocks:    make(map[string]*sync.Mutex),
		aliveObservations:         make(map[string]uint64),
		targetLocks:               make(map[string]*sync.Mutex),
		rootEnsureStates:          make(map[string]*rootEnsureState),
		rootProgramDriftLogged:    make(map[string]bool),
		rootProgramDriftInFlight:  make(map[string]int),
		rootCreateRefusals:        make(map[string]rootCreateRefusal),
		rootCreatesInFlight:       make(map[string]*rootCreateTicket),
		rootKilledAt:              make(map[string]time.Time),
		deletedRootRepos:          make(map[string]string),
		killsInFlight:             make(map[string]struct{}),
		killRetries:               make(map[string]*session.CleanupRetry),
		ghostCleanupStalls:        make(map[string]string),
		backgroundMutationStop:    make(chan struct{}),
		restoresInFlight:          make(map[string]struct{}),
		lostRestoreStates:         make(map[string]*lostRestoreState),
		lostRestoreConfirmations:  make(map[string][]time.Time),
		limitResumeStates:         make(map[string]*limitResumeState),
		handoffRetryDue:           make(map[string]time.Time),
		handoffReportCh:           make(chan handoffReportJob, 64),
		settleOwed:                make(map[string]settleOwedEntry),
		dischargeOwed:             make(map[string]dischargeRetryEntry),
		remoteLossStates:          make(map[string]*remoteLossState),
		taskRunTurnWatches:        make(map[string]*taskRunTurnWatchEntry),
		unverifiedSendWatches:     make(map[string]map[string]*unverifiedSendWatch),
		pendingSwapNoticeWatches:  make(map[string]*pendingSwapNoticeWatch),
		stagedDraftWatches:        make(map[string]*stagedDraftWatch),
		instanceOpLocks:           make(map[string]*sync.Mutex),
		pausedPolls:               make(map[string]map[string]time.Time),
		taskRunProbeDue:           make(map[string]time.Time),
		rootHealAbsenceStreaks:    make(map[string]int),
		rootHealAbsenceLastStrike: make(map[string]time.Time),
		rootHealProbes:            make(map[string]*rootReattributionProbe),
		rootHealProbeFailures:     make(map[string]int),
		events:                    newEventsHub(),
		vscode:                    vscode,
		configAgents:              configAgents,
		accountLogins:             accountLogins,
	}
	// Publish the start-of-day root-agent snapshot; healRootAgentLayers is the
	// only later writer, and it republishes wholesale (#3264).
	mgr.rootAgentLayers.Store(&rootAgentLayers)
	// Seed the hot-reloadable live config with the startup config (#2480). Config()
	// reads it; ApplyConfig swaps it in place.
	mgr.live.Store(&livePosturePublication{cfg: cfg})
	// Build the usage-limit detector from the startup config; ApplyConfig rebuilds
	// it in place when limit_patterns changes.
	initialDetector := task.NewLimitDetector(cfg.LimitPatterns)
	mgr.limitDetector.Store(&initialDetector)
	// Read the editor override off the LIVE config, not a captured one: an
	// ApplyConfig swap of vscode_server_binary then reaches new spawns in place
	// (#2480), and the closure still never re-reads disk per spawn.
	vscode.configuredBinary = func() string {
		if c := mgr.Config(); c != nil {
			return c.VSCodeServerBinary
		}
		return ""
	}
	mgr.configAssistants = newConfigAssistantHub(mgr)
	return mgr, nil
}
