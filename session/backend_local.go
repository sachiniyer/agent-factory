package session

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// resolveProgramForInstance returns the actual tmux command for an instance.
// Resolution chain: agent enum -> cfg.ProgramOverrides[agent] (if set) ->
// bare agent name. The overrides come from the repo-resolved config (global
// program_overrides merged with the repo's .agent-factory/config.json) when
// the instance path belongs to a git repo; outside a repo, or when repo
// resolution fails, the global config alone applies. When AutoYes is set and
// the RESOLVED command actually runs claude, the --permission-mode
// bypassPermissions flag is appended to it — claude needs the flag at exec
// time, and Instance.Program now holds only the bare enum so the append can
// no longer happen in main.go. A nil cfg (e.g. tests that don't materialize a config)
// falls back to the raw Program string so legacy free-form values still
// reach tmux verbatim.
func resolveProgramForInstance(i *Instance) string {
	var cfg *config.Config
	if repo, err := config.RepoFromPath(i.Path); err == nil {
		if resolved, rerr := config.ResolveConfig(repo.Root); rerr == nil {
			cfg = &resolved.Config
		} else {
			log.WarningLog.Printf("failed to resolve repo config when resolving program for %q: %v", i.Title, rerr)
		}
	}
	if cfg == nil {
		loaded, err := config.LoadConfig()
		if err != nil {
			log.WarningLog.Printf("failed to load config when resolving program for %q: %v", i.Title, err)
			loaded = nil
		}
		cfg = loaded
	}
	resolved := config.ResolveProgram(cfg, i.Program)
	// Key the claude-only flag off the agent the RESOLVED command actually
	// runs, not the config-name enum: an override may point "claude" at a
	// different program, which would exit on the unknown flag (#1116).
	//
	// opencode is deliberately NOT wired here, despite owning an obvious-looking
	// analog. Its --dangerously-skip-permissions flag belongs to the `opencode
	// run` SUBCOMMAND, not to the bare TUI command af launches; passing it to the
	// TUI makes opencode print its help and exit — byte-identical to passing a
	// nonsense flag (both verified against 0.0.0-main-202604230742). Wiring it
	// would kill every AutoYes opencode spawn as an opaque readiness timeout,
	// which is exactly the #1043/#1116/#1131 class this comment block exists to
	// prevent. opencode's TUI also auto-approves tools in its default config
	// (it ran `rm README.md` unprompted), so there is nothing for AutoYes to
	// dismiss today.
	if i.AutoYes && tmux.DetectAgentFromCommand(resolved) == tmux.ProgramClaude &&
		// Sessions persisted by pre-#659 binaries got the flag appended at
		// create-time in main.go (19c0dd9), so legacy Instance.Program values
		// can already carry it; appending again duplicates the flag on every
		// restore (#818). A substring check suffices: claude exposes no short
		// form of --permission-mode, and the check also matches the
		// =-attached spelling.
		!strings.Contains(resolved, "--permission-mode") {
		resolved = resolved + " --permission-mode bypassPermissions"
	}
	return resolved
}

// LocalBackend implements Backend using local tmux sessions and git worktrees.
type LocalBackend struct{}

func (b *LocalBackend) Type() string { return "local" }

// Capabilities reports the local runtime's full-parity descriptor: a local git
// worktree driven by tmux, supporting every optional operation (#1592 Phase 1).
func (b *LocalBackend) Capabilities() Capabilities {
	return Capabilities{
		Workspace:        WorkspaceLocalWorktree,
		Archive:          true,
		Recover:          true,
		TabManagement:    true,
		TerminalTab:      true,
		InteractiveInput: true,
	}
}

// WorktreeUnavailableError marks a recover/respawn failure caused by the
// persisted worktree path being unavailable before tmux is touched. The daemon
// uses the typed shape to add one-shot diagnostics for vanished live worktrees
// without parsing error strings (#1303).
type WorktreeUnavailableError struct {
	Title        string
	WorktreePath string
	Err          error
}

func (e *WorktreeUnavailableError) Error() string {
	return fmt.Sprintf("recover: session %q worktree unavailable: %v", e.Title, e.Err)
}

