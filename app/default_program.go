package app

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
)

// programChoice is the program new sessions and tasks default to, and where
// that value comes from. It is embedded in home, so m.program reads as before.
type programChoice struct {
	// program is the last-known default. When programFollowsConfig is set it is
	// only a cache of the configured default_program, refreshed synchronously by
	// refreshDefaultProgram; otherwise it is an explicit choice (the launch
	// --program flag) that config changes must not override.
	program string
	// programFollowsConfig reports that program was derived from the
	// default_program config key rather than chosen explicitly. `af config set
	// default_program` applies live (#2480), so a value derived from config is
	// re-read whenever it is about to be used instead of trusting the boot-time
	// snapshot (#4889).
	programFollowsConfig bool
}

// newProgramChoice seeds the launch choice. An empty program means no --program
// flag was given, so the default follows config. configuredDefault is the
// launch-time RESOLVED default_program — the active project's own value when
// launched inside a repo — so a failed re-read falls back to the value launch
// actually resolved rather than the bare global default; appConfig's global
// value is only the last resort for a caller that never ran launch's
// resolution.
func newProgramChoice(program, configuredDefault string, appConfig *config.Config) programChoice {
	if program != "" {
		return programChoice{program: program}
	}
	seed := configuredDefault
	if seed == "" {
		seed = appConfig.DefaultProgram
	}
	return programChoice{program: seed, programFollowsConfig: true}
}

// defaultProgram returns the program a new session or task defaults to: the
// last-known default_program for the active project. It is a CACHE READ only —
// callers that need the live value run refreshDefaultProgram first.
func (m *home) defaultProgram() string {
	return m.program
}

// localDefaultProgramTimeout bounds the local default_program re-read. The git
// identity probe inside RepoFromPath is the part that can wedge on a stale
// mount or a broken checkout; bound it so a stuck probe fails the lookup —
// keeping the last-known cache — instead of pinning the event loop forever.
// A var so tests can shorten the bound — the timeout path it arms is the
// regression under test, and waiting ten seconds for it would out-slow the
// failure it exists to catch.
var localDefaultProgramTimeout = 10 * time.Second

// resolveConfiguredProgramContext reads default_program for the given project
// root, or from the global config alone when none is active (registry mode).
// The context bounds the git probe; the TOML reads after it run under the same
// context inside ResolveConfigForRepoContext.
//
// A package var so tests can stub the filesystem read.
var resolveConfiguredProgramContext = func(ctx context.Context, repoRoot string) (string, error) {
	if repoRoot == "" {
		cfg, err := config.LoadConfig()
		if err != nil {
			return "", err
		}
		return cfg.DefaultProgram, nil
	}
	repo, err := config.RepoFromPathContext(ctx, repoRoot)
	if err != nil {
		return "", err
	}
	resolved, err := config.ResolveConfigForRepoContext(ctx, repo)
	if err != nil {
		return "", err
	}
	return resolved.DefaultProgram, nil
}

// refreshDefaultProgram re-reads the live default_program for a LOCAL target
// and refills the cache when it succeeds (#4889). It runs synchronously on the
// event loop, matching the TUI's other synchronous config reads (the project
// switch's ResolveConfigForRepo, the preflight's resolveBackendKind, the global
// LoadConfig in newHome): the read is a bounded local TOML probe whose common
// case is microseconds, and whose worst case is capped by
// localDefaultProgramTimeout — strictly cheaper than the unbounded precedent.
//
// A read error keeps the last-known value rather than blanking the field.
//
// A REMOTE daemon keeps master's behaviour: the field shows the launch-time
// default, and no read happens at all — the remote daemon's default_program is
// remote state the client's local config files cannot see, and there is no
// synchronous remote read to make.
func (m *home) refreshDefaultProgram() {
	if !m.programFollowsConfig || isRemoteTarget() {
		return
	}
	ctx, cancel := context.WithTimeout(m.ctx, localDefaultProgramTimeout)
	defer cancel()
	program, err := resolveConfiguredProgramContext(ctx, m.repoRoot)
	if err != nil {
		log.WarningLog.Printf("default program: live default unreadable, keeping %q: %v", m.program, err)
		return
	}
	if program != "" {
		m.program = program
	}
}

