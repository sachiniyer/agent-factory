package app

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/sachiniyer/agent-factory/internal/namegen"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/overlay"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
)

// handleStateNew handles key events when in stateNew (naming a new instance).
func (m *home) handleStateNew(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		// Kill by the captured namingInstance pointer, not the live selection:
		// background sync may have drifted the selection off the naming row, in
		// which case selection-based Kill() silently no-ops and leaves a
		// "Loading" zombie behind (#717). Kill before clearing the pointer.
		if err := m.store.KillInstance(m.namingInstance); err != nil {
			log.ErrorLog.Printf("failed to clean up instance on cancel: %v", err)
		}
		m.state = stateDefault
		m.namingInstance = nil
		m.clearNamingPlaceholder()
		m.pendingPrompt = ""
		m.pendingBackend, m.pendingHost = "", ""
		m.backendPickerPending = false
		m.pendingAccount = ""
		m.pendingProgramChosen = false
		m.remoteDefaultPending = false
		m.remoteProgramCatalog = nil
		// Menu.SetState rebuilds the options slice; call it synchronously
		// on the event-loop goroutine rather than from a tea.Cmd closure
		// that runs off-loop and races with home.View -> Menu.String.
		m.menu.SetState(ui.StateDefault)
		return m, tea.Batch(m.selectionChanged(), tea.WindowSize())
	}

	instance := m.namingInstance
	if instance == nil {
		return m, nil
	}
	if m.backendPickerPending {
		// Keep the requested picker reachable while its catalog is in flight.
		// Switching fields would make the catalog's state guard discard it.
		switch msg.Type {
		case tea.KeyEnter, tea.KeyTab, tea.KeyShiftTab, tea.KeyCtrlR, tea.KeyCtrlO:
			return m, m.handleNotice(errors.New("Loading backends…"))
		}
	}
	// The line-clear key empties the title, as it does in every other TUI text
	// field (#4846).
	if ui.IsClearLineKey(msg) {
		if err := instance.SetTitle(""); err != nil {
			return m, m.handleError(err)
		}
		return m, nil
	}
	switch msg.Type {
	case tea.KeyEnter:
		// Resolve the effective title into a LOCAL and run every naming gate against
		// it, committing it to the instance only AFTER they all pass (#2470 review).
		//
		// An EXACT-empty field (nothing typed) adopts the shadow placeholder shown in
		// its row — the "autocreate". The match with the renderer is deliberate: the
		// row paints the placeholder only when the title is exactly empty
		// (ui/tree/render.go), so adopting on exact-empty too means enter creates
		// precisely the name the user could see. A whitespace-only field is NOT that —
		// its shadow text is already gone — so it stays the #973 error, not a silent
		// adopt of an off-screen name. TrimSpace mirrors the daemon's
		// validateTitleAvailableLocked.
		//
		// Writing the title before the gates (the first cut of this feature) left a
		// FAILED gate — a reserved/colliding name, or a missing agent binary at
		// preflight — with the suggestion permanently stamped as a real, full-contrast
		// title the user never chose and had to backspace out by hand. Keeping it a
		// local until every gate passes leaves the row showing the shadow placeholder,
		// retryable, on any failure.
		title := instance.Title
		if title == "" {
			title = m.namingPlaceholder
		}
		if strings.TrimSpace(title) == "" {
			return m, m.handleNotice(fmt.Errorf("title cannot be empty"))
		}
		// The input paths all clamp to the cap, so an overlong title here means
		// the field was seeded some other way (a restored failed-create draft).
		// Refusing keeps the field editable and creates nothing — the same
		// contract the naming-row input keeps (#5481).
		if runewidth.StringWidth(title) > 32 {
			return m, m.handleNotice(fmt.Errorf("title cannot be longer than 32 characters"))
		}
		// "root" is reserved for the daemon-managed root agent (#1106). The
		// daemon's reserveCreate is the authoritative gate; rejecting here
		// keeps the user in the naming overlay instead of surfacing the
		// error after submit, mirroring the #936 collision pre-check below.
		//
		// Ask the ADMISSION question, and ask it by RAISING the daemon's own
		// refusal rather than wording a second one (#3756). What is reserved is
		// the derived tmux name, not the spelling (#3732), so "ro ot" belongs
		// here too — and a hand-copied message is exactly what let this check
		// and the daemon drift into refusing different sets in the first place.
		if err := session.ReservedTitleRefusalFor(title, instance.Path); err != nil {
			return m, m.handleNotice(err)
		}
		for _, other := range m.store.GetInstances() {
			if other == instance {
				continue
			}
			// Mirror the daemon's authoritative collision rule (git.TitlesCollide:
			// case-insensitive equality OR same sanitized branch) so the naming
			// flow rejects what the daemon would reject after submit, instead of
			// only catching exact duplicates and deferring case/branch variants
			// to a post-Start error (#936).
			if git.TitlesCollide(other.Title, title, m.appConfig.BranchPrefix) {
				return m, m.handleNotice(fmt.Errorf("a session titled %q conflicts with existing session %q", title, other.Title))
			}
		}
		// Which runtime this create lands on, resolved from the PENDING selection
		// rather than read off the placeholder's capabilities (#2599): the
		// placeholder is pinned local and provisions nothing, so it no longer knows.
		// This is the same decision the daemon will make, by the same precedence,
		// and it creates nothing — BackendKindFor exists for exactly this.
		//
		// A kind that does not resolve is left to the daemon. The backend field
		// offers whatever the daemon's catalog lists (#2600), so a name this
		// process's enum has never heard of is normal; refusing it here, or guessing
		// that it is remote, would both be this side inventing an answer it does not
		// have.
		backendKind, backendKindErr := session.BackendKindFor(session.InstanceOptions{
			Backend: session.BackendKind(m.pendingBackend),
			Host:    m.pendingHost,
		}, instance.Path)
		if backendKindErr == nil && backendKind == session.BackendHook {
			if !session.RemoteHookTitleHasSpecificSlug(title) {
				return m, m.handleNotice(fmt.Errorf(
					"remote hook session title %q must retain at least one ASCII letter or digit after hook-name sanitization",
					title,
				))
			}
			existing := make([]*session.Instance, 0, m.store.NumInstances())
			for _, other := range m.store.GetInstances() {
				// Only hook sessions own this global slug namespace. Docker and
				// SSH are remote workspaces too, but their same-looking slugs are
				// repo-scoped paths or labels and cannot collide with --name.
				if other == instance || !other.ToInstanceData().IsRemoteHook() {
					continue
				}
				existing = append(existing, other)
			}
			if dup := session.FindSlugCollision(title, existing); dup != "" {
				return m, m.handleNotice(fmt.Errorf(
					"a remote session titled %q already maps to hook name %q",
					dup, session.Slugify(title),
				))
			}
		}
		// preflightSessionCreate reads only the backend/program, not the title, so it
		// runs before the title is committed.
		if err := m.preflightSessionCreate(); err != nil {
			return m, m.handleError(err)
		}
		// Every gate passed — commit the resolved title exactly once.
		//
		// SetTitle stays on handleError. It performs no input validation: its one
		// error is "cannot change title of a started instance", i.e. the naming
		// placeholder was somehow already started. That is a broken invariant, not
		// user feedback, and it must keep reaching ERROR monitoring. The branches
		// above are the validation ones, and only those are notices.
		if err := instance.SetTitle(title); err != nil {
			return m, m.handleError(err)
		}

		// Apply the program selected during naming. The optimistic create op
		// (OpCreating) was already raised in startNewInstance when the naming flow
		// began — re-raising it here would be a second BeginCreate from OpCreating,
		// an illegal edge the chokepoint rejects (#1350). Set it exactly once.
		instance.Program = m.pendingProgram
		// The WIRE value is different from the shown one (#4889 review): a
		// program the field only DISPLAYS — the config-derived seed the
		// synchronous refresh left — is submitted as "" so the daemon resolves
		// default_program at create time, on the daemon's host, against the
		// session's own repo. Committing the displayed string would freeze a
		// stale cache into an explicit request.
		//
		// Only a choice the user actually made — a picker pick, a launch
		// --program flag, or a restored failed-create draft — goes out as a
		// concrete program. A remote target stays implicit the same way — ""
		// asks the remote daemon to resolve ITS default_program, the only
		// default a remote create can honor (#4889 review).
		// A remote form cannot submit "" while its ListPrograms answer is still
		// in flight: the daemon would resolve a default the field has not yet
		// shown (#4889 review).
		if m.remoteDefaultPending {
			return m, m.handleNotice(errors.New("still resolving the remote daemon's default program — try again in a moment"))
		}
		sendProgram := m.sendProgramForCreate()
		// The account belongs to one agent's registry, so it must travel with
		// the program it was chosen or preselected for (#4889 review). When the
		// program goes out implicitly ("" resolves the live default on the
		// daemon) a stale pairing is possible: an account the daemon PRESELECTED
		// for the displayed program would be read as explicit for whatever
		// program the daemon resolves now — refuse-worthy or silently wrong. A
		// CHOSEN account is a commitment to the displayed program, so
		// sendProgramForCreate already pins that program explicit; an unchosen
		// preselect stays implicit alongside the program and lets the daemon
		// resolve both.
		sendAccount := m.pendingAccount
		if sendProgram == "" {
			sendAccount = ""
		}
		// The failed-create draft needs the user's CHOICES beside the wire
		// values the rules above produced: an account-pinned program and a
		// deliberate ambient pick are indistinguishable from their wire
		// encodings once the pending flags clear (#4889 review).
		//
		// A launch --program counts as chosen too: programFollowsConfig=false
		// sends it concretely even though only the picker sets the pending
		// flag, and a later project switch resets the config-following bit —
		// restoring such a draft as implicit would swap the launch program
		// for whatever the moved default is (#4889 review).
		programChosen := m.pendingProgramChosen || !m.programFollowsConfig
		accountChosen := m.pendingAccountChosen
		// Read the pending prompt here, on the event loop, and clear it with the
		// rest of the naming state: the cmd below runs off-loop, so reading
		// m.pendingPrompt from inside the closure would race the next create's
		// reset. TrimSpace so a field holding only whitespace is "no prompt"
		// rather than a stray newline delivered to the agent.
		rawPrompt := m.pendingPrompt
		prompt := strings.TrimSpace(rawPrompt)
		m.pendingPrompt = ""
		// Same reasoning for the backend picked in the ctrl+r field (#1933): read it
		// on the loop, clear it with the rest of the naming state.
		backend, host := m.pendingBackend, m.pendingHost
		m.pendingBackend, m.pendingHost = "", ""
		m.backendPickerPending = false
		// And for the ctrl+o account field (#3844), on the loop for the same
		// reason — sendAccount already captured its value above.
		m.pendingAccount = ""
		m.pendingAccountChosen = false
		m.pendingProgramChosen = false
		m.remoteDefaultPending = false
		m.remoteProgramCatalog = nil
		m.namingInstance = nil
		m.clearNamingPlaceholder()
		m.state = stateDefault
		m.menu.SetState(ui.StateDefault)

		// Capture the start seam on the event loop, before the goroutine: it is a
		// package var swapped by test seams, so reading it inside the cmd goroutine
		// would race a sibling parallel test's swap (the #960 PR 4 snapshot-race
		// class). Reading it here pins the value for this cmd.
		start := startSessionThroughDaemon
		startCmd := func() tea.Msg {
			req := sessionStartRequest{
				Title:    instance.Title,
				RepoPath: instance.Path,
				Program:  sendProgram,
				// The initial prompt typed into the naming form's shift+tab
				// field (#1936). session_control.go forwards it to the daemon,
				// which delivers it once the agent is ready — the same path
				// `af sessions create --prompt` takes. Empty means "no prompt",
				// exactly as before this field existed.
				Prompt: prompt,
				// The backend picked in the naming form's ctrl+r field (#1933),
				// forwarded verbatim as CreateSessionRequest.Backend — the field
				// `af sessions create --backend` fills. Empty means "resolve from
				// the repo's config", so an untouched field is byte-identical to
				// every create before this field existed.
				Backend: backend,
				Host:    host,
				// The credential account picked in the naming form's ctrl+o field
				// (#3844), forwarded verbatim as CreateSessionRequest.Account — the
				// field `af sessions create --account` fills. Empty means "the ambient
				// identity", so an untouched field is byte-identical to every create
				// before this field existed.
				Account:       sendAccount,
				ProgramChosen: programChosen,
				AccountChosen: accountChosen,
			}
			started, err := start(instance, req)
			return instanceStartedMsg{
				draft: &req, rawPrompt: rawPrompt,
				instance: instance,
				started:  started,
				err:      err,
				// Carried so the completion handler can compare it with the account
				// the daemon reports on the session it created (#3844 constraint 5).
				// Read off the request rather than the model: by the time this lands
				// the form has been reset, and a later create may hold a different one.
				account: sendAccount,
			}
		}

		return m, tea.Batch(tea.WindowSize(), m.selectionChanged(), startCmd)
	case tea.KeyTab:
		// Open program selection overlay. A remote target's catalog is the
		// DAEMON's — it can list agents this client's tmux.SupportedPrograms
		// lacks, and a resolved default absent from it must still render as the
		// selected row rather than collapsing the pick onto index 0 (#4889
		// review).
		catalog := tmux.SupportedPrograms
		if isRemoteTarget() && len(m.remoteProgramCatalog) > 0 {
			catalog = m.remoteProgramCatalog
		}
		items := make([]string, 0, len(catalog)+1)
		items = append(items, catalog...)
		if m.pendingProgram != "" && !slices.Contains(items, m.pendingProgram) {
			items = append(items, m.pendingProgram)
		}
		m.programPickerChoices = items
		selectedIdx := 0
		for i, p := range items {
			if m.pendingProgram == p {
				selectedIdx = i
			}
		}
		m.selectionOverlay = overlay.NewSelectionOverlay("Select program", items)
		m.selectionOverlay.SetWidth(40)
		m.selectionOverlay.SetSelectedIndex(selectedIdx)
		m.layoutSelectionOverlay()
		m.state = stateSelectProgram
		return m, nil
	case tea.KeyShiftTab:
		// Open the initial-prompt field, seeded with whatever is already
		// pending so reopening it is an edit, not a retype (#1936).
		m.promptOverlay = overlay.NewPromptOverlay("Initial prompt", m.pendingPrompt)
		m.layoutPromptOverlay()
		m.state = statePromptInput
		return m, nil
	case tea.KeyCtrlR:
		// Open the backend field (#1933). Unlike the program and prompt fields this
		// one needs the daemon's answer before it can render an honest list, so the
		// keypress starts a fetch and the overlay opens when it lands — see
		// openBackendPicker.
		return m.openBackendPicker()
	case tea.KeyCtrlO:
		// Open the account field (#3844). Same async shape as the backend field: the
		// registry lives in the DAEMON's home, so the keypress starts a fetch and the
		// overlay opens when it lands — see openAccountPicker.
		return m.openAccountPicker()
	case tea.KeyRunes:
		// Bracketed paste arrives as ONE KeyRunes message and may contain
		// newlines or other control runes. Titles are sidebar rows, so keep only
		// printable content from the whole payload before measuring it (#2640).
		cleanRunes := make([]rune, 0, len(msg.Runes))
		for _, r := range msg.Runes {
			if !unicode.IsControl(r) {
				cleanRunes = append(cleanRunes, r)
			}
		}
		return m.appendToNamingTitle(string(cleanRunes))
	case tea.KeyBackspace:
		runes := []rune(instance.Title)
		if len(runes) == 0 {
			return m, nil
		}
		if err := instance.SetTitle(string(runes[:len(runes)-1])); err != nil {
			return m, m.handleError(err)
		}
	case tea.KeySpace:
		return m.appendToNamingTitle(" ")
	case tea.KeyEsc:
		// Kill by the captured namingInstance pointer, not the live selection
		// (#717) — see the ctrl+c branch above for the full rationale.
		if err := m.store.KillInstance(m.namingInstance); err != nil {
			log.ErrorLog.Printf("failed to clean up instance on cancel: %v", err)
		}
		m.namingInstance = nil
		m.clearNamingPlaceholder()
		m.pendingPrompt = ""
		m.pendingBackend, m.pendingHost = "", ""
		m.backendPickerPending = false
		m.pendingAccount = ""
		m.pendingProgramChosen = false
		m.remoteDefaultPending = false
		m.remoteProgramCatalog = nil
		m.state = stateDefault
		cmd := m.selectionChanged()

		// Menu.SetState rebuilds the options slice; call it synchronously
		// on the event-loop goroutine rather than from a tea.Cmd closure
		// that runs off-loop and races with home.View -> Menu.String.
		m.menu.SetState(ui.StateDefault)
		return m, tea.Batch(cmd, tea.WindowSize())
	default:
	}
	return m, nil
}