func (e *WorktreeUnavailableError) Unwrap() error {
	return e.Err
}

// Start brings a local session up in two explicit phases (#1592 Phase 1 PR4):
// provision establishes WHERE the agent will run (the tmux session handle bound
// to the instance, plus — for a fresh create — the git worktree record and its
// branch), then launch starts WHAT runs in it (materializing the worktree on
// disk and spawning/reconnecting the agent process and its tabs). The split is
// behavior-preserving: Start = provision then launch is exactly the monolithic
// Start it replaced (same order, side effects, and errors), and it prepares the
// backend for the future agent-server "provision-and-expose" model, where
// provision spins up an off-box workspace and launch exposes the agent stream.
func (b *LocalBackend) Start(i *Instance, firstTimeSetup bool) error {
	if err := b.Provision(i, firstTimeSetup); err != nil {
		return err
	}
	return b.Launch(i, firstTimeSetup)
}

// Provision establishes the local workspace a session will run in WITHOUT
// starting any agent process (#1592 Phase 1 PR4): it binds the instance's tmux
// session handle and, on a first-time create, computes the git worktree record +
// branch name. Nothing here spawns a tmux server session, materializes the
// worktree on disk, or launches the agent program — those are launch's job.
// NewGitWorktree is purely in-memory (the disk-mutating `git worktree add` runs
// in worktree.Setup(), which launch owns), so a provision failure leaves nothing
// on disk to clean up and returns before launch's cleanup scope is ever entered
// — exactly as the pre-split Start did (its NewGitWorktree failure returned
// before the deferred cleanup handler was registered).
func (b *LocalBackend) Provision(i *Instance, firstTimeSetup bool) error {
	if strings.TrimSpace(i.Title) == "" {
		return fmt.Errorf("instance title cannot be empty")
	}

	var tmuxSession *tmux.TmuxSession
	i.mu.RLock()
	existingSession := i.tmuxLocked()
	i.mu.RUnlock()

	if existingSession != nil {
		// Use existing tmux session (useful for testing)
		tmuxSession = existingSession
	} else {
		// Create new tmux session with repo-scoped name. The program
		// passed here is a placeholder — SetProgram below replaces it
		// with the override-resolved + system-prompt-injected form
		// before Start/Restore.
		tmuxSession = tmux.NewTmuxSessionForRepo(i.Title, i.Path, i.Program)
	}

	i.mu.Lock()
	i.setTmuxLocked(tmuxSession)
	i.mu.Unlock()

	if firstTimeSetup {
		var (
			gitWorktree *git.GitWorktree
			branchName  string
			err         error
		)
		if i.inPlace {
			// --here: attach to the repo's own working tree at its current
			// branch. The worktree is external, so Setup() below is a no-op
			// and Cleanup()/Kill never removes the user's tree or branch.
			gitWorktree, branchName, err = git.NewGitWorktreeInPlace(i.Path)
		} else {
			gitWorktree, branchName, err = git.NewGitWorktree(i.Path, i.Title)
		}
		if err != nil {
			return fmt.Errorf("failed to create git worktree: %w", err)
		}
		i.mu.Lock()
		i.gitWorktree = gitWorktree
		i.Branch = branchName
		i.mu.Unlock()
	}

	return nil
}

