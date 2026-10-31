package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
	"github.com/sachiniyer/agent-factory/log"
)

// socketFSProbe is the test seam over the shared bounded filesystem probe on
// the tmux launch path. The fsprobe implementation answers with the
// filesystem's own errno from THIS process — the verdict a tmux server's
// internal failure loses into its output text.
var socketFSProbe = fsprobe.Write

// statSocketDir is the test seam over fsprobe.Stat — the bounded metadata
// lookup that decides whether the uid socket root exists. It must be
// bounded: a launch failure reaches this lookup while the daemon's
// synchronous restore poll holds the instance, and an unbounded os.Stat on
// an existing-but-wedged bind/NFS mount would block Start forever instead
// of entering environmental backoff (#5295 review).
var statSocketDir = fsprobe.Stat

// tmuxSocketFilesystemDir returns the directory whose filesystem must accept
// tmux's own server-state writes — the socket file and the tmux-<uid>
// directory the server creates it under — for a launch from THIS process's
// environment. It mirrors the socket-identity resolution the dedicated-server
// scope names itself after: $TMUX names a live socket path (probe the
// directory that hosts it), otherwise the default socket resolves under
// ${TMUX_TMPDIR:-/tmp}/tmux-<uid> — probe the base, because the uid dir may
// not exist yet while its parent must be writable for tmux to create it.
// The returned error carries a failed uid-dir stat as evidence; the dir is
// still the right probe target, so callers join the error and probe anyway.
func tmuxSocketFilesystemDir() (string, error) {
	if identity := os.Getenv("TMUX"); identity != "" {
		if socket, _, ok := strings.Cut(identity, ","); ok && socket != "" {
			return filepath.Dir(socket), nil
		}
	}
	base := os.Getenv("TMUX_TMPDIR")
	if base == "" {
		base = "/tmp"
	}
	// The server creates its socket under base/tmux-<uid>, not base itself —
	// and an EXISTING uid dir is the filesystem the socket write actually
	// lands on. If that child is a bind mount or its own (full) filesystem,
	// probing the healthy parent would answer the wrong filesystem's
	// question while tmux's failure stays an opaque ExitError (#5295
	// review). Fall back to base only when the uid dir does not exist yet:
	// then base is where tmux will create it. A stat that FAILS is not a
	// fallback to base — the uid dir stays the probe target so the write
	// probe's own bounded verdict lands for the filesystem tmux actually
	// uses, and the stat error returns as joined evidence.
	uidDir := filepath.Join(base, fmt.Sprintf("tmux-%d", os.Getuid()))
	st, err := statSocketDir(uidDir)
	switch {
	case err == nil && st != nil && st.IsDir():
		return uidDir, nil
	case err != nil && !os.IsNotExist(err):
		return uidDir, err
	default:
		return base, nil
	}
}

// Start creates and starts a new tmux session, then attaches to it. Program is the command to run in
// the session (ex. claude). workdir is the git worktree directory.
func (t *TmuxSession) Start(workDir string) error {
	// A Start that fails AFTER its prepare step minted launch capabilities has
	// nothing for them to belong to. Counting this session's mints tells the two
	// apart: a gate refusal before prepare (the name already exists) leaves the
	// live session's capability alone.
	minted := t.capabilityMints.Load()
	err := t.start(workDir)
	if err != nil && t.capabilityMints.Load() != minted {
		revokeLaunchCapabilities(t.sanitizedName)
	}
	return err
}

