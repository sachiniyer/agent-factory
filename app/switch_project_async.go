package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// This file keeps the project switch's snapshot fetch off the event loop
// (#7377), following kill_check.go's pattern for a key action whose blocking
// work decides what the user sees next:
//
//  1. switchProject does only in-memory work: it records the pending switch and
//     puts a static "Loading <project>…" line on the bar (no spinner, #1766).
//  2. The fetch runs in the returned tea.Cmd, waiting out a warming daemon
//     there, through the fetcher captured on the event loop.
//  3. The reply carries the switch's generation, and Update applies only the
//     reply for the switch still pending. A switch that was cancelled (Esc) or
//     superseded by a newer one is never applied.
//  4. While a switch is pending, input is held: Esc cancels it, Ctrl+C quits,
//     and every other key and mouse event is dropped. The current project is
//     still on screen, but the user has already asked to leave it, so a key
//     meant for the incoming project must not act on the outgoing one.
//  5. Background work for the outgoing project that finishes meanwhile treats
//     the user as having left it (leavingProject): it opens no modal, restores
//     no draft and enters no pane, so the switch commits from the state it
//     started in (#7379). Its outcome goes on the bar instead.

// projectSwitchCancelHint is what the loading line says Esc does.
const projectSwitchCancelHint = "esc to cancel"

// pendingProjectSwitch is a switch whose snapshot fetch is still in flight.
type pendingProjectSwitch struct {
	gen    uint64
	repo   *config.RepoContext
	ctx    context.Context
	cancel context.CancelFunc
}

// projectSwitchFetchedMsg is the reply to a switch's snapshot fetch.
type projectSwitchFetchedMsg struct {
	gen     uint64
	repo    *config.RepoContext
	data    []session.InstanceData
	skipped []daemon.SkippedRepo
	err     error
}

// switchProject re-scopes the running TUI to repo in place (#1461). It returns
// at once: the incoming project's snapshot is fetched in the returned tea.Cmd,
// and commitProjectSwitch applies it when the reply for this switch lands. The
// fetch comes FIRST, while the outgoing project is still fully intact, so a
// failed one leaves the current project exactly as it was (#1788).
//
// A newer switch supersedes one still loading. Switching to the project already
// shown cancels any pending switch and is otherwise a no-op.
func (m *home) switchProject(repo *config.RepoContext) (tea.Model, tea.Cmd) {
	m.cancelProjectSwitch()
	if repo.ID == m.repoID {
		return m, nil
	}
	parent := m.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	m.projectSwitchGen++
	m.projectSwitch = &pendingProjectSwitch{gen: m.projectSwitchGen, repo: repo, ctx: ctx, cancel: cancel}
	m.errBox.SetPinned(m.projectSwitchLoadingLine())
	return m, projectSwitchFetchCmd(ctx, m.projectSwitchGen, repo, m.snapshotFetcher)
}

// leavingProject reports whether the user has asked to leave the project on
// screen: a switch is pending. Background work for that project — a create,
// kill or archive finishing — must then treat the user as gone: it raises no
// modal, restores no draft and activates no pane, because the switch would
// otherwise commit with that project's modal or form over the next one
// (#7379). It reports its outcome on the bar.
func (m *home) leavingProject() bool { return m.projectSwitch != nil }

// projectSwitchFetchCmd fetches repo's snapshot off the event loop and replies
// with the switch's generation. fetch is captured by the caller on the event
// loop, the same reason fetchSnapshotCmd captures its seams.
func projectSwitchFetchCmd(
	ctx context.Context,
	gen uint64,
	repo *config.RepoContext,
	fetch func(context.Context, string) (daemon.SnapshotResponse, error),
) tea.Cmd {
	return func() tea.Msg {
		// No onWarming: this runs under the alt-screen, where a write to stdout
		// would paint over the TUI. The loading line already says what is
		// happening.
		data, skipped, err := fetchSnapshotWaitingOutWarmup(ctx, fetch, repo.ID, nil)
		return projectSwitchFetchedMsg{gen: gen, repo: repo, data: data, skipped: skipped, err: err}
	}
}

