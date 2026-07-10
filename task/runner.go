package task

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

var (
	waitForReadyTimeout      = 60 * time.Second
	waitForReadyPollInterval = 500 * time.Millisecond
	ampPromptFrameTop        = regexp.MustCompile(`(?m)^\s*╭[─ ]+(low|medium|high|deep)[─ ]+╮\s*\n\s*│`)
)

// LimitReachedError is what WaitForReady returns when the agent shows a
// usage-limit banner during startup instead of its input prompt (#1146 PR4):
// the plan is exhausted, so the agent will never become ready and spinning the
// full readiness timeout would record the task run as FAILED. It is a distinct,
// NON-failure outcome — callers PARK the session (mark it LimitReached, record a
// "waiting for the limit window" status, fire no failure side-effects) and let
// the existing resume machinery (the daemon auto-resume scheduler, or the manual
// `c` retry) re-deliver the stored prompt once the window resets. ResetAt is the
// parsed reset time, zero when the banner carried none.
type LimitReachedError struct {
	ResetAt time.Time
}

// ErrLimitReached is the sentinel LimitReachedError wraps so callers can match
// it with errors.As(&LimitReachedError) or errors.Is(ErrLimitReached) without
// depending on the concrete type.
var ErrLimitReached = errors.New("agent hit a usage limit during startup")

func (e *LimitReachedError) Error() string { return ErrLimitReached.Error() }

func (e *LimitReachedError) Unwrap() error { return ErrLimitReached }

// Indirected so tests can pin a fixed detector + clock instead of loading config
// and calling time.Now(). Production resolves the same limit_patterns overrides
// the daemon poll uses, so a hand-tuned detect regex parks a task run identically
// to how it surfaces one (#1146).
var (
	newLimitDetectorForWait = defaultLimitDetectorForWait
	waitLimitNow            = time.Now
)

// defaultLimitDetectorForWait builds the usage-limit detector WaitForReady uses,
// honoring config.LimitPatterns. A config-load failure degrades to the built-in
// claude/codex matchers rather than blocking startup — detection is best-effort
// and must never break the create path.
func defaultLimitDetectorForWait() LimitDetector {
	cfg, err := config.LoadConfig()
	if err != nil {
		return NewLimitDetector(nil)
	}
	return NewLimitDetector(cfg.LimitPatterns)
}

// isReadyContent reports whether the captured pane content indicates that the
// given agent is ready for input — or is showing a trust/confirmation prompt
// that downstream handlers know how to dismiss.
//
// The ready signals differ per agent, so callers resolve the canonical agent
// the pane actually runs (session.Instance.ResolvedAgent, which handles
// program_overrides and legacy free-form Program values) and pass it here. An
// empty agent means the resolved command runs no known agent — a
// program_overrides entry pointing at a plain shell or arbitrary tool
// (#1131) — so no agent's prompt glyph will ever appear; the generic signal
// is any non-blank pane output (the process launched and rendered something;
// WaitForReady separately fails fast if the session dies). This replaces the
// pre-#1131 behavior of falling through to the Claude signals, which spun the
// full 60s timeout for anything that never prints "❯". This is the single
// copy: the daemon reaches it via task.StartAndSendPrompt (daemon imports
// task since #782 inverted the old task→daemon dependency).
func isReadyContent(content, agent string) bool {
	switch agent {
	case tmux.ProgramCodex:
		// codex renders "›" (U+203A — distinct from claude's "❯" U+276F) as
		// its input-prompt glyph after the banner (#714).
		//
		// The workspace-trust dialog ("Do you trust this folder") is
		// deliberately NOT a ready signal (#729): there is no codex-specific
		// dismissal in CheckAndHandleTrustPrompt, so treating it as ready let
		// the next user prompt get typed into the dialog. Wait for the real
		// "›" prompt instead.
		return strings.Contains(content, "›")
	case tmux.ProgramAider:
		// aider prints an "Aider v…" banner, then a line-start "> " prompt.
		return strings.Contains(content, "\n> ") ||
			strings.Contains(content, "Aider v") ||
			isDocTrustPrompt(content)
	case tmux.ProgramGemini:
		// Best-guess (#714): no in-the-wild gemini-cli capture yet. The "╰"
		// box-drawing corner of gemini-cli's frame is a weak readiness signal.
		// TODO(#714): replace with a confirmed gemini-specific ready string.
		return strings.Contains(content, "╰") ||
			isDocTrustPrompt(content)
	case tmux.ProgramAmp:
		// Amp is ready when its input-prompt frame is visible. A bare
		// box-drawing character is too broad: loading frames, banners, and
		// stale scrollback can all contain the same glyphs before the prompt is
		// accepting input.
		return isAmpPromptFrame(content) ||
			isDocTrustPrompt(content)
	case tmux.ProgramClaude:
		if strings.Contains(content, "❯") ||
			strings.Contains(content, "Do you trust") ||
			strings.Contains(content, "new MCP server") {
			return true
		}
		return isDocTrustPrompt(content)
	default:
		// No known agent in the resolved command (#1131): generic readiness —
		// the pane rendered any non-blank output.
		return strings.TrimSpace(content) != ""
	}
}

func isAmpPromptFrame(content string) bool {
	return ampPromptFrameTop.MatchString(content)
}