func (t *TmuxSession) start(workDir string) error {
	// A fresh attempt supersedes any earlier proof: whatever an aborted Start
	// established about this name, it is about to be re-established or replaced.
	t.setProvenNoPane(false)
	t.setClosedConclusively(false)
	// The teardown mark is NOT a proof and is not superseded by the attempt: on a
	// same-object restart (agent swap) it describes the session af just closed,
	// which is still what the status monitor would report gone if this Start
	// fails before a replacement exists. It clears below only on tmux's answer
	// that a session is live behind the name (Codex on #4473).
	//
	// Check if the session already exists. This is a POSITIVE existence gate, so
	// it must not read the lossy bool: a wedged/timed-out has-session is NOT proof
	// the name is taken, and ExistsOrUnknown would launder it into "already
	// exists" — the exact misread #1962 fixes. Surface the timeout instead so the
	// caller learns the server never answered rather than that the name collided.
	exists, known := t.ProbeSession()
	if !known {
		// The pre-spawn gate's non-answer is environmental evidence in its
		// own right: a wedged or filesystem-stuck server cannot answer
		// has-session, and every retry would charge the structural budget
		// for a condition that resolves on its own. Pair a distinct
		// sentinel with the deadline so the classifier's
		// deadline+sentinel arm reads the bounded environmental class —
		// NOT every ErrTmuxTimeout, since the later readiness timeout can
		// also mean the pane command exited instantly (#5295 review).
		return fmt.Errorf("%w: has-session probe for session %q did not answer: %w: %w",
			ErrTmuxTimeout, t.sanitizedName, ErrTmuxServerNoAnswer, context.DeadlineExceeded)
	}
	if exists {
		// A live session af did not just create holds the name, so no teardown
		// request describes it — but only when the live session IS the
		// generation the current monitor polls. An old monitor bound to a
		// prior generation keeps its mark while a replacement owns the name:
		// its poll must still read af's own teardown of that generation as
		// expected (Codex on #4473).
		t.clearTeardownMarkForConfirmedGeneration()
		return fmt.Errorf("%w: %w: %s", ErrSessionNotStarted, ErrSessionNameTaken, t.sanitizedName)
	}
	// Refuse to spawn into a directory that is not there: `new-session -c`
	// silently falls back to the tmux server's current directory when the path
	// is missing, so without this check a respawn of a deleted worktree lands
	// the agent in an unrelated checkout or the daemon's $HOME (#5172). This is
	// the one seam every af spawn — create, restore, respawn, tab — passes
	// through. The name is already proved absent above and nothing has run
	// new-session, so a refusal here can leave no pane behind.
	//
	// The stat must be bounded, not a raw os.Stat: workDir is not confined to
	// af's home — sibling worktrees live beside the user's repo, `--here`
	// launches in the user's own checkout, and restore re-uses persisted paths
	// verbatim, so the path can sit on an existing-but-wedged bind/NFS/FUSE
	// mount whose stat blocks in an uninterruptible D state. An unbounded
	// stat here would wedge the daemon's synchronous create handler (and any
	// lock held under it) forever instead of entering environmental backoff
	// (#5295 review). statSocketDir runs the lookup inside fsprobe.Stat's 2s
	// flight and returns the typed ErrProbeTimedOut + context.DeadlineExceeded
	// pair on non-answer, which the lost-restore environmental classifier
	// reads as the bounded environmental class — the same discipline
	// tmuxSocketFilesystemDir and the worktree-add probe already keep.
	if workDir != "" {
		st, err := statSocketDir(workDir)
		if err != nil || st == nil || !st.IsDir() {
			// Not just "missing": an unreadable or un-stat-able directory is
			// equally unverifiable, and either verdict refuses the spawn. A
			// bounded non-answer (ErrProbeTimedOut: context.DeadlineExceeded)
			// is joined into the chain so the lost-restore classifier routes
			// the session to environmental backoff instead of hanging.
			if err == nil {
				err = fmt.Errorf("not a directory")
			}
			t.proveNoPaneIfDeterminatelyAbsent()
			return fmt.Errorf("%w: %w: session %q cannot start in %s: %w",
				ErrSessionNotStarted, ErrSpawnDirMissing, t.sanitizedName, workDir, err)
		}
	}
	// The name is positively absent, so any Start from here creates a new pane
	// process. Drop diagnostics owned by the prior process at that proven runtime
	// boundary. SetProgram cannot do this: the live-session Restore path rewrites
	// the command string without re-execing the existing pane.
	t.resetCodexSafetyState()
	t.resetCodexUpdateState()
	// Same proven boundary: a key af sent to the PREVIOUS pane process cannot
	// explain anything the new one does (#3579).
	t.resetDialogKeystroke()
	t.resetClaudeTrustState()

	// Create a new detached tmux session and start claude in it. The -e
	// markers (when supported) let `af doctor` trace any process the pane
	// spawns back to this session even after it is orphaned (#1104).
	// No separate program read here: prepareLaunchEnvironment snapshots the command and
	// its generated-args declaration together, so the two cannot be torn apart by a
	// concurrent rewrite (#3083 review).
	wrappedProgram, launchEnv, importNames, sessionEnv, defaultCommand, envErr := t.prepareLaunchEnvironment(workDir)
	if envErr != nil {
		// Nothing has run new-session, so if the name is DETERMINATELY absent no pane
		// can exist behind it and a teardown need not gate on liveness (#2985).
		t.proveNoPaneIfDeterminatelyAbsent()
		return fmt.Errorf("%w: prepare filtered session environment: %w", ErrSessionNotStarted, envErr)
	}
	args := []string{"new-session", "-d", "-s", t.sanitizedName, "-c", workDir}
	args = append(args, sessionEnvFlags(t.sanitizedName, newSessionGeneration(), t.launchInstanceID())...)
	for _, entry := range sessionEnv {
		args = append(args, "-e", entry)
	}
	args = append(args, wrappedProgram)
	if defaultCommand != "" {
		args = append(args, ";", "set-option", "-t", exactTarget(t.sanitizedName), "default-command", defaultCommand)
	}
	t.programMu.RLock()
	remainOnExit := t.remainOnExit
	account := t.account
	accountAgent := t.accountAgent
	t.programMu.RUnlock()
	if remainOnExit {
		// Atomic with new-session so even a command that exits instantly is
		// held as a dead pane rather than racing the option (#4479). The pane's
		// own @af_pane_pid stamp is written by the launch shim inside the pane
		// (sessionenv stamps its own pid — the shim IS the pane root), so no
		// spawn-side command is needed and nothing here races death (#5319).
		args = append(args, ";", "set-option", "-w", "-t", exactTarget(t.sanitizedName), "remain-on-exit", "on")
	}
	// Mask the UNSELECTED account boundary names so a shell tab or
	// default-command process in this account's session can never inherit
	// another account's identity or shell-startup variable from the server's
	// global environment — e.g. a user `set-environment -g`, an
	// update-environment import, or a server that predates the current
	// allowlist. The selected names are written by new-session's -e flags
	// above; the refresh's -r half used to mask the rest on the Start path,
	// and folding it into this same new-session queue keeps the mask without
	// a separate bounded command: a transient set-environment failure here
	// cannot Close() the freshly created, healthy pane, because the existence
	// poll below confirms the pane before a failed command's exit is ever
	// observed (review of #5347).
	if account != "" {
		selected := make(map[string]struct{}, len(sessionEnv))
		for _, entry := range sessionEnv {
			if name, _, ok := strings.Cut(entry, "="); ok {
				selected[name] = struct{}{}
			}
		}
		for _, name := range accountSessionBoundaryNames(accountAgent) {
			if _, ok := selected[name]; ok {
				continue
			}
			args = append(args, ";", "set-environment", "-r", "-t", exactTarget(t.sanitizedName), name)
		}
	}
	// Bootstrap before deciding whether new-session needs a temporary
	// update-environment override. Otherwise the no-server probe below omits
	// the override, the bootstrap creates a filtered server, and the first pane
	// loses its agent-specific environment. Thread the result into command
	// selection so a failed bootstrap falls back without repeating its timeout.
	serverErr := EnsureDaemonServer()
	args, restoreUpdateEnvironment, envErr := t.importClientEnvironmentArgs(args, importNames)
	if envErr != nil {
		// Same proof as above: still read-only, still before new-session.
		t.proveNoPaneIfDeterminatelyAbsent()
		return fmt.Errorf("%w: prepare existing tmux session environment: %w", ErrSessionNotStarted, envErr)
	}
	if restoreUpdateEnvironment != nil {
		// tmux abandons a ;-chained queue at the first failed command, so the
		// queue's own trailing restore is not guaranteed: any mid-queue failure
		// would leave the server's update-environment permanently expanded to
		// the credential import list, and every later launch would re-read the
		// corrupted value as its baseline. Re-apply the baseline on EVERY exit
		// path once the expansion is queued — a slightly delayed restore on a
		// failure rather than never (#5390). Bounded like every tmux command in
		// this package; best-effort, because a server too wedged to answer has
		// no healthy session outcome to withhold anyway.
		defer func() {
			ctx, cancel := tmuxTimeoutContext()
			if err := t.runTmuxBounded(ctx, restoreUpdateEnvironment...); err != nil {
				log.WarningLog.Printf(
					"could not restore tmux update-environment after %s launch (credential import list may stay expanded): %v",
					t.sanitizedName, err)
			}
			cancel()
		}()
	}
	cmd, systemdScoped := newTmuxServerCommandAfterEnsure(serverErr, args...)
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
				err = fmt.Errorf("%w (cleanup error: %w)", err, cleanupErr)
			} else if len(leaked) > 0 {
				// Start failed and we are tearing the partial session down ourselves;
				// its processes are going with it by design, not escaping (#2765).
				go reapSessionProcesses(reapOnRequest, t.sanitizedName, leaked, reapGraceWait, reapTermWait)
			}
		}
		return fmt.Errorf("%w: error starting tmux session: %w", ErrSessionNotStarted, err)
	}

	t.observeStart(StartBeforeExistencePoll)
	// Poll for session existence with exponential backoff. Break only on a probe
	// that ANSWERED "exists" (known && exists): reading the lossy bool here let a
	// mid-poll wedge exit the loop as if the session had come up, so Start reported
	// success for a session tmux never confirmed (#1962). A !known probe means keep
	// waiting until the 2s deadline, then take the timeout path below — which
	// threads pane-state / ErrTmuxTimeout correctly.
	//
	// Each probe is bounded by what is LEFT of the 2s budget, NOT the flat
	// tmuxCommandTimeout. The select's `case <-timeout` (the give-up path) only
	// runs AFTER this synchronous probe returns, so bounding the probe at the
	// flat 10s would let one wedged has-session hold the goroutine for the whole
	// 10s and leave that give-up case unreachable past the poll's own 2s deadline
	// (#2099 poll-loop class). probeSessionWithin/tmuxTimeoutContextWithin are what
	// make the deadline actually fire on time. Mirrors the paste-delivery poll.
	timeout := time.After(sessionReadyDeadline)
	pollDeadline := time.Now().Add(sessionReadyDeadline)
	sleepDuration := 5 * time.Millisecond
	for {
		if exists, known := t.probeSessionWithin(time.Until(pollDeadline)); known && exists {
			// The replacement is live: from here on a vanish is this session's,
			// and af has not asked for it. There is no mark to clear here — the
			// inner Restore installs a fresh monitor, which starts unmarked,
			// while the OLD monitor keeps its mark so a poll in flight across
			// this confirmation still attributes the old session's death to
			// the teardown af asked for (Codex on #4473).
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

				// tmux's own resource failure mode the exit status cannot
				// carry: the server dying on its socket/state filesystem
				// (ENOSPC creating the socket or the tmux-<uid> dir) reports
				// only as "can't create socket"/"no space" text in the
				// child's output — invisible to errors.Is by design, and the
				// lost-restore classifier never reads message text. Probe
				// that filesystem from THIS process so a real resource verdict
				// rides the chain instead of the launch failure charging the
				// structural retry budget (#5295 review).
				socketDir, statErr := tmuxSocketFilesystemDir()
				if probeErr := socketFSProbe(socketDir); probeErr != nil {
					launchErr = errors.Join(launchErr,
						fmt.Errorf("write probe of tmux socket filesystem %s also failed: %w", socketDir, probeErr))
				}
				if statErr != nil {
					launchErr = errors.Join(launchErr,
						fmt.Errorf("could not inspect tmux socket filesystem %s: %w", socketDir, statErr))
				}

				// The launch process began. Its exit status, even paired with later
				// name absence, cannot prove a pane never ran and is not still flushing
				// after tmux removed its session. Keep every answered post-spawn failure
				// outside ErrSessionNotStarted so LocalBackend preserves the worktree.
				return launchErr
			}
		case <-timeout:
			ptmx.Close()
			// Two things look identical from here: tmux never showed us the
			// session inside the deadline (a loaded machine delays the server
			// fork, #4606), or the pane program exited instantly (bad path,
			// rejected flag) and took the whole session down before this poll saw
			// it (#1116, #1131). Say what was observed and name both.
			timeoutErr := sessionReadyTimeoutError(t.sanitizedName, t.programCmd())
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
				timeoutErr = fmt.Errorf("%w (cleanup error: %w)", timeoutErr, cleanupErr)
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
	// session here surfaces as an error rather than recursively re-spawning,
	// and confirmedFresh because the existence poll above already answered for
	// the session THIS Start created — an unanswered rebind probe here is a
	// wedged server, not the retired generation coming back (Codex on #4473).
	t.observeStart(StartBeforeAttachProbe)
	_, err = t.restoreWithResult("", true)
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
			err = fmt.Errorf("%w (cleanup error: %w)", err, cleanupErr)
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

// RestoreResult says whether RestoreWithResult reattached to the persisted
// process or had to create a replacement process for it.
type RestoreResult uint8

const (
	RestoreReattached RestoreResult = iota
	RestoreRespawned
)

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
	_, err := t.RestoreWithResult(workDir)
	return err
}

