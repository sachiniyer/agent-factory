package tmux

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// Start creates and starts a new tmux session, then attaches to it. Program is the command to run in
// the session (ex. claude). workdir is the git worktree directory.
func (t *TmuxSession) Start(workDir string) error {
	// A fresh attempt supersedes any earlier proof: whatever an aborted Start
	// established about this name, it is about to be re-established or replaced.
	t.setProvenNoPane(false)
	// Check if the session already exists. This is a POSITIVE existence gate, so
	// it must not read the lossy bool: a wedged/timed-out has-session is NOT proof
	// the name is taken, and ExistsOrUnknown would launder it into "already
	// exists" — the exact misread #1962 fixes. Surface the timeout instead so the
	// caller learns the server never answered rather than that the name collided.
	exists, known := t.ProbeSession()
	if !known {
		return fmt.Errorf("%w: has-session probe for session %q did not answer", ErrTmuxTimeout, t.sanitizedName)
	}
	if exists {
		return fmt.Errorf("%w: tmux session already exists: %s", ErrSessionNotStarted, t.sanitizedName)
	}
	// The name is positively absent, so any Start from here creates a new pane
	// process. Drop diagnostics owned by the prior process at that proven runtime
	// boundary. SetProgram cannot do this: the live-session Restore path rewrites
	// the command string without re-execing the existing pane.
	t.resetCodexSafetyState()

	// Create a new detached tmux session and start claude in it. The -e
	// markers (when supported) let `af doctor` trace any process the pane
	// spawns back to this session even after it is orphaned (#1104).
	program := t.programCmd()
	wrappedProgram, launchEnv, importNames, envErr := t.launchEnvironment(program)
	if envErr != nil {
		// Nothing has run new-session, so if the name is DETERMINATELY absent no pane
		// can exist behind it and a teardown need not gate on liveness (#2985).
		t.proveNoPaneIfDeterminatelyAbsent()
		return fmt.Errorf("%w: prepare filtered session environment: %v", ErrSessionNotStarted, envErr)
	}
	args := []string{"new-session", "-d", "-s", t.sanitizedName, "-c", workDir}
	args = append(args, sessionEnvFlags(t.sanitizedName)...)
	args = append(args, wrappedProgram)
	args, envErr = t.importClientEnvironmentArgs(args, importNames)
	if envErr != nil {
		// Same proof as above: still read-only, still before new-session.
		t.proveNoPaneIfDeterminatelyAbsent()
		return fmt.Errorf("%w: prepare existing tmux session environment: %v", ErrSessionNotStarted, envErr)
	}
	cmd, systemdScoped := newTmuxServerCommand(args...)
	// A fresh tmux server snapshots its first client's environment. Filter the
	// client as well as the pane so a server created here never stores unrelated
	// credentials in its global environment. The pane exec shim filters again
	// for an already-running server whose global environment predates this fix.
	cmd.Env = append([]string(nil), launchEnv...)

	ptmx, commandDone, err := startPtyTracked(t.ptyFactory, cmd)
	if err != nil {
		if systemdScoped {
			err = fmt.Errorf("systemd-run --user --scope could not start: %w", err)
		}
		// A failed PtyFactory.Start means the process did not begin, which is why
		// this path may return ErrSessionNotStarted. Keep the historical defensive
		// cleanup for injected factories or platform edges that expose a partial
		// session. ExistsOrUnknown is safe here: the only action gated on true is a
		// bounded best-effort kill-session, never a false liveness claim.
		if t.ExistsOrUnknown() {
			leaked := SessionProcessTrees(t.cmdExec, t.sanitizedName)
			// Bound the cleanup kill-session (#2028): on bare exec.Command it could
			// hang forever on a wedged tmux server, and Start is on the daemon's
			// create/launch path, so an unbounded stall here wedges that handler. Route
			// it through the same bounded runner as the rest of the package's tmux
			// commands (#1917) — a tripped deadline degrades to a best-effort cleanup
			// failure, the same as any other kill-session error below.
			ctx, cancel := tmuxTimeoutContext()
			cleanupErr := t.runTmuxBounded(ctx, "kill-session", "-t", exactTarget(t.sanitizedName))
			cancel()
			if cleanupErr != nil {
				err = fmt.Errorf("%v (cleanup error: %v)", err, cleanupErr)
			} else if len(leaked) > 0 {
				// Start failed and we are tearing the partial session down ourselves;
				// its processes are going with it by design, not escaping (#2765).
				go reapSessionProcesses(reapOnRequest, t.sanitizedName, leaked, reapGraceWait, reapTermWait)
			}
		}
		return fmt.Errorf("%w: error starting tmux session: %w", ErrSessionNotStarted, err)
	}

	// Poll for session existence with exponential backoff. Break only on a probe
	// that ANSWERED "exists" (known && exists): reading the lossy bool here let a
	// mid-poll wedge exit the loop as if the session had come up, so Start reported
	// success for a session tmux never confirmed (#1962). A !known probe means keep
	// waiting until the 2s deadline, then take the timeout path below — which
	// threads pane-state / ErrTmuxTimeout correctly.
	timeout := time.After(2 * time.Second)
	sleepDuration := 5 * time.Millisecond
	for {
		if exists, known := t.ProbeSession(); known && exists {
			break
		}
		select {
		case commandErr := <-commandDone:
			// A nil exit only means the detached new-session command completed;
			// tmux may need another poll before the session becomes visible. A
			// non-zero systemd-run exit, however, is the actionable cause. Before
			// #2176 that status was discarded by Pty's reaper and the operator saw
			// only a misleading tmux readiness timeout.
			commandDone = nil
			if commandErr != nil {
				_ = ptmx.Close()
				var launchErr error
				if systemdScoped {
					launchErr = fmt.Errorf("error starting tmux session: systemd-run --user --scope failed to create session %q: %w", t.sanitizedName, commandErr)
				} else {
					launchErr = fmt.Errorf("error starting tmux session: tmux new-session for %q failed: %w", t.sanitizedName, commandErr)
				}

				// The launch process began. Its exit status, even paired with later
				// name absence, cannot prove a pane never ran and is not still flushing
				// after tmux removed its session. Keep every answered post-spawn failure
				// outside ErrSessionNotStarted so LocalBackend preserves the worktree.
				return launchErr
			}
		case <-timeout:
			ptmx.Close()
			// The pane program exiting instantly (bad path, rejected flag)
			// takes the whole tmux session down before this existence poll
			// ever sees it — name the likely cause and the exact command so
			// the user isn't left with a bare timeout (#1116, #1131).
			timeoutErr := fmt.Errorf("timed out waiting for tmux session %s; the pane program may have exited immediately after launch — check that it runs and accepts its flags (program: %q)", t.sanitizedName, t.programCmd())
			if systemdScoped {
				timeoutErr = fmt.Errorf("systemd-run --user --scope completed but the tmux session did not appear: %w", timeoutErr)
			}
			// The timeout classification is load-bearing: Launch uses it for
			// diagnostics, and older callers may still distinguish an unknown tmux
			// outcome from a clean pre-spawn failure. %w, never %v, so the sentinel
			// survives every wrapping layer.
			// A timeout may happen after tmux created a pane but before has-session
			// observed it. Teardown alone is not cleanup proof: kill-session returns
			// before the pane process finishes flushing. Wait for that process here,
			// and keep the outcome unknown if it outlives the bound, so LocalBackend
			// cannot remove the fresh worktree underneath its final writes.
			// No occupancy scan here, deliberately (#2998). This point sits ABOVE
			// gw.Cleanup(), which is what cancels a still-running
			// post_worktree_command — so a scan here matches AF's own provisioning
			// hook, whose cmd.Dir is this worktree. It also cannot see whether the
			// worktree is EXTERNAL: for an `--here` launch this is the user's own
			// checkout, which cleanup never removes and where the invoking shell
			// normally sits. Both produce refusals that block a legitimate cleanup.
			// The check belongs below, where ownership is known and the hooks are
			// already cancelled; recorded as a residual on #2998.
			cleanupState, cleanupErr := t.CloseAndWaitForPaneExit()
			if cleanupErr != nil {
				timeoutErr = fmt.Errorf("%v (cleanup error: %v)", timeoutErr, cleanupErr)
			}
			// A successful close+pane-exit confirmation on a positive-policy name
			// establishes that the exact launch identity is gone. The old blanket
			// timeout classification was necessary only while a fresh title could
			// contain spellings tmux rewrote (#2207); legacy exact names still stay
			// on that conservative path through hasStableTmuxSpelling.
			if cleanupState == PaneStateKnown && cleanupErr == nil && hasStableTmuxSpelling(t.sanitizedName) {
				return fmt.Errorf("%w: %w", timeoutErr, ErrSessionNotStarted)
			}
			return fmt.Errorf("%w: %w", timeoutErr, ErrTmuxTimeout)
		default:
			time.Sleep(sleepDuration)
			// Exponential backoff up to 50ms max
			if sleepDuration < 50*time.Millisecond {
				sleepDuration *= 2
			}
		}
	}
	ptmx.Close()

	// Set history limit to enable scrollback (default is 2000, we'll use 10000 for
	// more history). Bounded like every other tmux command in this package (#1917/
	// #2028): these run on the daemon's create path, so a wedged server must not
	// hang them; both are best-effort and only log on failure.
	ctx, cancel := tmuxTimeoutContext()
	if err := t.runTmuxBounded(ctx, "set-option", "-t", exactTarget(t.sanitizedName), "history-limit", "10000"); err != nil {
		// Logged at INFO with no "Warning:" text (#2166): the level is the
		// severity signal, and an embedded "Warning:" makes an INFO line trip a
		// log scraper that greps for the word.
		log.InfoLog.Printf("failed to set history-limit for session %s (scrollback stays at the tmux default): %v", t.sanitizedName, err)
	}
	cancel()

	// Enable mouse scrolling for the session
	ctx, cancel = tmuxTimeoutContext()
	if err := t.runTmuxBounded(ctx, "set-option", "-t", exactTarget(t.sanitizedName), "mouse", "on"); err != nil {
		log.InfoLog.Printf("failed to enable mouse scrolling for session %s: %v", t.sanitizedName, err)
	}
	cancel()

	// Attach to the session we just created. Pass empty workDir so a missing
	// session here surfaces as an error rather than recursively re-spawning.
	err = t.Restore("")
	if err != nil {
		// Probe BEFORE Close (which kills the session): the existence poll
		// above saw the session, so if it is gone again by attach time the
		// pane program exited within milliseconds of launch. Say so instead
		// of the misleading "session does not exist" (#1116, #1131).
		//
		// !ExistsOrUnknown is the definitively-absent branch: a wedged→"exists"
		// only means we fall through to the generic "error restoring" message
		// instead of the more specific "vanished" one — never a false "vanished"
		// claim against a merely-slow server. This only picks the error wording;
		// no destructive action is gated on it (#1962).
		vanished := !t.ExistsOrUnknown()
		// Preserve the teardown's unknown classification for callers even though
		// Launch now independently fails closed on every post-spawn Start error.
		state, cleanupErr := t.Close()
		if cleanupErr != nil {
			err = fmt.Errorf("%v (cleanup error: %v)", err, cleanupErr)
		}
		if state != PaneStateKnown {
			err = fmt.Errorf("%w: %w", err, ErrTmuxTimeout)
		}
		if vanished {
			return fmt.Errorf("tmux session %s vanished before attach; the pane program likely exited immediately after launch — check that it runs and accepts its flags (program: %q): %w", t.sanitizedName, t.programCmd(), err)
		}
		return fmt.Errorf("error restoring tmux session: %w", err)
	}

	return nil
}

