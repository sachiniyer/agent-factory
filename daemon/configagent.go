package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	xansi "github.com/charmbracelet/x/ansi"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
)

// The config agent's runtime: a BARE tmux session, owned by the daemon, with no
// session.Instance behind it.
//
// That is the whole point. An Instance is a row — it is persisted to
// instances.json, it is what Snapshot() iterates to build the session list, and
// it is restorable and killable from the sidebar. The config agent must be none
// of those things, so it cannot be an Instance. And the two requirements are not
// merely in tension, they are the same bit: the WS PTY attach route resolves its
// byte source by looking the session up in m.instances (agentServerForStream →
// resolveStreamSession), and Snapshot() builds the roster from that same map. To
// be attachable through that route IS to be a row. So the config agent does not
// use it: the TUI hands the terminal to `tmux attach-session` via
// tea.ExecProcess instead, and the daemon owns the session's life.
//
// Everything below therefore uses only the Instance-free half of session/tmux:
// NewTmuxSession, Start (any working dir — no git repo required, which matters
// because AF home is not one), CapturePaneContentContext, CheckAndHandleTrustPrompt,
// SendKeysCommand and Close.

// configAgentSessionPrefix is the tmux session-name prefix for config agents. It
// is distinct from any session title so a config agent can never collide with —
// or be mistaken for — a real session's tmux.
const configAgentSessionPrefix = "af-config-"

// configAgentSeq makes each config-agent session name unique within this daemon.
// A name collision would make Start fail ("tmux session already exists"), and
// two config agents CAN legitimately be asked for in one daemon lifetime (the
// user closes one and presses C again).
var configAgentSeq atomic.Uint64

// configAgentPromptReceiptTimeout bounds how long the spawn WAITS to CONFIRM
// that Codex recorded the briefing as a user turn before it hands the terminal
// over. Codex writes its user turn locally before model work begins, so on a
// healthy box the confirmation lands in well under a second; this is startup
// I/O, not an API-response budget.
//
// Its expiry is deliberately NOT fatal (#2483). The receipt is a POSITIVE
// signal — its absence within the window does not prove the briefing failed to
// land (the tmux paste+Enter already succeeded, and a slow or degraded Codex
// simply records the turn after the window). Reaping the session on that
// inferred negative left an operator on a degraded default agent unable to
// configure af at all, which is strictly worse than the send-and-attach every
// normal session does (task.WaitForReadyAndSendPrompt). So an unconfirmed
// receipt attaches anyway and logs why; only a cancelled spawn tears down.
//
// A var keeps the receipt regressions fast.
var configAgentPromptReceiptTimeout = 5 * time.Second

// After the TUI is allowed to attach, keep observing for a bounded interval so
// a merely late receipt and a persistently unconfirmed delivery do not collapse
// into the same log line. A var keeps the receipt regressions fast.
var configAgentPromptReceiptEscalationTimeout = 30 * time.Second

// configAgentReceiptJoinTimeout bounds how long a lifecycle path waits for a
// cancelled receipt watcher to unwind. Cancellation is only observed BETWEEN
// receipt scans: a scan sits in filepath.WalkDir / os.ReadFile under CODEX_HOME,
// neither of which takes a context, so a stalled filesystem makes the watcher
// uninterruptible for as long as that I/O blocks. An unbounded join would then
// hang daemon shutdown before it ever closes the tracked tmux sessions, which is
// a far worse outcome than letting one goroutine finish on its own — abandoning
// the join is safe because a cancelled watcher stays silent when it returns.
//
// Healthy watchers return the moment they are cancelled, so this only ever trips
// on the stall it exists for. A var so the regression can run in milliseconds.
var configAgentReceiptJoinTimeout = 5 * time.Second

// configAgentLateReceiptRecheckDelay anchors the single late receipt re-check
// (#5471): it fires once, this long AFTER the briefing was delivered — not after
// the escalation window expired. The rollout record is durable-or-absent by
// then, so its result finally separates "the receipt was just slow" from "the
// briefing never became a turn" without extending the attach-path windows. A var
// keeps the receipt regressions fast.
var configAgentLateReceiptRecheckDelay = 5 * time.Minute