// Launch starts (or restores) the agent PROCESS in the workspace Provision
// established (#1592 Phase 1 PR4): it materializes the worktree on disk
// (worktree.Setup on a fresh create), spawns or reconnects the tmux session, and
// brings up the non-agent tabs. It owns the failure-cleanup scope — a launch
// failure tears down provision's worktree via i.Kill on a fresh create, or
// releases only the attach PTY on a restore. worktree.Setup deliberately stays
// here rather than in provision: it is the first on-disk mutation, and its
// failure needs the same teardown as any other launch failure, so it belongs
// inside this cleanup scope. Behavior is identical to the pre-split Start — this
// is exactly the code that followed provision's work in the monolithic form.
func (b *LocalBackend) Launch(i *Instance, firstTimeSetup bool) error {
	i.mu.RLock()
	tmuxSession := i.tmuxLocked()
	i.mu.RUnlock()

	// Setup error handler to cleanup resources on any error.
	// Kill() acquires its own lock, so we must not hold i.mu here.
	var setupErr error
	defer func() {
		if setupErr != nil {
			if firstTimeSetup {
				// New session: full cleanup (tmux + worktree) is safe.
				if cleanupErr := i.Kill(); cleanupErr != nil {
					setupErr = fmt.Errorf("%v (cleanup error: %v)", setupErr, cleanupErr)
				}
			} else {
				// Restore: the server-side tmux session may already be live
				// (has-session passed) and we only failed to allocate the
				// local attach PTY — e.g. EMFILE/ENOMEM in Restore (#895).
				// Use CloseAttachOnly, NOT Close: Close runs `tmux
				// kill-session` and would destroy a recoverable live session
				// (scrollback + running processes), turning a transient attach
				// failure into data loss. CloseAttachOnly releases only the
				// local attach resources this object opened and leaves the
				// server session and its worktree intact for a later retry.
				//
				// Releasing only the AGENT tab here is deliberate (#1065):
				// every restore failure point precedes setupTabs, so no other
				// tab has opened its attach PTY yet, and their tmux refs must
				// survive for a later retry to reconnect each tab by its exact
				// persisted name. The discard-duplicate path
				// (LocalBackend.CloseAttachOnly) is different: there setupTabs
				// has already run, so it must release EVERY tab's PTY.
				i.mu.Lock()
				ts := i.tmuxLocked()
				i.setTmuxLocked(nil)
				i.started = false
				i.mu.Unlock()
				if ts != nil {
					if cleanupErr := ts.CloseAttachOnly(); cleanupErr != nil {
						setupErr = fmt.Errorf("%v (cleanup error: %v)", setupErr, cleanupErr)
					}
				}
			}
		} else {
			i.mu.Lock()
			i.started = true
			i.mu.Unlock()
		}
	}()

	if !firstTimeSetup {
		// A persisted Dead/Lost instance's tmux session was killed out from
		// under it and the daemon explicitly recorded that (#935/#1108).
		// Loading it must NOT silently re-spawn that session: TmuxSession.Restore
		// re-spawns a missing session when workDir is non-empty (the #386
		// reboot-recovery path) and setupTabs would likewise re-spawn the shell
		// tab — together resurrecting a session behind the daemon's back,
		// contradicting its persisted state (#970). Return before both. The
		// deferred handler still flips started=true, so the row keeps rendering
		// its status, survives the next SaveInstances checkpoint (which skips
		// !Started instances), and stays killable; the daemon liveness poll
		// re-confirms the state because the bound session does not exist
		// server-side. A Lost session's recovery is via the daemon's restore
		// loop or user-initiated restore (#1108 PR 2, #1300), never a
		// load-time side effect; a tombstoned record's only future is
		// having its kill finished.
		if status := i.GetStatus(); status == Dead || status == Lost || i.UserKilled() {
			return nil
		}

		// Reuse existing session. Pass the worktree path so Restore can
		// re-spawn the tmux session if the server died across a reboot
		// (see #386). When the worktree is unavailable (e.g. tests inject
		// a tmux session without a gitWorktree), pass empty string.
		i.mu.RLock()
		gw := i.gitWorktree
		i.mu.RUnlock()
		var workDir string
		if gw != nil {
			workDir = gw.GetWorktreePath()
		}
		// Re-inject the system prompt so a lazy re-spawn (tmux server died
		// across a reboot, see #386/#444) starts the agent with the same
		// program string as the original first-time launch — most
		// importantly, claude-code's --plugin-dir flag, without which
		// /af-* slash commands silently vanish post-reboot (#511).
		// Setting the program on the existing attach path is harmless:
		// attach-session does not re-exec the program.
		if workDir != "" {
			tmuxSession.SetProgram(injectSystemPrompt(resolveProgramForInstance(i)))
		}
		if err := tmuxSession.Restore(workDir); err != nil {
			setupErr = fmt.Errorf("failed to restore existing session: %w", err)
			return setupErr
		}
	} else {
		i.mu.RLock()
		gw := i.gitWorktree
		i.mu.RUnlock()

		// Setup git worktree first
		if err := gw.Setup(); err != nil {
			setupErr = fmt.Errorf("failed to setup git worktree: %w", err)
			return setupErr
		}

		// Inject Agent Factory instructions into the session. On a first launch
		// only, seed provider conversation identity when a supported agent offers
		// an explicit id flag; restore/respawn paths keep their existing latest-
		// session behavior until PR2 wires resume-by-recorded-id.
		program := prepareLaunchConversation(i, resolveProgramForInstance(i))
		tmuxSession.SetProgram(injectSystemPrompt(program))

		// Create new session
		if err := tmuxSession.Start(gw.GetWorktreePath()); err != nil {
			// Cleanup git worktree if tmux session creation fails
			if cleanupErr := gw.Cleanup(); cleanupErr != nil {
				err = fmt.Errorf("%v (cleanup error: %v)", err, cleanupErr)
			}
			setupErr = fmt.Errorf("failed to start new session: %w", err)
			return setupErr
		}
	}

	// Bring up the instance's non-agent tabs (#930 PR 2/4). This is best-effort:
	// a tab failure leaves the instance fully usable with just the agent tab (the
	// failed tab renders a fallback), so it must not fail the whole start. Runs
	// after the agent session is up so each tab can be a sibling of it (sharing
	// tmux deps).
	b.setupTabs(i)

	return nil
}