// CheckAndHandleTrustPrompt checks the pane content once for a trust prompt and dismisses it if found.
// Returns true if the prompt was found and handled.
func (t *TmuxSession) CheckAndHandleTrustPrompt() bool {
	t.inputMu.Lock()
	defer t.inputMu.Unlock()

	content, err := t.CapturePaneContent()
	if err != nil {
		return false
	}

	// Key off the agent actually running in the pane, token-matched — a loose
	// substring check would route e.g. /opt/claude-wrapper/run through the
	// claude branch (#1116 defect class).
	switch DetectAgentFromCommand(t.programCmd()) {
	case ProgramClaude:
		if claudeTrustPromptPresent(content) {
			if err := t.TapEnter(); err != nil {
				log.ErrorLog.Printf("could not tap enter on trust/MCP screen: %v", err)
				return false
			}
			return true
		}
	case ProgramCodex:
		if t.handleCodexSafetyBuffering(content) {
			return true
		}
		if CodexTrustPromptPresent(content) {
			if err := t.TapEnter(); err != nil {
				log.ErrorLog.Printf("could not tap enter on Codex directory-trust screen: %v", err)
				return false
			}
			return true
		}
	default:
		if DocTrustPromptPresent(content) {
			if err := t.TapDAndEnter(); err != nil {
				log.ErrorLog.Printf("could not tap enter on trust screen: %v", err)
				return false
			}
			return true
		}
	}
	return false
}