// configAgentLateReceiptRecheckWindow bounds the late re-check's own poll. At
// that horizon the receipt is durable-or-absent; the window only absorbs a
// record Codex is still appending when the scan lands. A var so the regression
// can run in milliseconds.
var configAgentLateReceiptRecheckWindow = 5 * time.Second

// configAgentPaneCaptureTimeout bounds the pre-delivery pane capture. The
// excerpt is a best-effort diagnostic collected synchronously on the spawn
// path; it must never hold up briefing delivery, so it gets a short budget of
// its own instead of the full tmuxCommandTimeout a wedged capture-pane would
// otherwise burn (#5471 review).
var configAgentPaneCaptureTimeout = 2 * time.Second

// The pane excerpt logged when a briefing receipt stays unconfirmed is bounded
// twice: in lines and in bytes. The tail is what is kept — a trust, login, or
// update modal that absorbed Enter (#5471) is the thing rendered at the bottom
// of the pane when the window expires.
const (
	configAgentPaneExcerptMaxLines = 30
	configAgentPaneExcerptMaxBytes = 4096
)

type configAgentReceiptWatcher struct {
	cancel context.CancelFunc
	done   <-chan struct{}
}

// joinReceiptWatchers cancels every watcher and waits for their goroutines to
// unwind under ONE shared deadline, so N stalled watchers cost the caller one
// configAgentReceiptJoinTimeout rather than N of them. Giving up is reported,
// never silent: an abandoned watcher is a live goroutine blocked on filesystem
// I/O, and that is worth an operator seeing.
func joinReceiptWatchers(watchers []configAgentReceiptWatcher) {
	for _, watcher := range watchers {
		watcher.cancel()
	}
	if len(watchers) == 0 {
		return
	}
	timer := time.NewTimer(configAgentReceiptJoinTimeout)
	defer timer.Stop()
	for _, watcher := range watchers {
		select {
		case <-watcher.done:
		case <-timer.C:
			log.WarningLog.Printf(
				"config agent: a briefing-receipt watcher did not unwind within %s of cancellation; "+
					"continuing without it (its rollout scan under CODEX_HOME is blocked in filesystem I/O)",
				configAgentReceiptJoinTimeout,
			)
			return
		}
	}
}

// configAgentSupervisor owns every bare tmux session this daemon spawned for a
// config agent, so none can outlive its use.
//
// The daemon owns them rather than the TUI for two reasons, both learned here:
// a TUI-spawned process is one the daemon cannot reap if the TUI crashes, and
// this repo has a documented history of orphaned tmux sessions (#1093, #1104).
// Stop() is registered with the daemon's other teardown at construction — before
// the control socket binds — so a SIGTERM during warm-up cannot skip it, exactly
// as vscodeSupervisor.Stop() is.
type configAgentSupervisor struct {
	mu              sync.Mutex
	sessions        map[string]*tmux.TmuxSession
	receiptWatchers map[string]configAgentReceiptWatcher
	// stopped latches on daemon teardown so a spawn racing shutdown cannot
	// register a session nothing will ever reap.
	stopped bool
}

func newConfigAgentSupervisor() *configAgentSupervisor {
	return &configAgentSupervisor{
		sessions:        make(map[string]*tmux.TmuxSession),
		receiptWatchers: make(map[string]configAgentReceiptWatcher),
	}
}

// track registers a live config-agent session. It returns false when the daemon
// is already shutting down, in which case the caller must tear the session down
// itself rather than leak it.
func (c *configAgentSupervisor) track(name string, ts *tmux.TmuxSession) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return false
	}
	c.sessions[name] = ts
	return true
}

// session returns the live tmux session tracked under name, or nil if none is (an
// unknown name, or one already reaped). The web config-assistant stream (#2467)
// uses it to wrap a just-spawned config agent's bare tmux session in a
// session.BareSessionStreamer without threading the *tmux.TmuxSession back out
// through SpawnConfigAgent's name+socket return.
func (c *configAgentSupervisor) session(name string) *tmux.TmuxSession {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessions[name]
}