// Recover re-establishes a Lost instance's tmux sessions (#1108): re-spawn the
// agent program in its worktree with the same resolved-program flag injection
// as a first-time launch (#1132 choke-point — never hand-rolled flag logic),
// then bring the other tabs back through the same setupTabs path a restore
// uses. Invoked by the daemon's restore loop and by user-initiated restore
// (#1300); the #970 guard in Start keeps loads side-effect free.
//
// Idempotence across retries: the injected program is recomputed from the
// clean persisted i.Program on every attempt (SetProgram replaces, never
// appends), so repeated failures never accumulate duplicate flags. On failure
// only the agent tab's attach resources are released (the #1065 rule: no other
// tab has opened a PTY yet on this path) and the tmux refs are kept, so the
// next tick's retry reconnects each tab by its exact persisted name; the
// instance stays a killable Lost row throughout.
func (b *LocalBackend) Recover(i *Instance) error {
	if status := i.GetStatus(); status != Lost {
		return fmt.Errorf("recover: session %q is %v, not Lost", i.Title, status)
	}
	if i.UserKilled() {
		return fmt.Errorf("recover: session %q carries a kill tombstone", i.Title)
	}
	return b.respawn(i)
}

// Respawn re-establishes an instance's backing tmux session in place WITHOUT any
// liveness precondition — the guard-free core Recover wraps. It exists so the
// usage-limit manual-retry (#1146) can re-spawn an agent that exited while blocked
// at a limit wall: that session is LiveLimitReached, which Recover's !Lost guard
// would reject, but the re-spawn mechanics are identical. Callers own the
// precondition (Recover enforces Lost/no-tombstone; resumeFromLimit enforces
// LimitReached/no-tombstone under the target lock).
func (b *LocalBackend) Respawn(i *Instance) error {
	return b.respawn(i)
}