// handleProjectSwitchFetched applies the reply for the switch still pending
// and drops any other: one the user cancelled, or one a newer switch replaced.
func (m *home) handleProjectSwitchFetched(msg projectSwitchFetchedMsg) (tea.Model, tea.Cmd) {
	if m.projectSwitch == nil || m.projectSwitch.gen != msg.gen {
		return m, nil
	}
	m.cancelProjectSwitch()
	if msg.err != nil {
		return m, m.handleError(projectSwitchFailure(msg.repo, msg.err))
	}
	return m.commitProjectSwitch(msg.repo, msg.data, msg.skipped)
}

// projectSwitchFailure is the toast for a switch whose fetch failed. A daemon
// that never answered gets the wait and the next step, never the transport
// text, which goes to the log instead (#7377).
func projectSwitchFailure(repo *config.RepoContext, err error) error {
	name := filepath.Base(repo.Root)
	var noAnswer *daemonNoAnswerError
	if errors.As(err, &noAnswer) {
		log.WarningLog.Printf("switch project: snapshot for %s: %v", repo.Root, noAnswer.err)
		return fmt.Errorf("failed to load sessions for %s: %s — try again, or check the daemon with `af daemon status`", name, noAnswer)
	}
	return fmt.Errorf("failed to load sessions for %s: %w", name, err)
}

// cancelProjectSwitch abandons the pending switch, if any: its fetch is
// cancelled, and its reply, should one still arrive, is dropped.
func (m *home) cancelProjectSwitch() {
	if m.projectSwitch == nil {
		return
	}
	m.projectSwitch.cancel()
	m.projectSwitch = nil
	m.errBox.SetPinned("")
}

// projectSwitchLoadingLine is the bar's pinned line while a switch is pending,
// or "" when none is. It is pinned rather than standing so a notice never hides
// it: while it is up, keys are held, and the bar is the only thing saying why
// (#7379).
func (m *home) projectSwitchLoadingLine() string {
	if m.projectSwitch == nil {
		return ""
	}
	return fmt.Sprintf("Loading %s… · %s", filepath.Base(m.projectSwitch.repo.Root), projectSwitchCancelHint)
}

// handleKeyWhileSwitchingProject is the keyboard while a switch is pending:
// Esc cancels it and stays on the current project, Ctrl+C is still the hard
// exit, and every other key is dropped rather than acted on.
func (m *home) handleKeyWhileSwitchingProject(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.Type == tea.KeyEsc:
		m.cancelProjectSwitch()
		return m, nil
	case msg.String() == "ctrl+c":
		return m.handleQuit()
	}
	return m, nil
}

// handleCreateInOtherProject applies the reply of a create made in a project the
// TUI has since switched away from (#7381). Nothing of it touches the store: the
// store holds the current project's rows, so the title-based remove and replace
// the create handler uses would act on another project's session, and an added
// row would show a session of one project in another's sidebar. The daemon owns
// the session either way, and that project's snapshot shows it on the way back.
// A failed create's draft is kept for its own project's next create, and every
// message names the project.
func (m *home) handleCreateInOtherProject(msg instanceStartedMsg) (tea.Model, tea.Cmd) {
	project := filepath.Base(msg.repoRoot)
	if msg.err != nil {
		title := msg.instance.Title
		if mutationMayHaveLanded(msg.err) {
			return m, m.handleError(mutationOutcomeError(
				fmt.Sprintf("creating session %q in %s", title, project), project+"'s session list", msg.err))
		}
		detail := "Your input is retained. " + msg.err.Error()
		if msg.draft != nil {
			m.keepFailedCreate(&msg)
			detail += " The next create in " + project + " reopens the failed draft."
		}
		return m, m.handleError(elsewhereFailure("Cannot create session", title, project, detail))
	}
	started := msg.instance
	if msg.started != nil {
		started = msg.started
	}
	if err := accountSkewRefusal(msg.account, started); err != nil {
		return m, m.handleError(err)
	}
	return m, m.showTransientMessage(fmt.Sprintf("Session %q started in %s", started.Title, project))
}