// RestoreWithResult performs Restore and reports whether its definitive-absence
// branch successfully created a replacement process. Callers that retain facts
// about one concrete pane use this result to retire them only on replacement.
func (t *TmuxSession) RestoreWithResult(workDir string) (RestoreResult, error) {
	return t.restoreWithResult(workDir, false)
}

// restoreWithResult is RestoreWithResult plus confirmedFresh: the caller
// already proved THIS operation's new-session answers the name (Start's inner
// attach), so an unanswered rebind must not inherit the outgoing monitor's
// generation — that generation is retired, and binding the live replacement
// to it latches the fresh monitor dead the moment the server answers again
// (Codex on #4473).
func (t *TmuxSession) restoreWithResult(workDir string, confirmedFresh bool) (RestoreResult, error) {
	// !existsOrUnknown is the definitively-absent branch (#1962): only a session
	// tmux CONFIRMED gone triggers the re-spawn. A wedged→"exists" falls through
	// to the pure rebind below, which is the safe direction — re-spawning against
	// a server that is merely wedged around a still-live session would create a
	// duplicate. The #386 respawn design has always fired only on definitive
	// absence, and this preserves it.
	existsOrUnknown, answered, existErr := t.restoreExistence()
	if existErr != nil {
		return RestoreReattached, existErr
	}
	if !existsOrUnknown {
		if workDir == "" {
			return RestoreReattached, fmt.Errorf("tmux session %q does not exist", t.sanitizedName)
		}
		// The respawn launches new-session -c <workDir>; a worktree deleted
		// since the session persisted means tmux silently falls back to the
		// server's cwd instead of failing (#5172). Refuse up front, naming the
		// path, so the caller routes the session to the lost/rebuild loop
		// rather than launching an agent into an unrelated directory. (Start
		// itself refuses too — this check keeps the refusal typed for restore
		// callers before the program rewrite happens.) The live-session
		// reattach branch below is unaffected: a live pane is not re-launched
		// into anything.
		//
		// The stat is routed through statSocketDir (fsprobe.Stat) for the
		// same reason Start's gate is: this branch runs on the daemon's
		// synchronous restore poll, and a persisted worktree path can sit on
		// an existing-but-wedged mount whose unbounded stat would block the
		// poll (and the locks it holds) forever instead of entering
		// environmental backoff (#5295 review). The bounded non-answer's
		// ErrProbeTimedOut + context.DeadlineExceeded pair is joined into the
		// ErrSpawnDirMissing chain so the classifier routes the session to
		// capped environmental backoff rather than hanging.
		st, err := statSocketDir(workDir)
		if err != nil || st == nil || !st.IsDir() {
			if err == nil {
				err = fmt.Errorf("not a directory")
			}
			return RestoreReattached, fmt.Errorf(
				"%w: tmux session %q missing on Restore and its worktree %s is unavailable: %w",
				ErrSpawnDirMissing, t.sanitizedName, workDir, err)
		}
		log.InfoLog.Printf("tmux session %q missing on Restore; re-spawning in %s", t.sanitizedName, workDir)
		// Program AND declaration together: the resume flags are af-authored, so a
		// stale declaration would refuse an account-scoped restore (#3083 review).
		t.rewriteProgramCmdByAf(t.resumeRewrite())
		if err := t.Start(workDir); err != nil {
			return RestoreReattached, err
		}
		return RestoreRespawned, nil
	}

	// The session is live. Restore is now a pure logical rebind (#1592 Phase 2
	// PR7): it opens NO `tmux attach-session` render client — the local runtime's
	// data plane is the daemon's clientless agent-server (pipe-pane → WS broker,
	// PR5/6), and interactive full-screen attach is a WS subscriber in the client
	// (apiclient.AttachStream). All Restore still owes is a fresh status monitor,
	// swapped under monitorMu because the daemon poll may be inside HasUpdated()
	// reading the old pointer and mutating its fields right now (#1528). A caller
	// with a workDir is reattaching persisted state, so its first capture only
	// establishes the monitor baseline; Start's inner Restore("") keeps the fresh
	// process behavior where first output is an update.
	//
	// Clear the ProvenNoPane and ClosedConclusively flags: reattaching to a LIVE
	// session means this object is now in front of a pane that genuinely exists,
	// so any earlier absence proof is invalidated. Without this clear, a reattach
	// through this branch inherits a stale flag and stopForAccountSwap skips its
	// liveness check for a pane that is still running.
	return RestoreReattached, t.reattach(reattachOptions{
		answered: answered, confirmedFresh: confirmedFresh, refreshAccount: true, baseline: workDir != "",
	})
}