// CodexTrustPromptPresent reports whether content shows Codex's directory-trust
// modal introduced by 0.144.6:
//
//	Do you trust the contents of this directory?
//	› 1. Yes, continue
//	  2. No, quit
//	Press enter to continue
//
// This modal is on the config-agent delivery path before the real composer.
// Its selected option uses the SAME `›` glyph as Codex's composer, so the old
// readiness check treated it as ready, pasted the briefing into the modal, and
// used the trailing Enter to select Yes. Every tmux command succeeded while
// Codex recorded no user turn, leaving an empty composer by attach time (#2220).
//
// The match is anchored on the question, both option labels and the affordance.
// CheckAndHandleTrustPrompt also runs on live-session polls, so a prose mention
// of one phrase must never inject Enter into a working agent.
var ansiCSISequence = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

func CodexTrustPromptPresent(content string) bool {
	content = ansiCSISequence.ReplaceAllString(strings.ReplaceAll(content, "\r\n", "\n"), "")
	question, selectedYes, noOption, affordance, last := -1, -1, -1, -1, -1
	for idx, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" {
			continue
		}
		last = idx
		switch {
		case strings.HasPrefix(line, "Do you trust the contents of this directory?"):
			question = idx
		case line == "› 1. Yes, continue":
			selectedYes = idx
		case line == "2. No, quit":
			noOption = idx
		case line == "Press enter to continue":
			affordance = idx
		}
	}
	return question >= 0 && question < selectedYes && selectedYes < noOption &&
		noOption < affordance && affordance == last
}