// watchReceipt keeps a bounded post-attach receipt check under the same
// ownership as the bare tmux session. Reap and daemon shutdown cancel and join
// it, so a closed config agent cannot emit a later degradation warning.
func (c *configAgentSupervisor) watchReceipt(name string, watch func(context.Context)) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	c.mu.Lock()
	if c.stopped || c.sessions[name] == nil {
		c.mu.Unlock()
		cancel()
		close(done)
		return
	}
	c.receiptWatchers[name] = configAgentReceiptWatcher{cancel: cancel, done: done}
	c.mu.Unlock()

	go func() {
		defer close(done)
		watch(ctx)
		c.mu.Lock()
		if current, ok := c.receiptWatchers[name]; ok && current.done == done {
			delete(c.receiptWatchers, name)
		}
		c.mu.Unlock()
	}()
}

// reap kills one config-agent session and forgets it. Unknown names are a no-op
// success: reap is called on a best-effort path (the TUI returning from the
// takeover), and a session already gone is the desired end state either way.
func (c *configAgentSupervisor) reap(name string) error {
	c.mu.Lock()
	ts, ok := c.sessions[name]
	delete(c.sessions, name)
	watcher, watching := c.receiptWatchers[name]
	delete(c.receiptWatchers, name)
	c.mu.Unlock()
	if watching {
		joinReceiptWatchers([]configAgentReceiptWatcher{watcher})
	}
	if !ok {
		return nil
	}
	// Pane state deliberately ignored (#1917): a config agent has no worktree (see
	// HooksDone above), so Close's PaneState cannot gate any destructive step —
	// there is nothing to delete under a possibly-live pane.
	if _, err := ts.Close(); err != nil {
		return fmt.Errorf("failed to close config-agent session %s: %w", name, err)
	}
	return nil
}

// Stop reaps every config-agent session this daemon owns. Called from the
// daemon's teardown.
func (c *configAgentSupervisor) Stop() {
	c.mu.Lock()
	c.stopped = true
	sessions := make([]*tmux.TmuxSession, 0, len(c.sessions))
	for _, ts := range c.sessions {
		sessions = append(sessions, ts)
	}
	c.sessions = make(map[string]*tmux.TmuxSession)
	watchers := make([]configAgentReceiptWatcher, 0, len(c.receiptWatchers))
	for _, watcher := range c.receiptWatchers {
		watchers = append(watchers, watcher)
	}
	c.receiptWatchers = make(map[string]configAgentReceiptWatcher)
	c.mu.Unlock()

	// Bounded: killing the tracked tmux sessions below is the part of shutdown
	// that must not be skipped, and a watcher wedged in receipt I/O must never
	// stand in front of it.
	joinReceiptWatchers(watchers)

	var wg sync.WaitGroup
	for _, ts := range sessions {
		wg.Add(1)
		go func(ts *tmux.TmuxSession) {
			defer wg.Done()
			// Pane state ignored: config agents have no worktree (see HooksDone).
			if _, err := ts.Close(); err != nil {
				log.WarningLog.Printf("config agent: closing session failed during shutdown: %v", err)
			}
		}(ts)
	}
	wg.Wait()
}

// tmuxReadinessTarget adapts a bare tmux session to task.ReadinessTarget, so the
// config agent's startup uses the SAME readiness logic every session uses —
// per-agent isReadyContent matching, the usage-limit park, the timeout budget —
// rather than a second copy that would drift.
type tmuxReadinessTarget struct {
	ts *tmux.TmuxSession
	// agent is the resolved agent name, detected from the command the pane
	// actually runs (not a config enum), matching Instance.ResolvedAgent.
	agent string
}

func (t tmuxReadinessTarget) ResolvedAgent() string { return t.agent }

func (t tmuxReadinessTarget) PreviewContent(ctx context.Context) (string, error) {
	return t.ts.CapturePaneContentContext(ctx)
}