// ReattachOnly performs only the live-session rebind half of
// RestoreWithResult: it never takes the definitive-absence respawn branch, so
// it is the restore primitive for a process tab, whose command must run
// exactly once at creation and never be re-executed by af (#4479). Call it only
// when the session exists or its state is unknown — a confirmed-absent session
// gets no rebind and no respawn, because the pane it named is gone either way.
//
// answered is the caller's own existence probe's known (ProbeSession): true
// only when tmux answered that the session exists, false when the probe did not
// answer. It carries the same meaning as RestoreWithResult's has-session answer
// and binds the fresh monitor the same way (#4473). The rebind does not probe
// existence again, so a wedged server costs the caller one bounded has-session,
// not two.
//
// On a pane held by remain-on-exit the rebind is equally correct: the session
// still exists, the monitor simply never fires, and the pane's retained output
// stays previewable.
func (t *TmuxSession) ReattachOnly(workDir string, answered bool) error {
	return t.reattach(reattachOptions{answered: answered, refreshAccount: true, baseline: workDir != ""})
}

// ReattachInert rebinds a session that is still alive and does nothing else:
// no definitive-absence respawn (there is no directory to spawn into) and no
// account-environment refresh. It is the load path of a row whose worktree was
// deleted outside af (#5102), kept only so its live pane stays bound and the
// row stays archivable. The refresh exists to heal the scope env a later spawn
// inherits, and nothing will spawn into a deleted worktree; running it anyway
// is a dozen bounded tmux commands whose transient failure would load the row
// startup-unknown — excluded from reconciliation and refused by archive, the
// one remedy the row is being kept for. A confirmed-absent session is reported
// exactly as RestoreWithResult("") reports it.
//
// Its monitor baselines like any reattach of persisted state: the pane predates
// this process, so its first capture is not new output. A fresh monitor would
// report it as an update across a mere daemon restart, recording false churn and
// overwriting a persisted Ready or LimitReached with Running.
func (t *TmuxSession) ReattachInert() error {
	existsOrUnknown, answered := sessionExistsReportingAnswer(t.cmdExec, t.sanitizedName)
	if !existsOrUnknown {
		return fmt.Errorf("tmux session %q does not exist", t.sanitizedName)
	}
	return t.reattach(reattachOptions{answered: answered, baseline: true})
}