// appendToNamingTitle appends suffix to the naming field's title, clamping the
// result to the 32-cell cap. Overflow input is TRUNCATED with a notice saying
// so rather than refused (#5481): a paste can arrive split across several
// KeyRunes messages, so refusing each overflowing message still committed the
// earlier chunks — the field ended holding a silent prefix of what was typed.
// Clamping instead makes the message match the outcome every time: the field
// shows exactly the title the session will be created with, and the notice
// reports that the rest was dropped. Typed input at the cap behaves the same —
// the extra keystroke is dropped and the notice explains why.
//
// The length cap and its notice are user feedback; the SetTitle error is not
// (see the commit site in handleStateNew) — it keeps ERROR severity here too.
func (m *home) appendToNamingTitle(suffix string) (tea.Model, tea.Cmd) {
	instance := m.namingInstance
	newTitle := instance.Title + suffix
	truncated := runewidth.StringWidth(newTitle) > 32
	if truncated {
		newTitle = runewidth.Truncate(newTitle, 32, "")
	}
	if err := instance.SetTitle(newTitle); err != nil {
		return m, m.handleError(err)
	}
	if truncated {
		return m, m.handleNotice(fmt.Errorf("title is limited to 32 characters; extra input was dropped"))
	}
	return m, nil
}

// startNewInstance creates a new instance and enters stateNew for naming.
func (m *home) startNewInstance() (tea.Model, tea.Cmd) {
	m.backendPickerPending = false
	if restored, restoreCmd := m.restoreFailedCreate(); restored {
		return m, tea.Batch(m.selectionChanged(), restoreCmd)
	}
	// A session lives in a project, and registry mode has none until the user
	// selects one (#2477) — so refuse here rather than opening a form that cannot
	// be submitted (#2764).
	//
	// The old fallback to the process cwd was written when m.repoRoot was always
	// the launch repo, and #2477 made it reachable with no repo behind it: the
	// naming form opened, the user typed a name, and the create failed only on
	// submit, daemon-side, with `failed to get git repo root for <cwd>: exit
	// status 128`. Nothing in that sentence tells them a project has to be picked
	// first, and by then they had already done the work of naming the session.
	//
	if m.repoRoot == "" {
		return m, m.handleNotice(errors.New(noActiveProjectNotice(m.enterPicksAProject(), m.projectsFocused())))
	}
	// Re-read the live default_program synchronously before seeding the field:
	// `af config set default_program` applies live (#2480), so the launch-time
	// cache alone can show a stale agent (#4889). The read is a bounded local
	// probe — the same class of synchronous config read the project switch and
	// preflight already run on this goroutine — and a remote target keeps the
	// launch-time value untouched.
	m.refreshDefaultProgram()
	m.pendingProgram = m.defaultProgram()
	// Every create starts with an empty prompt field and an unchosen backend. The
	// cancel paths clear both too, but this is the authoritative reset: it also
	// covers a create that ended by any route other than Enter/Esc/ctrl+c.
	m.pendingPrompt = ""
	m.pendingBackend, m.pendingHost = "", ""
	m.pendingProgramChosen = false
	m.remoteDefaultPending = false
	m.remoteProgramCatalog = nil
	m.clearPendingAccount()
	if m.pendingProgram == "" && m.appConfig != nil {
		m.pendingProgram = m.appConfig.DefaultProgram
	}
	// Target the ACTIVE project's repo root, not the process cwd: after an
	// in-place project switch (#1461) the active repo is m.repoRoot, which may no
	// longer be where af was launched. At launch m.repoRoot is the cwd's repo, so
	// this is equivalent for the unswitched case. The guard above guarantees it is
	// set.
	repoPath := m.repoRoot
	// The naming row needs a ROW, not a runtime (#2599).
	//
	// This instance exists so the rail has something to type a title into. It is
	// thrown away on submit — startSessionThroughDaemon ignores it entirely and
	// rebuilds the session from what the daemon returns — and killed on cancel. Yet
	// NewInstance resolves the create's backend and PROVISIONS it, and for a
	// non-local kind that is real work: dockerRuntime.Provision runs `docker run` +
	// clone + `af agent-server`, hookRuntime.Provision runs the repo's launch_cmd.
	// So in a repo whose config says backend = "docker"/"ssh"/"hook", pressing `n`
	// was refused before the form ever opened, by an error thrown from inside a
	// provisioner — the TUI could not create a session in that repo at all. It also
	// contradicts #960: the daemon is the sole provisioner.
	//
	// Pinning the placeholder to the local runtime is what makes it inert.
	// localRuntime.Provision is a pure constructor (ProvisionResult{Backend:
	// &LocalBackend{}}) — no worktree, no tmux, no container, no git subprocess —
	// so nothing is established anywhere until the daemon creates the real session.
	//
	instance, err := session.NewInstance(session.InstanceOptions{
		Title:                          "",
		Path:                           repoPath,
		Program:                        m.pendingProgram,
		Backend:                        session.BackendLocal,
		ProvisionSessionEnvPassthrough: append([]string(nil), m.appConfig.SessionEnvPassthrough...),
	})
	if err != nil {
		return m, m.handleError(err)
	}
	_ = instance.Transition(session.BeginCreate())
	m.store.AddInstance(instance)
	m.sidebar.SelectInstance(instance)
	m.namingInstance = instance
	// #2470: generate the autocreate-name suggestion and show it as shadow text on
	// the naming row. Pressing enter on the untouched field adopts it.
	m.namingPlaceholder = m.suggestSessionName(instance)
	m.sidebar.SetNamingPlaceholder(instance, m.namingPlaceholder)
	m.state = stateNew
	m.menu.SetNamingHasPrompt(false)
	m.menu.SetNamingBackend(false)
	m.menu.SetNamingAccount(false)
	m.menu.SetState(ui.StateNewInstance)
	// Ask which account this project would apply (#3386). Asynchronous and
	// optional: the form is fully usable before it lands, and if it never does the
	// create behaves exactly as it did before this key existed — the daemon applies
	// the same default either way. What the preselection buys is that the user SEES
	// it and can change it.
	accountFetch := m.fetchAccountDefault(instance, sessionenv.AgentForCommand(m.pendingProgram))
	if !isRemoteTarget() {
		return m, accountFetch
	}
	// The remote daemon resolves its OWN default_program (#4889 review): the
	// launch-time local seed displayed above is only a guess at remote state,
	// and the account preselect just fired against that guess's agent. Asking
	// ListPrograms corrects the field — and re-fires the account fetch for the
	// resolved agent — when the daemon's default differs. Submit waits on the
	// answer so "" cannot resolve a program the user never saw.
	m.remoteDefaultPending = true
	return m, tea.Batch(accountFetch, m.fetchRemoteDefaultProgram(instance))
}

