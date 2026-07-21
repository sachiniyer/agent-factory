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

	// Create a new detached tmux session and start claude in it. The -e
	// markers (when supported) let `af doctor` trace any process the pane
	// spawns back to this session even after it is orphaned (#1104).
	args := []string{"new-session", "-d", "-s", t.sanitizedName, "-c", workDir}
	args = append(args, sessionEnvFlags(t.sanitizedName)...)
	args = append(args, t.programCmd())
	cmd, systemdScoped := newTmuxServerCommand(args...)

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
				go reapLeakedProcesses(t.sanitizedName, leaked, reapGraceWait, reapTermWait)
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
	t.promptMu.Lock()
	defer t.promptMu.Unlock()

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

// DocTrustPromptPresent reports whether content shows the documentation-link
// trust dialog shared by aider/gemini:
//
//	Open documentation url for more info? (Y)es/(N)o/(D)on't ask again [Yes]:
//
// BOTH the full prose question and the "(D)on't ask again" affordance are
// required, and NEITHER is redundant:
//
//   - The affordance is not a discriminator. aider renders "(D)on't ask again"
//     on EVERY confirmation it asks ("Add src/main.go to the chat?", …). It only
//     tells us some prompt is up — not which one. It is the weaker anchor.
//   - The prose is the discriminator, and only at FULL length. Do not shorten it
//     to a prefix like "Open documentation url": the prefix plus an affordance
//     that is nearly always present means an unrelated confirmation gets answered
//     'D' whenever that string is anywhere on screen — including in a source file
//     the agent has open. This repo is self-hosted, so that is not hypothetical.
//
// The full prose is also the ORIGINAL observed string (2b23c52); the shorter form
// was a later paraphrase introduced for readiness detection (7d78ee6), not an
// observation of the dialog. Match what the dialog actually renders.
//
// ACTION HERE REQUIRES POSITIVE EVIDENCE, because the two ways to be wrong do not
// cost the same:
//
//   - A MISS costs one keypress. The dialog stays up, the user presses D — or the
//     next tick of the daemon's 1-second poll catches it, since this re-runs
//     continuously and the real dialog does not go away on its own.
//   - A FALSE POSITIVE types 'D'+Enter into a RUNNING agent that never asked
//     anything (TapDAndEnter, via CheckAndHandleTrustPrompt above). That is
//     unbidden input into someone's live session: it can answer a different
//     question than the one we think is on screen, or land in an agent's prompt
//     box and modify their files. The poll's caller discards the bool, so nothing
//     breaks the loop — it re-fires every tick the phrase stays on screen (#1952).
//
// So this is deliberately conservative BY CONSTRUCTION, and the asymmetry above is
// the reason. When the match is ambiguous the answer is DO NOTHING. If you are here
// to "loosen this so it catches more cases": that trade buys back a keypress the
// user can supply themselves, and pays for it in keystrokes injected into live
// agents. Prefer adding a NEW anchored predicate for a specific dialog you can
// identify over widening this one.
//
// Before changing this predicate, answer: what does the new one ADMIT that the old
// one did not? Not what it rejects — what it lets through. Adding a conjunct while
// quietly weakening another term reads as tightening and is not; that is how the
// #1952 fix itself first shipped a NEW false-positive path. The tests only exercise
// the real dialogs, so they will not catch it for you.
//
// The match runs against a visible-only capture (CapturePaneContent), so content
// is whatever is on screen right now — including the agent's own output, a log
// line, or a file it printed. Requiring a marker only the real dialog renders is
// what keeps ordinary output from being mistaken for a question.
//
// This is the single copy, shared with task's readiness check (task/runner.go
// isReadyContent) so the dismissal and the readiness signal can never drift apart
// again — the drift between this and the Claude branch beside it is exactly how
// #1952 happened. It lives here because task already imports session/tmux; the
// reverse edge would be an import cycle.
func DocTrustPromptPresent(content string) bool {
	return strings.Contains(content, "Open documentation url for more info") &&
		strings.Contains(content, "(D)on't ask again")
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