// reattachOptions are the choices reattach's callers make explicitly.
type reattachOptions struct {
	// answered is whether the existence probe that routed here answered.
	answered bool
	// confirmedFresh is restoreWithResult's: Start's own new-session is what
	// answered the name.
	confirmedFresh bool
	// refreshAccount re-derives the pane's account environment; false only for
	// ReattachInert.
	refreshAccount bool
	// baseline is whether this rebinds persisted state, whose first capture
	// only establishes the monitor's baseline. It used to be inferred from a
	// non-empty workDir, which is what Restore's callers with persisted state
	// pass — but ReattachInert reattaches persisted state with no directory to
	// name (#5102), so the choice is the caller's to state.
	baseline bool
}

// reattach is the live-session rebind shared by restoreWithResult,
// ReattachOnly and ReattachInert.
func (t *TmuxSession) reattach(opts reattachOptions) error {
	answered, confirmedFresh := opts.answered, opts.confirmedFresh
	t.setProvenNoPane(false)
	t.setClosedConclusively(false)
	monitor := newStatusMonitor()
	if opts.baseline {
		monitor = newReattachStatusMonitor()
	}
	// Bind the fresh monitor to the generation that just answered live: the
	// resolved (id, server pid, created) tuple names exactly that tmux
	// session, which neither the reused name nor the bare id can do — a poll
	// holding the OLD monitor across this swap must not land a capture on
	// the replacement and read the old mark against the new generation's
	// death, and a replacement server reissuing the same $id must not
	// impersonate it either (#4473 review). nil degrades to the name target.
	// The probe runs only when the existence check answered: a wedged
	// has-session already spent a full tmuxCommandTimeout, and a second
	// command here would pay the same deadline for the same non-answer —
	// once per persisted tab on the local restore path (Codex on #4473).
	// resolvedAt is stamped BEFORE the probe runs, not after: it is the
	// ordering evidence setMonitor uses to decide whether a settled teardown
	// mark may retire, and only a resolution that BEGAN after the close
	// returned proves the teardown did not take (#4473 review).
	var resolved *tmuxGeneration
	var resolvedAt time.Time
	if answered {
		resolvedAt = time.Now()
		resolved, _ = t.confirmedGeneration()
	}
	// The refresh heals a PRE-EXISTING live session whose boundary environment
	// predates or lost its account scope — the restore path's legitimate use
	// (pinned by TestAccountScopedShellTabInheritsSelectedCredentials). It must
	// NOT run when confirmedFresh: Start's new-session queue already wrote the
	// -e session-env entries and default-command atomically in the same call, so
	// re-deriving them here is pure redundancy whose only effect is a dozen extra
	// bounded tmux commands — and a single transient failure there makes Start
	// call t.Close() and destroy the freshly created, healthy pane. It must not
	// run when the probe went unanswered either: a wedged has-session already
	// spent a full tmuxCommandTimeout, and the refresh's first set-environment
	// would pay the same deadline for the same non-answer — twelve times over —
	// violating the one-bounded-has-session discipline the generation probe
	// above and ReattachOnly's doc both promise (Codex on #4473).
	if opts.refreshAccount && !confirmedFresh && answered {
		if err := t.refreshRestoredAccountEnvironment(); err != nil {
			return fmt.Errorf("%w: %w", ErrAccountEnvironmentRefresh, err)
		}
	}
	// Teardown attribution follows the GENERATION, not the monitor slot: a
	// resolved id matching the outgoing monitor's generation shares its mark
	// (retiring a settled one — the session just answered live), a resolved
	// different generation starts unmarked, and only a fully unanswered
	// rebind carries the old generation — a wedged probe is no evidence the
	// request resolved, so carrying keeps af's own teardown at INFO once the
	// server answers (Codex on #4473). The exception is confirmedFresh:
	// Start's own existence poll already answered for a session THIS Start
	// created, so an unanswered rebind cannot be the retired generation and
	// the fresh monitor must not be bound to it. Either way the OLD monitor
	// keeps its generation, so an in-flight poll still reads its own
	// attribution.
	t.setMonitor(monitor, generationResolution{
		generation: resolved,
		answered:   answered || confirmedFresh,
		startedAt:  resolvedAt,
	})
	return nil
}

// sessionReadyDeadline bounds how long Start waits for tmux to report a freshly
// created session. It is scheduler latency, not work, so a machine under heavy
// load can exceed it with a healthy program (#4606).
const sessionReadyDeadline = 2 * time.Second

// sessionReadyTimeoutError is the Start deadline error. It reports the wait
// that expired and both causes it cannot tell apart, rather than blaming the
// program's flags. The leading "timed out waiting for tmux session <name>" is
// matched by callers and tests.
func sessionReadyTimeoutError(name, program string) error {
	return fmt.Errorf("timed out waiting for tmux session %s: it did not appear within %s. Either the machine is too busy for tmux to create it in time (check the load average and retry), or the pane program exited immediately after launch (check that it runs and accepts its flags) (program: %q)",
		name, sessionReadyDeadline, program)
}