// sendProgramForCreate is the Program the pending new-session form puts on the
// wire — the single derivation the submit path and preflight share (#4889
// review): "" when the form follows config untouched (the daemon resolves the
// live default_program at create), else the concrete pendingProgram. The
// concrete cases are an explicit program pick, a launch-fixed --program, and a
// CHOSEN account — an account belongs to one agent's registry, so picking one
// commits to the displayed program and pins it explicit rather than pairing
// that account with whatever default the daemon would resolve.
//
// A remote target is deliberately NOT a concrete case: "" asks the remote
// daemon to resolve ITS default_program, which is the only default a remote
// create can honor — the launch-time local seed displayed in the field is a
// guess at remote state, and sending it explicitly would override the daemon's
// live default with a program that may not even exist on that host.
func (m *home) sendProgramForCreate() string {
	if m.pendingProgramChosen || !m.programFollowsConfig {
		return m.pendingProgram
	}
	if m.pendingAccountChosen && m.pendingAccount != "" && m.pendingProgram != "" {
		return m.pendingProgram
	}
	return ""
}

// remoteDefaultMsg carries the REMOTE daemon's resolved default_program back
// to an open naming form, plus the program catalog it reported. The local seed
// the form opened with is only a guess at remote state; this reply is the
// authoritative project-scoped answer the create's implicit "" will actually
// resolve to (#4889 review).
type remoteDefaultMsg struct {
	naming   *session.Instance
	seq      int64
	program  string
	programs []string
	err      error
}

// listProgramsThroughDaemon is the fetch seam, mirroring
// listAccountsThroughDaemon: a package var so the unit suite can answer with a
// fixture instead of a daemon. It goes through the HTTP client because the
// answer must come from the daemon the create goes to — which for a remote
// target is another machine, whose default_program this host cannot read.
var listProgramsThroughDaemon = func(repoPath string) (daemon.ListProgramsResponse, error) {
	var resp daemon.ListProgramsResponse
	err := withDaemonHTTP(func(c *apiclient.Client) error {
		var e error
		resp, e = c.ListPrograms(repoPath)
		return e
	})
	if err != nil {
		return daemon.ListProgramsResponse{}, err
	}
	return resp, nil
}

// SetProgramListerForTest swaps the program-catalog seam so a test can answer
// with a fixture remote default.
func SetProgramListerForTest(f func(repoPath string) (daemon.ListProgramsResponse, error)) func() {
	prev := listProgramsThroughDaemon
	listProgramsThroughDaemon = f
	return func() { listProgramsThroughDaemon = prev }
}

// fetchRemoteDefaultProgram asks the target daemon which program an implicit
// create resolves to for the active project. Fired when the target is remote:
// refreshDefaultProgram deliberately skips remote targets (the local config
// read can't see the daemon host's config), so the displayed seed needs this
// correction. Asynchronous, like the account preselect — the form is usable
// before it lands, and a failed fetch is silent.
func (m *home) fetchRemoteDefaultProgram(naming *session.Instance) tea.Cmd {
	if naming == nil {
		return nil
	}
	m.remoteDefaultSeq++
	seq := m.remoteDefaultSeq
	repoPath := m.repoRoot
	fetch := listProgramsThroughDaemon
	return func() tea.Msg {
		resp, err := fetch(repoPath)
		programs := make([]string, 0, len(resp.Programs))
		for _, p := range resp.Programs {
			programs = append(programs, p.Name)
		}
		return remoteDefaultMsg{naming: naming, seq: seq, program: resp.Default, programs: programs, err: err}
	}
}