// noActiveProjectNotice is the refusal a create gets with no active project.
//
// The action and its key LEAD the sentence and the explanation trails: the
// transient notice clips to the terminal width and the TAIL is what vanishes
// (#1973). Driving the real TUI at 120 columns with the explanation first cut
// exactly the recovery step, leaving a message that named a problem and no way
// out of it.
//
// The key is read from the binding rather than spelled here, because
// switch_project is REBINDABLE (`[keys]` config) — keys.go's own note on
// KeySetPrompt calls out that hardcoding ctrl+p "would silently drift the day a
// user rebinds that action". A user who unbound it entirely gets the section
// instead of a key that does nothing.
func noActiveProjectNotice(enterPicks, projectsFocused bool) string {
	return "select a project first — " + switchProjectPickHint(enterPicks, projectsFocused) +
		"; af is running with no active project, so there is no repo for this session to live in"
}

// switchProjectPickHint names the action that gives af an active project — for
// the region the user is actually in.
//
// The focus argument is the whole point, not a refinement. The Projects section
// is captive (#1620) and suppresses ctrl+p BY NAME (see handleProjectsFocus), so
// a hint advertising it while that section holds the keyboard would reproduce
// #2830 exactly, with a different dead key — and registry mode focuses Projects
// at startup, which is precisely when this copy is on screen. From there Enter
// on the cursor row IS the pick, and it is what the section header already
// advertises. Everywhere else Enter means something else and ctrl+p is the key
// that works.
//
// Shared by every surface that has to say this — the create refusal above and
// the registry-mode empty workspace — so the two cannot drift on wording. What
// they must NOT share is a single hardcoded key, which is the mistake this
// signature exists to prevent.
func switchProjectPickHint(enterPicks, projectsFocused bool) string {
	switch {
	case enterPicks:
		// A fixed binding with no configKey: Enter cannot be rebound away from
		// this, so the word is literal.
		//
		// It used to be justified by matching the Projects header's own
		// "· enter switch"; since #3642 that header reads its glyph off the
		// binding table like every other chip and renders "· ↵ switch". The word
		// stays here anyway, because this is PROSE and that is a key chip:
		// "press ↵ to pick one" is worse English, and the footer has always
		// rendered the chip form for the same action (ui/menu.go).
		return "press enter to pick one"
	case projectsFocused:
		// Focused on a captive section with no rows: Enter is a no-op
		// (SelectedProject reports false) and ctrl+p is suppressed, so the only
		// honest instruction is to leave the section first. Startup no longer
		// lands here, but a user can still tab in.
		return "press esc, then " + switchProjectKeyPhrase() + " to add one"
	default:
		return switchProjectKeyPhrase() + " to pick one"
	}
}

