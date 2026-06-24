package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

var (
	waitForReadyTimeout      = 60 * time.Second
	waitForReadyPollInterval = 500 * time.Millisecond
)

// isReadyContent reports whether the captured pane content indicates that the
// given agent is ready for input — or is showing a trust/confirmation prompt
// that downstream handlers know how to dismiss.
//
// The ready signals differ per agent, so callers resolve the canonical agent
// name (session.DetectAgentFromProgram, which handles legacy free-form Program
// paths) and pass it here. An empty or non-canonical agent falls through to
// the Claude signals — the historical behavior before this became agent-aware
// (#714). This is the single copy: the daemon reaches it via
// task.StartAndSendPrompt (daemon imports task since #782 inverted the old
// task→daemon dependency).
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
	default:
		// claude and any unknown / legacy program (historical default).
		if strings.Contains(content, "❯") ||
			strings.Contains(content, "Do you trust") ||
			strings.Contains(content, "new MCP server") {
			return true
		}
		return isDocTrustPrompt(content)
	}
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
	// per-agent prompt signals; legacy free-form Program values normalize via
	// DetectAgentFromProgram and unknown values fall through to claude (#714).
	agent := session.DetectAgentFromProgram(instance.Program)
	timeout := time.After(waitForReadyTimeout)
	ticker := time.NewTicker(waitForReadyPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			content, err := instance.Preview()
			if err != nil {
				log.ErrorLog.Printf("waitForReady timed out (preview also failed: %v)", err)
				return formatWaitForReadyTimeoutError(waitForReadyTimeout, "")
			}
			log.ErrorLog.Printf("waitForReady timed out. Last pane content: %s", content)
			return formatWaitForReadyTimeoutError(waitForReadyTimeout, content)
		case <-ticker.C:
			content, err := instance.Preview()
			if err != nil {
				continue
			}
			if isReadyContent(content, agent) {
				return nil
			}
		}
	}
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

// NextTaskRunTitle chooses a repo-scoped title for a task run that will not
// collide with persisted sessions or an already-live tmux session. Recurring
// tasks can fire while a previous run is still around, so task sessions cannot
// use the static task name blindly.
func NextTaskRunTitle(repoID, repoPath, baseTitle, program string) (string, error) {
	path, err := config.RepoInstancesPath(repoID)
	if err != nil {
		return "", err
	}

	var title string
	if err := config.WithFileLock(path, func() error {
		raw, err := config.LoadRepoInstances(repoID)
		if err != nil {
			return err
		}

		var existing []session.InstanceData
		if err := json.Unmarshal(raw, &existing); err != nil {
			return fmt.Errorf("failed to parse existing instances: %w", err)
		}

		usedTitles := make([]string, 0, len(existing))
		for _, data := range existing {
			usedTitles = append(usedTitles, data.Title)
		}

		// Mirror the daemon's title-conflict validation so we never hand back a
		// candidate the daemon would reject, which would turn every
		// TUI-triggered run into a round-trip error. The daemon rejects titles
		// that collide either case-insensitively or after branch sanitization
		// (see daemon.titlesCollide / git.SanitizeBranchName). A candidate like
		// "nightly" collides with an existing "Nightly" (#721); "my-task"
		// collides with "My Task" once both sanitize to the same branch (#741).
		// LoadConfig supplies the same BranchPrefix the worktree layer uses;
		// fall back to case-insensitive-only if it is unavailable.
		branchPrefix := ""
		if cfg, cfgErr := config.LoadConfig(); cfgErr == nil {
			branchPrefix = cfg.BranchPrefix
		}
		titleTaken := func(candidate string) bool {
			candidateBranch := git.SanitizeBranchName(branchPrefix + candidate)
			for _, used := range usedTitles {
				if strings.EqualFold(used, candidate) {
					return true
				}
				if git.SanitizeBranchName(branchPrefix+used) == candidateBranch {
					return true
				}
			}
			return false
		}

		for i := 1; i <= 10000; i++ {
			candidate := baseTitle
			if i > 1 {
				candidate = fmt.Sprintf("%s-%d", baseTitle, i)
			}
			if titleTaken(candidate) {
				continue
			}
			if tmux.NewTmuxSessionForRepo(candidate, repoPath, program).DoesSessionExist() {
				continue
			}
			title = candidate
			return nil
		}
		return fmt.Errorf("could not find an available title for %q", baseTitle)
	}); err != nil {
		return "", err
	}

	return title, nil
}