// DocTrustPromptPresent reports whether content ends at the documentation-link
// confirmation prompt shared by aider/gemini:
//
//	<reverse-video>https://aider.chat/docs/...
//	<question text> (Y)es/(N)o/(D)on't ask again [Yes]:
//
// aider's InputOutput.confirm_ask accepts caller-controlled question text, then
// appends the confirmation suffix above. InputOutput.offer_url separately renders
// its URL subject in reverse video before asking. Match that renderer-owned SGR
// prefix immediately before the final prompt line. Do not key daemon input on the
// overridable prose. In particular, this source file contains the old prose and
// affordance together; a strings.Contains matcher lets repo-controlled text inject
// D+Enter (#2638).
//
// The reverse-video URL subject and final-line requirements both matter. The
// affordance appears on unrelated aider confirmations, while an ordinary URL can
// appear immediately before one. Without the renderer prefix those two unrelated
// lines compose a false positive. A no-color or otherwise ambiguous capture is not
// proof of a prompt and stays false.
//
// This is deliberately conservative because the two errors are asymmetric. A miss
// costs one manual keypress, and the real dialog remains visible for the next poll
// to retry. A false positive types D+Enter into a live agent that asked nothing; it
// can answer a different question or modify the agent's input, and the daemon's
// continuous poll repeats it while the text remains visible (#1952).
//
// This is the single copy, shared with task's readiness check (task/runner.go
// isReadyContent) so the dismissal and the readiness signal can never drift apart
// again — the drift between this and the Claude branch beside it is exactly how
// #1952 happened. It lives here because task already imports session/tmux; the
// reverse edge would be an import cycle.
func DocTrustPromptPresent(content string) bool {
	const confirmPrefix = "(Y)es/(N)o/(D)on't ask again [Yes]:"
	content = strings.ReplaceAll(content, "\r\n", "\n")
	lines := strings.Split(content, "\n")
	last := len(lines) - 1
	for last >= 0 && strings.TrimSpace(ansiCSISequence.ReplaceAllString(lines[last], "")) == "" {
		last--
	}
	if last < 1 || !strings.HasSuffix(
		strings.TrimSpace(ansiCSISequence.ReplaceAllString(lines[last], "")), confirmPrefix,
	) {
		return false
	}
	return reverseVideoURLSubject(lines[last-1])
}

func reverseVideoURLSubject(line string) bool {
	reverse := false
	remaining := line
	for strings.HasPrefix(remaining, "\x1b[") {
		end := strings.IndexByte(remaining, 'm')
		if end < 2 {
			return false
		}
		for _, code := range strings.Split(remaining[2:end], ";") {
			switch code {
			case "", "0", "27":
				reverse = false
			case "7":
				reverse = true
			}
		}
		remaining = remaining[end+1:]
	}
	visible := strings.TrimSpace(ansiCSISequence.ReplaceAllString(remaining, ""))
	return reverse && !strings.ContainsAny(visible, " \t") &&
		(strings.HasPrefix(visible, "https://") || strings.HasPrefix(visible, "http://"))
}

