package daemon

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
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
	mu       sync.Mutex
	sessions map[string]*tmux.TmuxSession
	// stopped latches on daemon teardown so a spawn racing shutdown cannot
	// register a session nothing will ever reap.
	stopped bool
}

func newConfigAgentSupervisor() *configAgentSupervisor {
	return &configAgentSupervisor{sessions: make(map[string]*tmux.TmuxSession)}
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

// reap kills one config-agent session and forgets it. Unknown names are a no-op
// success: reap is called on a best-effort path (the TUI returning from the
// takeover), and a session already gone is the desired end state either way.
func (c *configAgentSupervisor) reap(name string) error {
	c.mu.Lock()
	ts, ok := c.sessions[name]
	delete(c.sessions, name)
	c.mu.Unlock()
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
	c.mu.Unlock()

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

// configAgentTrustPromptAttempts bounds the trust-prompt dismissal loop,
// mirroring task.StartAndSendPrompt's own cap.
const configAgentTrustPromptAttempts = 5

// configAgentTrustRetryDelay is the pause between trust-prompt dismissal
// attempts, mirroring task.StartAndSendPrompt.
const configAgentTrustRetryDelay = 500 * time.Millisecond

// SpawnConfigAgent starts a config agent in a bare tmux session rooted at AF
// home, waits for it to be ready, dismisses any trust prompt, delivers the
// briefing, and returns the tmux session name for the caller to attach to.
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
func (m *Manager) SpawnConfigAgent(ctx context.Context, req SpawnConfigAgentRequest) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req.Program == "" {
		return "", fmt.Errorf("config agent: no program to run")
	}
	home, err := config.GetConfigDir()
	if err != nil {
		return "", fmt.Errorf("config agent: cannot resolve the agent-factory home: %w", err)
	}

	name := fmt.Sprintf("%s%d", configAgentSessionPrefix, configAgentSeq.Add(1))
	ts := tmux.NewTmuxSession(name, req.Program)
	if err := ts.Start(home); err != nil {
		return "", fmt.Errorf("config agent: failed to start tmux session: %w", err)
	}
	if !m.configAgents.track(name, ts) {
		// The daemon is shutting down; do not leak the session we just made. Pane
		// state ignored: no worktree, so nothing destructive follows (see HooksDone).
		_, _ = ts.Close()
		return "", fmt.Errorf("config agent: the daemon is shutting down")
	}
	// Any failure past this point tears the session down rather than leaving a
	// tmux nobody owns.
	fail := func(err error) (string, error) {
		if rerr := m.configAgents.reap(name); rerr != nil {
			log.WarningLog.Printf("config agent: cleanup after a failed spawn also failed: %v", rerr)
		}
		return "", err
	}

	// The agent the pane ACTUALLY runs, detected from the command — so an
	// override pointing "claude" at something else gets that program's readiness
	// heuristic and trust-prompt handling, not claude's (#1116, #1131).
	agent := tmux.DetectAgentFromCommand(req.Program)

	if err := task.WaitForReadyOn(ctx, tmuxReadinessTarget{ts: ts, agent: agent}); err != nil {
		return fail(fmt.Errorf("config agent: %w", err))
	}
	if err := dismissConfigAgentTrustPrompt(ctx, ts, agent); err != nil {
		return fail(fmt.Errorf("config agent: %w", err))
	}
	// The briefing rides in over a tmux paste buffer (stdin-streamed), so its
	// length is not bounded by ARG_MAX and it never becomes a command-line flag —
	// an unknown flag would kill the agent at exec and read as a readiness
	// timeout.
	if req.Prompt != "" {
		if err := ts.SendKeysCommand(req.Prompt); err != nil {
			return fail(fmt.Errorf("config agent: failed to deliver the briefing: %w", err))
		}
	}
	log.InfoLog.Printf("config agent: started %s in %s (agent %q)", name, home, agent)
	return name, nil
}

// dismissConfigAgentTrustPrompt clears the agent's first-run trust dialog, the
// same loop task.StartAndSendPrompt runs for a normal session.
//
// The per-agent gate mirrors LocalBackend.CheckAndHandleTrustPrompt
// (session/backend_local.go): only the known agents have a trust dialog, and
// asking a non-agent command about one would tap Enter into an arbitrary
// program. The decision is keyed off the RESOLVED agent for the same reason it
// is there.
func dismissConfigAgentTrustPrompt(ctx context.Context, ts *tmux.TmuxSession, agent string) error {
	switch agent {
	case tmux.ProgramClaude, tmux.ProgramCodex, tmux.ProgramAider, tmux.ProgramGemini, tmux.ProgramAmp:
	default:
		return nil // not a known agent: it has no trust dialog to dismiss
	}
	for attempts := 0; ts.CheckAndHandleTrustPrompt(); attempts++ {
		if attempts+1 >= configAgentTrustPromptAttempts {
			return fmt.Errorf("trust prompt did not dismiss after %d attempts", configAgentTrustPromptAttempts)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(configAgentTrustRetryDelay):
		}
		if err := task.WaitForReadyOn(ctx, tmuxReadinessTarget{ts: ts, agent: agent}); err != nil {
			return err
		}
	}
	return nil
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