// respawn holds the shared re-spawn mechanics for Recover and Respawn: re-spawn
// the agent program in its worktree with the same resolved-program flag injection
// as a first-time launch (#1132 choke-point — never hand-rolled flag logic) and
// the resume-path rewrite Restore applies (resumeProgram: claude --continue,
// codex resume --last), then bring the other tabs back through the same setupTabs
// path a restore uses. No liveness guard — the exported wrappers own that.
//
// Idempotence across retries: the injected program is recomputed from the clean
// persisted i.Program on every attempt (SetProgram replaces, never appends), so
// repeated failures never accumulate duplicate flags. On failure only the agent
// tab's attach resources are released (the #1065 rule: no other tab has opened a
// PTY yet on this path) and the tmux refs are kept, so the next tick's retry
// reconnects each tab by its exact persisted name.
func (b *LocalBackend) respawn(i *Instance) error {
	i.mu.RLock()
	ts := i.tmuxLocked()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if ts == nil {
		return fmt.Errorf("recover: session %q has no tmux binding", i.Title)
	}
	var workDir string
	if gw != nil {
		workDir = gw.GetWorktreePath()
	}
	if workDir == "" {
		return fmt.Errorf("recover: session %q has no worktree to re-spawn into", i.Title)
	}
	resolvedProgram := resolveProgramForInstance(i)
	if _, err := os.Stat(workDir); err != nil {
		if !os.IsNotExist(err) {
			// Surface the real cause instead of a generic tmux new-session error:
			// a deleted worktree is the expected permanent-failure shape, and the
			// restore loop's escalation log should say so.
			return &WorktreeUnavailableError{Title: i.Title, WorktreePath: workDir, Err: err}
		}
		if rebuildErr := gw.RebuildFromExistingBranch(); rebuildErr != nil {
			exactProgram, ok := prepareExactResumeConversation(i, resolvedProgram)
			if !ok {
				return &WorktreeUnavailableError{
					Title:        i.Title,
					WorktreePath: workDir,
					Err: fmt.Errorf("%w (rebuild from existing branch failed: %v; fresh rebuild requires a recorded conversation id for the resolved agent)",
						err, rebuildErr),
				}
			}
			if freshErr := gw.RebuildFreshFromRecordedBase(); freshErr != nil {
				return &WorktreeUnavailableError{
					Title:        i.Title,
					WorktreePath: workDir,
					Err: fmt.Errorf("%w (rebuild from existing branch failed: %v; fresh rebuild from recorded base failed: %v)",
						err, rebuildErr, freshErr),
				}
			}
			resolvedProgram = exactProgram
			log.InfoLog.Printf("recover: rebuilt missing worktree for session %q at %s from recorded base and recreated branch %s", i.Title, workDir, gw.GetBranchName())
		} else {
			log.InfoLog.Printf("recover: rebuilt missing worktree for session %q at %s from branch %s", i.Title, workDir, gw.GetBranchName())
		}
	}

	ts.SetProgram(injectSystemPrompt(prepareResumeConversation(i, resolvedProgram)))
	if err := ts.Restore(workDir); err != nil {
		if cleanupErr := ts.CloseAttachOnly(); cleanupErr != nil {
			err = fmt.Errorf("%v (cleanup error: %v)", err, cleanupErr)
		}
		return fmt.Errorf("recover: failed to re-spawn session %q: %w", i.Title, err)
	}
	b.setupTabs(i)

	// The program was just re-spawned and is booting: Running, exactly like a
	// fresh create. ConfirmLive clears the OpRestoring/OpCreating fence this
	// completion resolves while yielding to a kill/archive teardown fence (#1195
	// Phase 2d — the chokepoint form of MarkLive). The daemon poll re-derives
	// Ready/Running from the live session from here on and persists the transition.
	_ = i.Transition(ConfirmLive())

	// The re-spawned tmux is a new pane process; a PTY broker that was still holding
	// the dead pane's clientless capture must drop it so the next Subscribe streams
	// the live pane rather than a parked, silent readLoop (#1682). The memoized
	// accessor keeps this a no-op for sessions nobody ever streamed (empty broker
	// map) and skips a remote runtime's agent-server (not a localAgentServer).
	if as, ok := i.AgentServer().(*localAgentServer); ok {
		as.resetBrokerCaptures()
	}
	return nil
}