// claudeTrustPromptPresent reports whether the captured pane content is showing
// one of Claude Code's launch-time gates that af auto-dismisses with Enter: the
// folder-trust prompt (either wording) or the "new MCP server" trust prompt.
//
// This runs on the daemon's CONTINUOUS Snapshot poll (session/agentserver_local.go
// CheckAndHandleTrustPrompt), and CapturePaneContent is visible-only
// (capture-pane -p, no -S), so `content` is whatever is on screen right now —
// including ordinary agent output. A bare substring match on natural-language
// prompt text would therefore risk firing a spurious Enter on unrelated output.
//
// Claude Code reworded the folder-trust gate from the interrogative
// "Do you trust the files in this folder?" to a "Quick safety check: Is this a
// project you created or one you trust? ... ❯ 1. Yes, I trust this folder /
// Enter to confirm · Esc to cancel" dialog. af's dismissal missed the new
// wording, so brand-new sessions hung at the prompt and rendered a blank pane.
//
// We match the reworded prompt ANCHORED: the natural-language question must
// co-occur with a dialog-chrome marker only the real modal renders ("Yes, I
// trust this folder" or the "Enter to confirm" affordance), so a stray mention
// of the phrase in scrollback or agent output never triggers a dismissal. The
// old wording is a self-contained, dialog-specific string and stays matched
// as-is. The MCP prompt ("New MCP server found. Do you trust this new MCP
// server? ❯ 1. Yes ... Enter to confirm") is anchored on its UNIQUE question
// "do you trust this new mcp server" — a phrase Claude only ever renders inside
// the real MCP trust modal, never in ordinary output. We deliberately do NOT
// anchor on a generic marker like "Enter to confirm": that affordance appears
// in many dialogs, so pairing it with a bare "new mcp server" mention would
// still false-match on normal agent output. Each anchor here is a string that
// only its own dialog emits, closing the whole false-positive class.
func claudeTrustPromptPresent(content string) bool {
	lower := strings.ToLower(content)

	// Reworded folder-trust dialog — the question co-occurs with the
	// dialog-only option label. Both strings are specific to this modal.
	reworded := strings.Contains(content, "Is this a project you created or one you trust") &&
		strings.Contains(content, "Yes, I trust this folder")

	// MCP trust dialog — anchored on its unique question (case-insensitive).
	mcpDialog := strings.Contains(lower, "do you trust this new mcp server")

	return reworded ||
		mcpDialog ||
		strings.Contains(content, "Do you trust the files in this folder?")
}

// Restore attaches to an existing tmux session. If the session is missing
// (e.g. the tmux server died after a machine reboot, see #386) and workDir is
// non-empty, a fresh session is spawned in workDir using the same program so
// persisted instances can resume across reboots. If the session is missing
// and workDir is empty, the missing-session condition is surfaced as an error;
// real failures (PTY open errors, Start failures such as missing binaries or
// vanished worktrees) are always surfaced.
//
// When re-spawning, the program string is rewritten via resumeProgram so
// agents that expose a "resume the most recent session in cwd" flag pick
// the prior conversation back up instead of starting fresh (#595). Agents
// without such a flag, or programs that already include one, are left
// untouched.
func (t *TmuxSession) Restore(workDir string) error {
	// !ExistsOrUnknown is the definitively-absent branch (#1962): only a session
	// tmux CONFIRMED gone triggers the re-spawn. A wedged→"exists" falls through
	// to the pure rebind below, which is the safe direction — re-spawning against
	// a server that is merely wedged around a still-live session would create a
	// duplicate. The #386 respawn design has always fired only on definitive
	// absence, and this preserves it.
	if !t.ExistsOrUnknown() {
		if workDir == "" {
			return fmt.Errorf("tmux session %q does not exist", t.sanitizedName)
		}
		log.InfoLog.Printf("tmux session %q missing on Restore; re-spawning in %s", t.sanitizedName, workDir)
		t.setProgramCmd(resumeProgram(t.programCmd()))
		return t.Start(workDir)
	}

	// The session is live. Restore is now a pure logical rebind (#1592 Phase 2
	// PR7): it opens NO `tmux attach-session` render client — the local runtime's
	// data plane is the daemon's clientless agent-server (pipe-pane → WS broker,
	// PR5/6), and interactive full-screen attach is a WS subscriber in the client
	// (apiclient.AttachStream). All Restore still owes is a fresh status monitor,
	// swapped under monitorMu because the daemon poll may be inside HasUpdated()
	// reading the old pointer and mutating its fields right now (#1528).
	t.setMonitor(newStatusMonitor())
	return nil
}
