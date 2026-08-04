package app

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/sachiniyer/agent-factory/internal/namegen"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/ui"
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
		m.pendingBackend = ""
		m.pendingForceRemote = false
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
		// "root" is reserved for the daemon-managed root agent (#1106). The
		// daemon's reserveCreate is the authoritative gate; rejecting here
		// keeps the user in the naming overlay instead of surfacing the
		// error after submit, mirroring the #936 collision pre-check below.
		if session.IsReservedTitle(title) {
			return m, m.handleNotice(fmt.Errorf("title %q is reserved for the daemon-managed root agent; pick another name", title))
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
			Backend:     session.BackendKind(m.pendingBackend),
			ForceRemote: m.pendingForceRemote,
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
		// Read the pending prompt here, on the event loop, and clear it with the
		// rest of the naming state: the cmd below runs off-loop, so reading
		// m.pendingPrompt from inside the closure would race the next create's
		// reset. TrimSpace so a field holding only whitespace is "no prompt"
		// rather than a stray newline delivered to the agent.
		prompt := strings.TrimSpace(m.pendingPrompt)
		m.pendingPrompt = ""
		// Same reasoning for the backend picked in the ctrl+r field (#1933): read it
		// on the loop, clear it with the rest of the naming state.
		backend := m.pendingBackend
		m.pendingBackend = ""
		// And for `N`. It is read from the model, not from instance.Capabilities(),
		// because the placeholder no longer resolves a runtime to carry it (#2599) —
		// this is the value that keeps a remote create remote.
		forceRemote := m.pendingForceRemote
		m.pendingForceRemote = false
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
				Program:  instance.Program,
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
				Backend:     backend,
				ForceRemote: forceRemote,
			}
			started, err := start(instance, req)
			return instanceStartedMsg{
				instance: instance,
				started:  started,
				err:      err,
			}
		}

		return m, tea.Batch(tea.WindowSize(), m.selectionChanged(), startCmd)
	case tea.KeyTab:
		// Open program selection overlay
		items := make([]string, len(tmux.SupportedPrograms))
		selectedIdx := 0
		for i, p := range tmux.SupportedPrograms {
			items[i] = p
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
		newTitle := instance.Title + string(cleanRunes)
		// The length cap is user feedback; the SetTitle error below is not (see
		// the commit site above) — it keeps ERROR severity here too.
		if runewidth.StringWidth(newTitle) > 32 {
			return m, m.handleNotice(fmt.Errorf("title cannot be longer than 32 characters"))
		}
		if err := instance.SetTitle(newTitle); err != nil {
			return m, m.handleError(err)
		}
	case tea.KeyBackspace:
		runes := []rune(instance.Title)
		if len(runes) == 0 {
			return m, nil
		}
		if err := instance.SetTitle(string(runes[:len(runes)-1])); err != nil {
			return m, m.handleError(err)
		}
	case tea.KeySpace:
		newTitle := instance.Title + " "
		if runewidth.StringWidth(newTitle) > 32 {
			return m, m.handleNotice(fmt.Errorf("title cannot be longer than 32 characters"))
		}
		if err := instance.SetTitle(newTitle); err != nil {
			return m, m.handleError(err)
		}
	case tea.KeyEsc:
		// Kill by the captured namingInstance pointer, not the live selection
		// (#717) — see the ctrl+c branch above for the full rationale.
		if err := m.store.KillInstance(m.namingInstance); err != nil {
			log.ErrorLog.Printf("failed to clean up instance on cancel: %v", err)
		}
		m.namingInstance = nil
		m.clearNamingPlaceholder()
		m.pendingPrompt = ""
		m.pendingBackend = ""
		m.pendingForceRemote = false
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

// startNewInstance creates a new instance and enters stateNew for naming.
// If remote is true, the instance is forced to use the remote hook backend.
func (m *home) startNewInstance(remote bool) (tea.Model, tea.Cmd) {
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
	// Both keys, not just `n`. `N` did refuse in registry mode, but by asking the
	// cwd about remote_hooks and reporting that repo-shaped answer — sending a
	// user with no project selected off to configure hooks for a directory that
	// is not even a project. Ahead of that check, this one names the actual
	// blocker, which is also why nothing below needs a repo-less path anymore.
	if m.repoRoot == "" {
		return m, m.handleNotice(errors.New(noActiveProjectNotice()))
	}
	m.pendingProgram = m.program
	// Every create starts with an empty prompt field and an unchosen backend. The
	// cancel paths clear both too, but this is the authoritative reset: it also
	// covers a create that ended by any route other than Enter/Esc/ctrl+c.
	m.pendingPrompt = ""
	m.pendingBackend = ""
	m.pendingForceRemote = false
	if m.pendingProgram == "" && m.appConfig != nil {
		m.pendingProgram = m.appConfig.DefaultProgram
	}
	// Target the ACTIVE project's repo root, not the process cwd: after an
	// in-place project switch (#1461) the active repo is m.repoRoot, which may no
	// longer be where af was launched. At launch m.repoRoot is the cwd's repo, so
	// this is equivalent for the unswitched case. The guard above guarantees it is
	// set.
	repoPath := m.repoRoot
	if remote {
		configured, err := session.RemoteHooksConfiguredForPath(repoPath)
		if err != nil {
			return m, m.handleError(err)
		}
		if !configured {
			// The menu advertises `N new remote` next to `n new`, so an
			// unconfigured repo must SAY that rather than eat the keypress
			// (#2020). RemoteHooksConfiguredForPath reports the unconfigured
			// repo as (false, nil) — a normal empty state, not an error — which
			// is why only a MALFORMED remote_hooks config used to surface
			// anything, and the common case (no remote_hooks at all) did
			// nothing at all. Every other gated action in the TUI explains
			// itself; this was the one that did not.
			//
			// The cause and the fix lead the sentence: the transient notice
			// clips to the terminal width and the tail is what disappears
			// (#1973), so the guide URL — recoverable under `E details` — goes
			// last.
			return m, m.handleNotice(fmt.Errorf(
				"remote sessions need a remote_hooks backend configured for this repo — press n for a local session, or configure remote_hooks and try again. Guide: https://sachiniyer.github.io/agent-factory/remote-hooks/"))
		}
	}
	m.pendingForceRemote = remote
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
	// It costs no information: the placeholder's kind was never an input to the
	// create. Both selectors travel to the daemon explicitly on
	// sessionStartRequest — the naming form's backend field as Backend (#1933) and
	// `N` as ForceRemote, now carried in m.pendingForceRemote rather than read back
	// off this instance's capabilities — and the repo's own `backend` key is
	// resolved daemon-side. All three still decide the session that gets built.
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
	m.menu.SetState(ui.StateNewInstance)
	return m, nil
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
func noActiveProjectNotice() string {
	pick := "pick one in the Projects section"
	if k := keys.GlobalKeyBindings[keys.KeySwitchProject].Help().Key; k != "" {
		pick = fmt.Sprintf("press %s to pick one", k)
	}
	return "select a project first — " + pick +
		"; af is running with no active project, so there is no repo for this session to live in"
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
		if session.IsReservedTitle(name) {
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