// HooksDone is always nil: a config agent has no worktree and therefore no
// post-worktree hooks, so the readiness budget arms immediately — the same path
// an external worktree already takes.
func (t tmuxReadinessTarget) HooksDone() <-chan struct{} { return nil }

// configAgentTrustTarget is tmuxReadinessTarget plus the trust-prompt check, so
// the config agent runs task.DismissTrustPrompt — the same loop, on the same
// budget, as every regular session.
//
// The budget used to live here as a second pair of constants (5 attempts ×
// 500ms) under a comment claiming they mirrored task/start.go's, which they
// never did: the canonical budget is 20 × 1s, so a config agent whose trust
// dialogs took longer than 2.5s failed with "trust prompt did not dismiss" —
// and the caller reaps the session and shows the error, so the user is left
// unable to configure af — where a regular session would have succeeded
// (#2097). The numbers are not copied here any more; the loop is shared.
type configAgentTrustTarget struct{ tmuxReadinessTarget }

func (t configAgentTrustTarget) CheckAndHandleTrustPrompt() bool {
	return t.ts.CheckAndHandleTrustPrompt()
}

// SpawnConfigAgent starts a config agent in a bare tmux session rooted at AF
// home, waits for it to be ready, dismisses any trust prompt, delivers the
// briefing, and returns the tmux session name AND the absolute socket path for
// the caller to attach to.
//
// The socket path is returned alongside the name so the TUI can attach with
// `tmux -S <socket> attach-session` (#2019): the daemon and the TUI are
// different processes and can resolve different TMUX_TMPDIR values, so the
// default-socket path is not something the attaching side can assume. The socket
// this session lives on is authoritative only here, where it was spawned.
//
// AF home — not the user's repo — is the working directory, and that is a
// deliberate improvement over the in-place seam this replaces: the agent's cwd
// is now the directory whose config it is editing, rather than the user's live
// working tree. tmux.Start takes any directory and requires no git repo, which
// is what makes AF home usable at all (every Instance provisioning path hard-errors
// outside a repo).
//
// On ANY failure the session is torn down before returning, so a half-started
// config agent never survives as an orphan.
func (m *Manager) SpawnConfigAgent(ctx context.Context, req SpawnConfigAgentRequest) (string, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req.Program == "" {
		return "", "", fmt.Errorf("config agent: no program to run")
	}
	home, err := config.GetConfigDir()
	if err != nil {
		return "", "", fmt.Errorf("config agent: cannot resolve the agent-factory home: %w", err)
	}
	// Detect and snapshot before launch so Codex cannot create its rollout between
	// the before-image and tmux start. The receipt path is resolved from the exact
	// command environment, not the daemon's CODEX_HOME (#2228 review), and the
	// snapshot anchors "new rollout" to launch time: a receipt scan counts only
	// rollouts created after this point, so an older unrelated rollout cannot be
	// mistaken for the launched process's own (#5471).
	agent := tmux.DetectAgentFromCommand(req.Program)
	var promptReceipt session.ConversationCaptureSnapshot
	// isCodexResume gates two diagnostics-only mitigations: a resumed pane can
	// still show the previous conversation — arbitrary private text a credential
	// scrubber cannot recognize — so the pane excerpt is suppressed, and a
	// resumed rollout predates the delivery snapshot, so absence from the
	// new-rollout set cannot disprove delivery and "never arrived" degrades to
	// an indeterminate verdict (#5471 review).
	var isCodexResume bool
	if agent == tmux.ProgramCodex && req.Prompt != "" {
		codexHome, rerr := configAgentCodexReceiptHome(req.Program, home)
		if rerr != nil {
			return "", "", fmt.Errorf("config agent: cannot locate the launched Codex receipt store: %w", rerr)
		}
		promptReceipt = session.BeginConversationCaptureAtCodexHome(codexHome)
		isCodexResume = len(tmux.ConversationSelectorArgs(req.Program)) > 0
	}

	// seq is only for uniqueness within this daemon; the tmux session NewTmuxSession
	// actually creates is TmuxPrefix + this (af_af-config-<n>) — see SanitizedName
	// below. Keeping the af_ prefix is what keeps the session inside the namespace
	// every af cleanup path recognizes (`^af_`).
	seq := fmt.Sprintf("%s%d", configAgentSessionPrefix, configAgentSeq.Add(1))
	ts := tmux.NewTmuxSession(seq, req.Program)
	currentConfig, err := config.LoadConfig()
	if err != nil {
		return "", "", fmt.Errorf("config agent: cannot load current session environment configuration: %w", err)
	}
	if err := ts.SetEnvPassthrough(currentConfig.SessionEnvPassthrough); err != nil {
		return "", "", fmt.Errorf("config agent: invalid session environment pass-through: %w", err)
	}
	if err := ts.Start(home); err != nil {
		return "", "", fmt.Errorf("config agent: failed to start tmux session: %w", err)
	}
	// The name the caller must attach to and reap by is the name tmux actually
	// knows — the sanitized name, WITH the af_ prefix — not the bare seq. Returning
	// the bare seq was a latent bug: `tmux attach-session -t af-config-<n>` does not
	// resolve the real session af_af-config-<n>, so the attach failed with "can't
	// find session" (masked inside tmux by the nesting refusal this PR fixes, but a
	// hard failure everywhere once $TMUX is scrubbed). Track, reap, and return the
	// resolvable name so every side agrees (#2019).
	sessionName := ts.SanitizedName()
	if !m.configAgents.track(sessionName, ts) {
		// The daemon is shutting down; do not leak the session we just made. Pane
		// state ignored: no worktree, so nothing destructive follows (see HooksDone).
		_, _ = ts.Close()
		return "", "", fmt.Errorf("config agent: the daemon is shutting down")
	}
	// Any failure past this point tears the session down rather than leaving a
	// tmux nobody owns.
	fail := func(err error) (string, string, error) {
		if rerr := m.configAgents.reap(sessionName); rerr != nil {
			m.warn().Printf("config agent: cleanup after a failed spawn also failed: %v", rerr)
		}
		return "", "", err
	}

	// The absolute socket path this session lives on, queried from tmux itself so
	// it is authoritative rather than recomputed from TMUX_TMPDIR (#2019). A
	// failure here is NOT fatal: the attach can still resolve the session via its
	// own default socket when both sides share a TMUX_TMPDIR, which is the common
	// case — so degrade to an empty path and let that fall back, rather than
	// failing a spawn that would otherwise attach fine.
	socketPath, serr := ts.SocketPath(ctx)
	if serr != nil {
		m.warn().Printf("config agent: could not resolve the tmux socket path for %s (%v); the attach will fall back to the default socket", sessionName, serr)
		socketPath = ""
	}

	if err := task.WaitForReadyOn(ctx, tmuxReadinessTarget{ts: ts, agent: agent}); err != nil {
		return fail(fmt.Errorf("config agent: %w", err))
	}
	if err := dismissConfigAgentTrustPrompt(ctx, configAgentTrustTarget{tmuxReadinessTarget{ts: ts, agent: agent}}); err != nil {
		return fail(fmt.Errorf("config agent: %w", err))
	}
	// The briefing rides in over a tmux paste buffer (stdin-streamed), so its
	// length is not bounded by ARG_MAX and it never becomes a command-line flag —
	// an unknown flag would kill the agent at exec and read as a readiness
	// timeout.
	if req.Prompt != "" {
		// The pane as it stood AT delivery (#5471 review): the modal that can
		// absorb the briefing's Enter is up NOW, and the same Enter usually
		// dismisses or redraws it — the trust modal of #2220 reveals a bare
		// composer on acceptance — so any capture taken after the send would
		// log the wrong state and mislead triage. It is also necessarily
		// pre-attach: no caller can be typing into the pane before this spawn
		// returns. The buffer is held for the receipt watcher and only logged
		// if the receipt stays unconfirmed.
		var paneContent string
		var paneErr error
		var paneCapturedAt time.Time
		if agent == tmux.ProgramCodex && !isCodexResume {
			paneCapturedAt = time.Now()
			// Best-effort: the excerpt is only diagnostic, so the capture runs
			// on a short budget and can never stall the send behind a wedged
			// capture-pane (#5471 review).
			capCtx, capCancel := context.WithTimeout(ctx, configAgentPaneCaptureTimeout)
			paneContent, paneErr = ts.CapturePaneContentContext(capCtx)
			capCancel()
		}
		if err := ts.SendKeysCommand(req.Prompt); err != nil {
			return fail(fmt.Errorf("config agent: failed to deliver the briefing: %w", err))
		}
		// Anchors both the reported receipt latency and the late re-check: both
		// are measured from DELIVERY, not from when a poll happened to observe.
		briefingDeliveredAt := time.Now()
		// A successful tmux send is not a delivery receipt, so we try to CONFIRM
		// one. In the #2220 reproduction every load/paste/Enter command succeeded
		// against Codex's directory-trust modal; Enter merely selected "Yes", Codex
		// opened a bare composer, and the briefing never became a user turn. Codex's
		// rollout is the receiver-owned boundary, and a confirmed turn there is the
		// only thing that proves the briefing landed. Pane pixels cannot substitute
		// — real Codex renders the same `› [Pasted Content …]` for both a pending
		// paste and a submitted user message.
		//
		// But a confirmation we could not get within the window is not proof the
		// briefing failed (#2483): the paste+Enter already succeeded, and a slow or
		// degraded Codex records the turn just after our budget. Reaping on that
		// inferred negative broke the assistant on a degraded default agent
		// entirely. So an unconfirmed receipt attaches anyway and records why —
		// matching the send-and-attach every normal session does — and only a
		// cancelled spawn (the client/daemon is going away) tears the session down,
		// as every other step on this context does.
		if agent == tmux.ProgramCodex {
			if err := session.WaitForPromptReceipt(ctx, agent, promptReceipt, req.Prompt, configAgentPromptReceiptTimeout); err != nil {
				if ctx.Err() != nil {
					return fail(fmt.Errorf("config agent: spawn cancelled while awaiting the briefing receipt: %w", err))
				}
				if errors.Is(err, session.ErrPromptReceiptNotObserved) {
					m.info().Printf(
						"config agent: %s briefing receipt was not observed within %s (%v)",
						sessionName, configAgentPromptReceiptTimeout, err,
					)
					m.configAgents.watchReceipt(sessionName, func(receiptCtx context.Context) {
						lateErr := session.WaitForPromptReceipt(
							receiptCtx,
							agent,
							promptReceipt,
							req.Prompt,
							configAgentPromptReceiptEscalationTimeout,
						)
						// Cancellation is checked FIRST, and for both outcomes. The
						// config agent is being reaped or the daemon is shutting
						// down, so nothing this watcher learned is still worth
						// reporting — and because the join that follows
						// cancellation is bounded, this goroutine may well outlive
						// the session it is describing.
						if receiptCtx.Err() != nil {
							return
						}
						if lateErr == nil {
							m.info().Printf(
								"config agent: %s briefing receipt confirmed after the initial %s window (%s after delivery)",
								sessionName, configAgentPromptReceiptTimeout,
								time.Since(briefingDeliveredAt).Round(100*time.Millisecond),
							)
							return
						}
						if !errors.Is(lateErr, session.ErrPromptReceiptNotObserved) {
							m.warn().Printf(
								"config agent: %s could not continue checking the briefing receipt after the initial %s window: %v",
								sessionName, configAgentPromptReceiptTimeout, lateErr,
							)
							return
						}
						m.warn().Printf(
							"config agent: %s briefing receipt was still not observed after %s (%v); delivery remains unconfirmed",
							sessionName,
							configAgentPromptReceiptTimeout+configAgentPromptReceiptEscalationTimeout,
							lateErr,
						)
						if isCodexResume {
							m.info().Printf(
								"config agent: %s pane excerpt suppressed after unconfirmed briefing receipt: the resumed pane may hold the previous conversation",
								sessionName,
							)
						} else {
							m.logConfigAgentPaneExcerpt(sessionName, paneContent, paneErr, briefingDeliveredAt.Sub(paneCapturedAt))
						}
						// One late re-check (#5471): the 5s/35s windows cannot tell
						// a slow rollout from a briefing that never became a turn,
						// so at a horizon where the record is durable-or-absent the
						// rollout is consulted one last time. The wait parks on the
						// watcher context, so reap and daemon shutdown still unwind
						// this goroutine instantly.
						if wait := configAgentLateReceiptRecheckDelay - time.Since(briefingDeliveredAt); wait > 0 {
							timer := time.NewTimer(wait)
							select {
							case <-receiptCtx.Done():
								timer.Stop()
								return
							case <-timer.C:
							}
						}
						if receiptCtx.Err() != nil {
							return
						}
						// Nothing remains to diagnose once the session is gone —
						// reaped out from under the watcher, or the pane exited on
						// its own without one. Skip quietly either way, but ONLY
						// on positive evidence: ProbeSessionStrict keeps a
						// transient has-session failure from laundering into
						// "gone" and silently abandoning the verdict (#5471
						// review).
						if m.configAgents.session(sessionName) == nil {
							return
						}
						if exists, known, _ := ts.ProbeSessionStrict(); known && !exists {
							return
						}
						finalErr := session.WaitForPromptReceipt(
							receiptCtx,
							agent,
							promptReceipt,
							req.Prompt,
							configAgentLateReceiptRecheckWindow,
						)
						if receiptCtx.Err() != nil {
							return
						}
						elapsed := time.Since(briefingDeliveredAt).Round(100 * time.Millisecond)
						switch {
						case finalErr == nil:
							m.info().Printf(
								"config agent: %s briefing receipt observed at the late re-check, %s after delivery",
								sessionName, elapsed,
							)
						case errors.Is(finalErr, session.ErrPromptReceiptNotObserved):
							if isCodexResume {
								// The resumed rollout predates the snapshot, so
								// absence from the new-file set cannot disprove
								// delivery — an indeterminate result, not never.
								m.warn().Printf(
									"config agent: %s briefing receipt could not be verified; a resumed rollout predates the delivery snapshot %s after delivery",
									sessionName, elapsed,
								)
							} else {
								m.warn().Printf(
									"config agent: %s briefing receipt never arrived; still unobserved %s after delivery",
									sessionName, elapsed,
								)
							}
						default:
							m.warn().Printf(
								"config agent: %s late receipt re-check could not determine the outcome: %v",
								sessionName, finalErr,
							)
						}
					})
				} else {
					m.warn().Printf(
						"config agent: %s could not confirm the briefing receipt: %v",
						sessionName, err,
					)
				}
			}
		}
	}
	m.info().Printf("config agent: started %s in %s on socket %q (agent %q)", sessionName, home, socketPath, agent)
	return sessionName, socketPath, nil
}