// isDocTrustPrompt reports whether content shows the documentation-link trust
// dialog shared by aider/gemini (and surfaced by claude). Both substrings are
// required to avoid false positives from unrelated documentation links.
func isDocTrustPrompt(content string) bool {
	return strings.Contains(content, "Open documentation url") &&
		strings.Contains(content, "(D)on't ask again")
}

// WaitForReady polls the instance's tmux pane until the program shows its
// input prompt or trust prompt, or times out after 60 seconds.
func WaitForReady(instance *session.Instance) error {
	// Resolve the canonical agent once so isReadyContent matches the right
	// per-agent prompt signals (#714). ResolvedAgent detects the agent from
	// the command the pane actually runs — not the config-name enum — so a
	// program_overrides entry pointing at a different program gets that
	// program's readiness heuristic, and a non-agent override gets the
	// generic one instead of waiting 60s for a claude glyph (#1116, #1131).
	agent := instance.ResolvedAgent()
	// Resolve the usage-limit detector once so a claude/codex pane that shows a
	// limit banner mid-startup is recognized and PARKED, not spun into a failure
	// (#1146 PR4). Only claude/codex ever match; other agents never park here.
	detector := newLimitDetectorForWait()
	timeout := time.After(waitForReadyTimeout)
	ticker := time.NewTicker(waitForReadyPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			content, err := instance.Preview()
			if err != nil {
				// Mirror the ticker case: ErrSessionGone is a definitive,
				// non-retryable death, so surface the actionable "session died"
				// cause even when it happens right at the timeout boundary —
				// never the misleading generic timeout error (#979 fixed only
				// the ticker case; #989 closes this gap so both Preview() call
				// sites handle the sentinel identically).
				if errors.Is(err, tmux.ErrSessionGone) {
					return fmt.Errorf("session died while waiting for agent to start: %w", err)
				}
				log.ErrorLog.Printf("waitForReady timed out (preview also failed: %v)", err)
				return formatWaitForReadyTimeoutError(waitForReadyTimeout, "")
			}
			// A limit banner may be all that ever rendered: park instead of
			// failing even at the timeout boundary, symmetric with the ticker case.
			if perr := limitParkError(detector, content, agent); perr != nil {
				return perr
			}
			log.ErrorLog.Printf("waitForReady timed out. Last pane content: %s", content)
			return formatWaitForReadyTimeoutError(waitForReadyTimeout, content)
		case <-ticker.C:
			content, err := instance.Preview()
			if err != nil {
				// ErrSessionGone is a definitive, non-retryable failure: the
				// tmux session no longer exists, so it can never become ready.
				// Fail fast with a clear cause instead of polling the full
				// timeout and returning a misleading "timed out" error (#976).
				// Other errors are transient — keep polling.
				if errors.Is(err, tmux.ErrSessionGone) {
					return fmt.Errorf("session died while waiting for agent to start: %w", err)
				}
				continue
			}
			// Check the usage-limit banner BEFORE readiness: a limit-blocked pane
			// never shows the ready glyph, but checking limit first keeps the
			// intent explicit and future-proofs against a banner that also carried
			// one. On a hit, stop waiting and return the park sentinel (#1146).
			if perr := limitParkError(detector, content, agent); perr != nil {
				return perr
			}
			if isReadyContent(content, agent) {
				return nil
			}
		}
	}
}

// limitParkError returns a *LimitReachedError when content shows a usage-limit
// banner for agent, else nil. Factored out so both the ticker and timeout
// branches of WaitForReady detect the banner identically (#1146 PR4).
func limitParkError(detector LimitDetector, content, agent string) error {
	if hit, resetAt, _ := detector.Check(content, agent, waitLimitNow()); hit {
		return &LimitReachedError{ResetAt: resetAt}
	}
	return nil
}

// formatWaitForReadyTimeoutError builds the user-facing timeout error. When
// the captured pane content is non-empty, the error body carries a trimmed
// snippet of the last few lines so users see what the agent was doing instead
// of an opaque "timed out" message. See sachiniyer/agent-factory#502.
func formatWaitForReadyTimeoutError(timeout time.Duration, content string) error {
	base := fmt.Sprintf("timed out waiting for program to start (%s)", timeout)
	snippet := trimPaneSnippet(content)
	if snippet == "" {
		return errors.New(base)
	}
	var b strings.Builder
	b.WriteString(base)
	b.WriteString("\nlast pane content:")
	for _, line := range strings.Split(snippet, "\n") {
		b.WriteString("\n  ")
		b.WriteString(line)
	}
	return errors.New(b.String())
}

// trimPaneSnippet returns at most the last 5 non-empty trailing lines of the
// captured pane content, capped at 400 bytes. ANSI escape sequences are left
// intact — keeping the snippet short matters more than stripping them.
func trimPaneSnippet(content string) string {
	lines := strings.Split(content, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return ""
	}
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	out := strings.Join(lines, "\n")
	if len(out) > 400 {
		out = out[len(out)-400:]
	}
	return out
}

// TaskRunBaseTitle returns the preferred title for a task-created session.
func TaskRunBaseTitle(t Task) string {
	if t.Name != "" {
		return t.Name
	}
	return fmt.Sprintf("task-%s", t.ID)
}