// setupTabs brings up an instance's non-agent tabs after its agent session is
// live. On restore it reconnects every persisted tab (shell and any later
// process tabs) to its exact tmux session by name so they survive an af/daemon
// restart, re-spawning in the worktree only if the tmux server died across a
// reboot (Restore handles both). A fresh instance comes up with only the agent
// tab (#1100) — terminal tabs are created on demand ('t' / `af sessions
// tab-create`), never automatically. The fresh-$SHELL fallback below only fires
// when a PERSISTED shell tab restored dead (#991), replacing it so the user
// lands on a working terminal instead of a corpse.
func (b *LocalBackend) setupTabs(i *Instance) {
	i.mu.RLock()
	agentTmux := i.tmuxLocked()
	gw := i.gitWorktree
	tabs := append([]*Tab(nil), i.Tabs...)
	i.mu.RUnlock()

	if agentTmux == nil || gw == nil {
		return
	}
	worktreePath := gw.GetWorktreePath()
	if worktreePath == "" {
		return
	}

	// Reconnect every persisted non-agent tab that carries a session (Tabs[0] is
	// the agent tab, already restored by Start). Track whether a persisted shell
	// tab exists at all, and whether at least one is live, to decide if the
	// dead-shell replacement below applies.
	hasShellTab := false
	hasLiveShell := false
	replacementShellName := ""
	for idx, tab := range tabs {
		if idx == 0 {
			continue
		}
		if tab.Kind == TabKindShell {
			hasShellTab = true
			if replacementShellName == "" {
				replacementShellName = tab.Name
			}
		}
		if tab.tmux == nil {
			continue
		}
		if err := tab.tmux.Restore(worktreePath); err != nil {
			log.WarningLog.Printf("restore tab %q for %q failed: %v", tab.Name, i.Title, err)
		}
		// Only count a shell tab as live when its tmux session actually exists
		// server-side after Restore. Restore (and its re-spawn) can fail — e.g.
		// the worktree was removed so `tmux new-session -c $workdir` errors —
		// leaving a dead shell tab. Gating on presence alone (the old behavior)
		// suppressed the fresh-shell fallback below and stranded the user with a
		// dead terminal (#991).
		if tab.Kind == TabKindShell && tab.tmux.DoesSessionExist() {
			hasLiveShell = true
		}
	}
	// No persisted shell tab means a fresh instance (or one whose user closed
	// every shell tab): come up with just the agent tab — a terminal tab is
	// never auto-created (#1100). With a live shell there is nothing to replace.
	if !hasShellTab || hasLiveShell {
		return
	}

	// A persisted shell tab restored dead (#991): create a fresh shell session
	// as a sibling of the agent session so it inherits the agent's PTY factory /
	// executor — real in production, mock in tests — keeping the create path
	// hermetic. The name extends the agent session's name deterministically so
	// it is collision-free and restorable by exact name.
	if replacementShellName == "" {
		replacementShellName = shellTabName
	}
	shellTmux := agentTmux.NewSiblingSession(agentTmux.SanitizedName()+"__"+replacementShellName, defaultShell())
	if err := shellTmux.Start(worktreePath); err != nil {
		log.WarningLog.Printf("start shell tab for %q failed: %v", i.Title, err)
		return
	}

	i.mu.Lock()
	if existing := i.shellTabLocked(); existing != nil {
		existing.tmux = shellTmux
	} else {
		tab := newShellTab(shellTmux)
		tab.Name = replacementShellName
		i.Tabs = append(i.Tabs, tab)
	}
	i.mu.Unlock()
}

// Kill is best-effort: each cleanup step runs independently and a failure in
// one (e.g. a broken git worktree) only logs a warning rather than aborting
// the rest. The in-memory pointers are cleared regardless so the daemon
// caller can always proceed to remove the persisted record. See issue #478.
func (b *LocalBackend) Kill(i *Instance) error {
	// PR 2 of #930 gives an instance N tabs (agent + shell today), so Kill tears
	// down each tab's session, not just the agent's. The kill mode kill-sessions
	// every tab (waiting for each pane to exit before the worktree delete, #802),
	// deletes the worktree, and clears the refs — see teardownTabs. Best-effort:
	// a stuck tmux or a failed worktree cleanup only logs so the caller can still
	// drop the record (#478/#802). Returns nil regardless.
	return i.teardownTabs(teardownKill{})
}

// CloseAttachOnly releases this instance's hold on its tmux sessions — the
// attach PTYs and the `tmux attach-session` child processes — WITHOUT running
// `tmux kill-session`. The server-side tmux sessions and the git worktree
// behind them are left untouched. The daemon uses this to discard a duplicate
// Instance built from disk that turned out to already be tracked in memory
// (#867): the duplicate must surrender the PTYs it opened during restore
// without tearing down the live sessions the canonical Instance shares.
func (b *LocalBackend) CloseAttachOnly(i *Instance) error {
	// Since #930 an instance holds N tabs (agent + shell/process), and restoring
	// the duplicate opened an attach PTY for EVERY tab (Start restores the agent
	// tab, setupTabs the rest), so releasing only the agent tab's PTY leaked one
	// fd per extra tab each time the daemon discarded a duplicate — eventually
	// EMFILE in the long-running daemon (#1065). The release-PTY mode drops every
	// tab's attach without kill-session and leaves the worktree intact — see
	// teardownTabs — returning the joined per-tab close errors.
	return i.teardownTabs(teardownReleasePTY{})
}