// configAgentCodexReceiptHome resolves the same CODEX_HOME the launched shell
// command will give Codex. Command-local assignments win over daemon variables;
// an unset CODEX_HOME falls back through the command-specific HOME. Relative
// values resolve against the effective launch cwd, including GNU env -C.
func configAgentCodexReceiptHome(program, workingDir string) (string, error) {
	return tmux.CodexHomeFromCommand(program, workingDir)
}

// dismissConfigAgentTrustPrompt clears the agent's first-run trust dialog, the
// same loop task.StartAndSendPromptWithConversationCapture runs for a normal
// session.
//
// The per-agent gate is tmux.ProgramNeedsTrustDismissal — the same call
// LocalBackend.CheckAndHandleTrustPrompt makes, not a copy of its list. This
// used to enumerate the agents here under a comment claiming it mirrored that
// one, and it had already drifted: opencode was missing (#2416). The exposure
// was not a hang: isReadyContent's opencode arm calls a doc-trust dialog ready,
// so had opencode ever raised one the spawn would have gone on to deliver the
// briefing INTO it — #729's signature. opencode is not known to raise one, which
// is why this stayed latent rather than being reported.
func dismissConfigAgentTrustPrompt(ctx context.Context, target task.TrustPromptTarget) error {
	if !tmux.ProgramNeedsTrustDismissal(target.ResolvedAgent()) {
		return nil // AF has no dismissal to run for this program
	}
	return task.DismissTrustPrompt(ctx, target)
}