// handleRemoteDefault applies the daemon's resolved default to the open form.
//
// The guards mirror handleAccountDefault's: the reply must belong to the form
// still open (a submit, cancel, or new naming instance retires it), and it
// must not overwrite a program the user already picked. When it lands on a
// different program than the local guess, the account preselect fetched for
// the OLD agent no longer describes the pair the submit will form, so the
// account field re-asks for the resolved one.
func (m *home) handleRemoteDefault(msg remoteDefaultMsg) (tea.Model, tea.Cmd) {
	switch m.state {
	case stateNew, stateSelectBackend, stateSelectProgram, stateSelectAccount, statePromptInput:
		// These fields edit the same naming flow; a picker open over it must
		// not discard the correction.
	default:
		return m, nil
	}
	if m.namingInstance == nil || m.namingInstance != msg.naming {
		return m, nil
	}
	// The naming pointer alone cannot fence a stale reply: a failed remote
	// create restores the SAME session.Instance and re-fires the lookup, so
	// the earlier fetch's answer would pass the pointer guard, clear the new
	// lookup's gate, and install its stale catalog (#4893 review). The
	// issuance sequence fences it — only the latest fetch's reply counts.
	if msg.seq != m.remoteDefaultSeq {
		return m, nil
	}
	// Only a usable answer — no error, and a default to display — lifts the
	// submit gate the fetch armed. A failed or empty lookup must not free an
	// implicit submit: "" would still resolve SOMETHING daemon-side, a program
	// the field has never shown (#4893 review). The gate flips to its failed
	// form, which asks for an explicit pick rather than another wait.
	// wasVerify is captured first: only a reply to a fetch an implicit Enter
	// armed may stamp remoteDefaultVerifiedFor — the submit-time re-read, not
	// the form-open seed (#4893 review).
	wasVerify := m.remoteDefaultVerifyPending
	m.remoteDefaultVerifyPending = false
	m.remoteDefaultPending = false
	m.remoteDefaultFailed = false
	if len(msg.programs) > 0 {
		// The remote catalog is what the program picker must offer: a newer
		// daemon can support agents this client's tmux.SupportedPrograms lacks
		// (#4889 review).
		m.remoteProgramCatalog = msg.programs
	}
	if msg.err != nil || msg.program == "" {
		m.remoteDefaultFailed = true
		return m, nil
	}
	if m.state == stateSelectProgram && m.selectionOverlay != nil {
		// The open picker was built from the catalog this reply just replaced
		// — client-local when the fetch had not landed, or the daemon's
		// previous answer. Enter there would commit a program the daemon may
		// not support, and the rows no longer describe what the picker offers.
		// The account picker below dismisses for the same reason (#4893
		// review).
		m.selectionOverlay = nil
		m.programPickerChoices = nil
		m.state = stateNew
		m.menu.SetState(ui.StateNewInstance)
	}
	// A committed program is not the guess this reply may correct: an explicit
	// picker choice sets pendingProgramChosen, a launch --program pins via
	// programFollowsConfig, and a CHOSEN named account commits to the
	// displayed program through the pair — correcting any of them would put a
	// program on the wire the user never asked for, or pair the chosen account
	// with an agent whose registry it is not in (#4889/#4893 review). The
	// account half asks for a non-EMPTY name: a deliberate ambient pick sends
	// the same "" an untouched field does, so it does not pin the program
	// either — sendProgramForCreate agrees (#4893 review).
	if m.pendingProgramChosen || !m.programFollowsConfig ||
		(m.pendingAccountChosen && m.pendingAccount != "") {
		return m, nil
	}
	if m.pendingProgram == msg.program {
		if wasVerify {
			// The submit-armed answer matched the display — the field is
			// verified for this Enter onward (#4893 review).
			m.remoteDefaultVerifiedFor = msg.program
		}
		return m, nil
	}
	m.pendingProgram = msg.program
	m.namingInstance.Program = msg.program
	m.namingRev++
	if wasVerify {
		// The submit-armed answer moved the default: the correction below
		// puts the live value on the field, which is the display the next
		// Enter reviews (#4893 review).
		m.remoteDefaultVerifiedFor = msg.program
	}
	// Every account state the OLD agent produced must go: a preselect or a
	// deliberate pick belongs to that agent's registry, and keeping it would
	// submit a name the resolved agent does not have — or pin a cross-agent
	// pair the user never chose (#4889 review).
	m.clearPendingAccount()
	if m.state == stateSelectAccount && m.selectionOverlay != nil {
		// The open picker's rows were built from the old agent's registry;
		// leaving it up would let a stale row mark an account as chosen.
		m.selectionOverlay = nil
		m.accountPickerChoices = nil
		m.state = stateNew
		m.menu.SetState(ui.StateNewInstance)
	}
	return m, m.fetchAccountDefault(msg.naming, msg.program)
}