func (b *LocalBackend) Preview(i *Instance) (string, error) {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxLocked()
	i.mu.RUnlock()

	if !s || ts == nil {
		return "", nil
	}
	return ts.CapturePaneContent()
}

// PreviewContext is Preview bound to ctx: the pane capture is cancellable, so a
// cancelled readiness wait tears down the in-flight `tmux capture-pane` subprocess
// instead of letting it run to completion (task.WaitForReady's capturePreview).
func (b *LocalBackend) PreviewContext(ctx context.Context, i *Instance) (string, error) {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxLocked()
	i.mu.RUnlock()

	if !s || ts == nil {
		return "", nil
	}
	return ts.CapturePaneContentContext(ctx)
}

func (b *LocalBackend) PreviewFullHistory(i *Instance) (string, error) {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxLocked()
	i.mu.RUnlock()

	if !s || ts == nil {
		return "", nil
	}
	return ts.CapturePaneContentWithOptions("-", "-")
}

func (b *LocalBackend) HasUpdated(i *Instance) (updated bool, hasPrompt bool, content string) {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxLocked()
	i.mu.RUnlock()

	if !s || ts == nil {
		return false, false, ""
	}
	return ts.HasUpdated()
}

func (b *LocalBackend) SendPromptCommand(i *Instance, prompt string) error {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxLocked()
	i.mu.RUnlock()

	if !s {
		return fmt.Errorf("instance not started")
	}
	if ts == nil {
		return fmt.Errorf("tmux session not initialized")
	}
	return ts.SendKeysCommand(prompt)
}

func (b *LocalBackend) IsAlive(i *Instance) bool {
	i.mu.RLock()
	ts := i.tmuxLocked()
	i.mu.RUnlock()

	if ts == nil {
		return false
	}
	return ts.DoesSessionExist()
}

func (b *LocalBackend) CheckAndHandleTrustPrompt(i *Instance) bool {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxLocked()
	i.mu.RUnlock()

	if !s || ts == nil {
		return false
	}
	// Dispatch on the agent the pane actually runs (ResolvedAgent) so a
	// program_overrides entry pointing an agent name at a non-agent binary
	// never gets an agent's trust-prompt handling (#1116/#1131 defect class),
	// while restored sessions with legacy free-form Program values (e.g.
	// "/home/foo/bin/claude") still get it — same persisted-state class of
	// regression as #677. Codex was added in #729: it was previously excluded
	// here, so a codex trust/confirmation dialog was never dismissed even
	// though isReadyContent could surface it.
	// opencode has NO trust gate of its own (a fresh `git init` repo goes straight
	// to its composer, verified on 0.0.0-main-202604230742), so it lands in
	// CheckAndHandleTrustPrompt's generic doc-trust branch and no-ops harmlessly.
	// It is listed anyway: this is the one site enumerating every agent, and an
	// agent silently missing from it is the #729 defect (codex was excluded here,
	// so its dialog was surfaced by isReadyContent but never dismissed).
	switch i.ResolvedAgent() {
	case tmux.ProgramClaude, tmux.ProgramCodex, tmux.ProgramAider, tmux.ProgramGemini, tmux.ProgramAmp, tmux.ProgramOpencode:
		return ts.CheckAndHandleTrustPrompt()
	}
	return false
}

func (b *LocalBackend) TapEnter(i *Instance) {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxLocked()
	autoYes := i.AutoYes
	i.mu.RUnlock()

	if !s || !autoYes || ts == nil {
		return
	}
	if err := ts.TapEnter(); err != nil {
		log.ErrorLog.Printf("error tapping enter: %v", err)
	}
}