// ReapConfigAgent tears down a config-agent session by name. It is the caller's
// "I am done with the takeover" signal; the supervisor's Stop() is the backstop
// for a client that never sends it.
func (m *Manager) ReapConfigAgent(req ReapConfigAgentRequest) error {
	if req.SessionName == "" {
		return fmt.Errorf("config agent: no session name to reap")
	}
	return m.configAgents.reap(req.SessionName)
}

// logConfigAgentPaneExcerpt logs the pre-delivery pane capture taken just
// before the briefing was sent — a bounded, sanitized excerpt (#5471). It
// exists to answer the one question the unconfirmed-receipt windows cannot:
// whether a modal — trust, login, update — absorbed the Enter meant to submit
// the briefing, because that modal is what was VISIBLE on the pane when the
// keys landed. The capture precedes both the send and any possible attach, so
// what reaches the log shows the modal that was actually up rather than the
// composer its dismissal revealed — and can never contain user input. A
// capture failure still logs, because a pane that cannot be read at delivery
// is itself the diagnostic.
func (m *Manager) logConfigAgentPaneExcerpt(sessionName, content string, captureErr error, capturedBeforeDelivery time.Duration) {
	if captureErr != nil {
		m.warn().Printf(
			"config agent: %s briefing receipt is unconfirmed and the pane could not be captured for diagnostics: %v",
			sessionName, captureErr,
		)
		return
	}
	m.info().Printf(
		"config agent: %s pane excerpt after unconfirmed briefing receipt (captured %s before delivery):\n%s",
		sessionName, capturedBeforeDelivery.Round(100*time.Millisecond), configAgentPaneExcerpt(content),
	)
}