// switchProjectKeyPhrase names the project-switch key, or the section when the
// action is unbound — still actionable, unlike a key that does nothing.
func switchProjectKeyPhrase() string {
	if k := keys.GlobalKeyBindings[keys.KeySwitchProject].Help().Key; k != "" {
		return fmt.Sprintf("press %s", k)
	}
	return "use the Projects section"
}

// suggestSessionName picks a readable random "adjective-noun" name for the
// naming placeholder (#2470) that the naming flow would not itself reject: it
// avoids the reserved root title and any existing title git.TitlesCollide flags
// — the exact rule the enter handler enforces — so accepting the placeholder on
// an empty submit passes those same checks. A residual collision (a session
// created during naming) is still caught at submit and, authoritatively, by the
// daemon.
func (m *home) suggestSessionName(naming *session.Instance) string {
	prefix := ""
	if m.appConfig != nil {
		prefix = m.appConfig.BranchPrefix
	}
	return namegen.Suggest(func(name string) bool {
		// The same admission question the submit gate asks, for the same reason:
		// a suggestion the create would refuse is not a suggestion. namegen emits
		// no whitespace, so the two predicates cannot differ on anything it can
		// produce today — but one file holding two answers to one question is the
		// drift #3756 is about.
		if session.ReservedTitleCollision(name) != "" {
			return true
		}
		for _, other := range m.store.GetInstances() {
			if other == naming {
				continue
			}
			if git.TitlesCollide(other.Title, name, prefix) {
				return true
			}
		}
		return false
	})
}

// clearNamingPlaceholder drops the autocreate-name shadow text (#2470) from both
// the model and the sidebar renderer when a naming session ends by any route.
func (m *home) clearNamingPlaceholder() {
	m.namingPlaceholder = ""
	m.sidebar.SetNamingPlaceholder(nil, "")
}

// projectsFocused reports whether the captive Projects section currently holds
// the keyboard. Which keys are live depends on it (#1620), so any copy naming a
// key has to ask.
func (m *home) projectsFocused() bool {
	return m.ring.Active() == layout.RegionProjects
}

// enterPicksAProject reports whether Enter would actually switch projects right
// now: the captive section must hold the keyboard AND have a row under the
// cursor. With no rows SelectedProject reports false and handleProjectsFocus
// consumes Enter as a no-op, so advertising it there is another dead key.
func (m *home) enterPicksAProject() bool {
	return m.projectsFocused() && m.projects.HasProjects()
}