// configAgentPaneExcerpt renders captured pane content safe to write to the
// daemon log: ANSI sequences stripped (xansi.Strip — the same stripper
// ui/err.go's sanitizeError and the delivery-witness matching in
// session/tmux/submit.go use, so no bespoke regex misses an OSC variant),
// residual C0/C1/DEL controls dropped except the newlines and tabs that ARE the
// picture, credential shapes removed by the same RedactCredentials policy the
// log sink itself applies, and the whole thing tail-bounded so a modal drawn at
// the bottom of the pane is what survives the cut.
func configAgentPaneExcerpt(content string) string {
	clean := xansi.Strip(content)
	clean = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return -1
		}
		return r
	}, clean)
	clean = log.RedactCredentials(clean)
	if strings.TrimSpace(clean) == "" {
		return "(empty pane)"
	}
	lines := strings.Split(strings.TrimRight(clean, "\n"), "\n")
	truncated := len(lines) > configAgentPaneExcerptMaxLines
	if truncated {
		lines = lines[len(lines)-configAgentPaneExcerptMaxLines:]
	}
	tail := strings.Join(lines, "\n")
	for len(tail) > configAgentPaneExcerptMaxBytes {
		truncated = true
		if idx := strings.IndexByte(tail, '\n'); idx >= 0 {
			// Cut whole lines from the top so the excerpt can never begin
			// mid-token.
			tail = tail[idx+1:]
			continue
		}
		// A single line longer than the cap gets a byte cut; land it on a rune
		// boundary so the log never sees half a UTF-8 sequence.
		tail = tail[len(tail)-configAgentPaneExcerptMaxBytes:]
		for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
			tail = tail[1:]
		}
	}
	if truncated {
		return "… (truncated)\n" + tail
	}
	return tail
}
