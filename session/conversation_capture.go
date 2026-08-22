package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

var codexRolloutFileRE = regexp.MustCompile(`(?i)^rollout-.+-([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$`)

var (
	// ErrPromptReceiptUnavailable means the provider exposes no receiver-side
	// acknowledgement af knows how to read. It is distinct from a missing
	// receipt: unsupported is a capability question; not-observed means a
	// provider that does expose receipts never recorded this prompt.
	ErrPromptReceiptUnavailable = errors.New("prompt receipt unavailable")
	// ErrPromptReceiptNotObserved means the receiver never recorded the prompt
	// before the acknowledgement deadline. A tmux send returning nil does not
	// override this observation (#2220).
	ErrPromptReceiptNotObserved = errors.New("prompt receipt not observed")
	// ErrPromptReceiptAmbiguous means more than one new receiver conversation
	// appeared after the snapshot. Prompt text cannot identify which process owns
	// which rollout, even when only one has recorded it so far (#2228 review).
	ErrPromptReceiptAmbiguous = errors.New("prompt receipt ambiguous")
)

// errCodexCaptureUndecided marks a retryable observation in which at least one
// new rollout file exists but its session_meta cannot be classified yet. It is
// what separates "no evidence appeared" (absence, bounded by the caller's
// timeout) from "evidence exists and is still being written" (bounded by
// codexSessionMetaSettleTimeout on top). Both refuse if their bound expires.
var errCodexCaptureUndecided = errors.New("codex conversation capture undecided")

const promptReceiptPollInterval = 50 * time.Millisecond

// codexSessionMetaSettleTimeout bounds how much longer capture keeps polling
// past its base timeout when a new rollout file exists but its session_meta
// header is not yet classifiable. The base timeout answers "did a rollout
// appear at all"; once one has, the only open question is evidence that Codex
// has not finished writing. Codex 0.147.0 was measured writing the header
// 2.6–2.8s after process start (#3266), so a base window sized for file
// creation expired moments before the header became readable and a successful
// handoff permanently lost its conversation id. 15s is ~5x that observed
// latency; a header still unclassifiable at this bound gets the same undecided
// refusal as before. Var, not const, so tests can shorten the bound.
var codexSessionMetaSettleTimeout = 15 * time.Second

// ConversationCaptureSnapshot records provider-local state before a pane is
// spawned. It lets post-spawn capture identify a newly-created conversation
// without reading transcript contents.
type ConversationCaptureSnapshot struct {
	startedAt   time.Time
	codexHome   string
	workingDir  string
	codexBefore map[string]struct{}
}

func cloneConversationCaptureSnapshot(snap ConversationCaptureSnapshot) ConversationCaptureSnapshot {
	copy := snap
	if snap.codexBefore != nil {
		copy.codexBefore = make(map[string]struct{}, len(snap.codexBefore))
		for path := range snap.codexBefore {
			copy.codexBefore[path] = struct{}{}
		}
	}
	return copy
}

// BeginConversationCapture snapshots local provider transcript stores before
// spawning a tab. Today only Codex exposes a local file id we can safely observe;
// other providers degrade to their existing latest-session resume path unless a
// deterministic id was injected before spawn (Claude).
func BeginConversationCapture() ConversationCaptureSnapshot {
	return BeginConversationCaptureAtCodexHome(codexHomeDir())
}

// BeginConversationCaptureAtCodexHome snapshots an exact Codex store rather
// than the daemon's inherited CODEX_HOME. Config-agent commands can carry an
// inline environment assignment, so the process-specific path must be resolved
// before launch and supplied here (#2228 review).
func BeginConversationCaptureAtCodexHome(home string) ConversationCaptureSnapshot {
	return beginConversationCaptureAtCodexHomeAndWorkingDir(home, "")
}

func beginConversationCaptureAtCodexHomeAndWorkingDir(home, workingDir string) ConversationCaptureSnapshot {
	if workingDir != "" {
		workingDir = pathutil.ResolveForCompare(workingDir)
	}
	return ConversationCaptureSnapshot{
		startedAt:   time.Now(),
		codexHome:   home,
		workingDir:  workingDir,
		codexBefore: codexRolloutFiles(home),
	}
}

// CaptureAgentConversation waits until a supported provider exposes the
// conversation id for a just-spawned tab. Unsupported providers return empty
// data and nil error so callers gracefully keep existing --last/latest behavior.
func CaptureAgentConversation(agent string, snap ConversationCaptureSnapshot, timeout time.Duration) (AgentConversationData, error) {
	switch agent {
	case tmux.ProgramCodex:
		return captureCodexConversation(snap, timeout)
	default:
		return AgentConversationData{}, nil
	}
}

// WaitForPromptReceipt waits for the agent's own durable conversation store to
// record prompt as a user turn. This is deliberately receiver-side: successful
// tmux load-buffer, paste-buffer and send-keys calls establish only that af's
// input path did not error, not that a modal/composer accepted a turn (#2220).
//
// Codex is currently the only provider with a local receipt af can correlate to
// a just-spawned process. Callers must take snap before spawning the pane. Other
// providers return ErrPromptReceiptUnavailable rather than manufacturing an
// acknowledgement from pane pixels.
func WaitForPromptReceipt(
	ctx context.Context,
	agent string,
	snap ConversationCaptureSnapshot,
	prompt string,
	timeout time.Duration,
) error {
	if agent != tmux.ProgramCodex {
		return fmt.Errorf("%w for agent %q", ErrPromptReceiptUnavailable, agent)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := time.Now().Add(timeout)
	for {
		received, err := codexPromptReceiptOnce(snap, prompt)
		if err != nil {
			return err
		}
		if received {
			return nil
		}
		if timeout <= 0 || !time.Now().Before(deadline) {
			return fmt.Errorf("%w in Codex rollout within %s", ErrPromptReceiptNotObserved, timeout)
		}

		wait := promptReceiptPollInterval
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func codexHomeDir() string {
	if home := strings.TrimSpace(os.Getenv("CODEX_HOME")); home != "" {
		return home
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".codex")
	}
	return ""
}

func captureCodexConversation(snap ConversationCaptureSnapshot, timeout time.Duration) (AgentConversationData, error) {
	if snap.codexHome == "" {
		return AgentConversationData{}, nil
	}
	start := time.Now()
	absentDeadline := start.Add(timeout)
	settleDeadline := start.Add(timeout + codexSessionMetaSettleTimeout)
	for {
		conv, retry, err := captureCodexConversationOnce(snap)
		if conv.HasID() || !retry || timeout <= 0 {
			return conv, err
		}
		// Expiry while a concrete candidate is still unclassifiable is not
		// absence (#3266): the file already appeared inside the base window, so
		// the extended bound only waits for Codex to finish writing a header
		// that classification — unchanged and fail-closed — will then rule on.
		deadline := absentDeadline
		if errors.Is(err, errCodexCaptureUndecided) {
			deadline = settleDeadline
		}
		if time.Now().After(deadline) {
			return conv, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func captureCodexConversationOnce(snap ConversationCaptureSnapshot) (AgentConversationData, bool, error) {
	observed := newCodexRolloutFiles(snap)
	candidates, uncorrelated := codexRolloutFilesForWorkingDir(observed, snap.workingDir)
	if uncorrelated > 0 {
		// Codex can create each file before its first JSONL record is visible.
		// Retry unknowns instead of treating an incomplete session_meta as either
		// a match or proof that the rollout belongs to another process. The error
		// is returned only if the caller's bounded retry window expires.
		return AgentConversationData{}, true, fmt.Errorf(
			"%w: %d new rollout files appeared and %d had uncorrelatable session metadata",
			errCodexCaptureUndecided, len(observed), uncorrelated)
	}
	switch len(candidates) {
	case 0:
		return AgentConversationData{}, true, nil
	case 1:
		return AgentConversationData{
			Agent:       tmux.ProgramCodex,
			ID:          codexConversationIDFromPath(candidates[0]),
			CapturedAt:  time.Now(),
			CaptureKind: ConversationCaptureCodexRollout,
		}, false, nil
	default:
		return AgentConversationData{}, false, fmt.Errorf(
			"codex conversation capture ambiguous: %d new rollout files matched launch working directory",
			len(candidates))
	}
}

func codexRolloutFilesForWorkingDir(paths []string, workingDir string) ([]string, int) {
	if workingDir == "" {
		return paths, 0
	}
	if !filepath.IsAbs(workingDir) {
		return nil, len(paths)
	}
	var matches []string
	uncorrelated := 0
	for _, path := range paths {
		cwd, ok := codexRolloutWorkingDir(path)
		if !ok {
			uncorrelated++
			continue
		}
		if pathutil.ResolveForCompare(cwd) == workingDir {
			matches = append(matches, path)
		}
	}
	return matches, uncorrelated
}

func codexRolloutWorkingDir(path string) (string, bool) {
	data, err := readCodexRollout(path)
	if err != nil {
		return "", false
	}
	line, _, _ := bytes.Cut(data, []byte{'\n'})
	var metadata struct {
		Type    string `json:"type"`
		Payload struct {
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(line, &metadata); err != nil ||
		metadata.Type != "session_meta" || !filepath.IsAbs(metadata.Payload.Cwd) {
		return "", false
	}
	return metadata.Payload.Cwd, true
}

func newCodexRolloutFiles(snap ConversationCaptureSnapshot) []string {
	after := codexRolloutFiles(snap.codexHome)
	var candidates []string
	for path := range after {
		if _, existed := snap.codexBefore[path]; existed {
			continue
		}
		info, err := os.Stat(path)
		if err == nil && info.ModTime().Before(snap.startedAt.Add(-time.Second)) {
			continue
		}
		candidates = append(candidates, path)
	}
	return candidates
}

func codexPromptReceiptOnce(snap ConversationCaptureSnapshot, prompt string) (bool, error) {
	candidates := newCodexRolloutFiles(snap)
	if len(candidates) == 0 {
		return false, nil
	}
	if len(candidates) > 1 {
		return false, fmt.Errorf("%w: %d new Codex rollout files appeared", ErrPromptReceiptAmbiguous, len(candidates))
	}
	matches := 0
	for _, path := range candidates {
		received, err := codexRolloutHasPrompt(path, prompt)
		if err != nil {
			return false, err
		}
		if received {
			matches++
		}
	}
	switch matches {
	case 0:
		// Another Codex process can create a rollout during this spawn. Do not
		// fail merely because it exists: the exact briefing may appear in this
		// config agent's rollout on the next poll.
		return false, nil
	case 1:
		return true, nil
	default:
		// One candidate can contribute at most one match. Keep this defensive arm
		// so future receipt formats cannot silently weaken that invariant.
		return false, fmt.Errorf("%w: one rollout produced %d matches", ErrPromptReceiptAmbiguous, matches)
	}
}

func codexRolloutHasPrompt(path, prompt string) (bool, error) {
	data, err := readCodexRollout(path)
	if err != nil {
		return false, err
	}
	normalizedPrompt := normalizePromptReceiptText(prompt)
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			// Codex appends JSONL while af polls it. A read can land between the
			// write and newline of the newest event; an incomplete line is not a
			// negative receipt and will be retried on the next poll.
			continue
		}
		var payload struct {
			Type string `json:"type"`
			Role string `json:"role"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			continue
		}
		isUserTurn := event.Type == "event_msg" && payload.Type == "user_message"
		isUserItem := event.Type == "response_item" && payload.Type == "message" && payload.Role == "user"
		if !isUserTurn && !isUserItem {
			continue
		}
		var value any
		if err := json.Unmarshal(event.Payload, &value); err != nil {
			continue
		}
		if jsonValueContainsPrompt(value, normalizedPrompt) {
			return true, nil
		}
	}
	return false, nil
}

func readCodexRollout(path string) ([]byte, error) {
	file, err := os.OpenFile(
		path,
		os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC|syscall.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("open Codex rollout %s: %w", path, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect opened Codex rollout %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("read Codex rollout %s: not a regular file (%s)", path, info.Mode().Type())
	}

	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("read Codex rollout %s: %w", path, err)
	}
	return data, nil
}

func jsonValueContainsPrompt(value any, prompt string) bool {
	switch v := value.(type) {
	case string:
		return prompt != "" && strings.Contains(normalizePromptReceiptText(v), prompt)
	case []any:
		for _, item := range v {
			if jsonValueContainsPrompt(item, prompt) {
				return true
			}
		}
	case map[string]any:
		for _, item := range v {
			if jsonValueContainsPrompt(item, prompt) {
				return true
			}
		}
	}
	return false
}

func normalizePromptReceiptText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "\n")
}

func codexRolloutFiles(codexHome string) map[string]struct{} {
	out := make(map[string]struct{})
	if codexHome == "" {
		return out
	}
	root := filepath.Join(codexHome, "sessions")
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return filepath.SkipDir
			}
			return nil
		}
		if d == nil || d.IsDir() {
			return nil
		}
		if codexConversationIDFromPath(path) == "" {
			return nil
		}
		out[path] = struct{}{}
		return nil
	})
	return out
}

func codexConversationIDFromPath(path string) string {
	matches := codexRolloutFileRE.FindStringSubmatch(filepath.Base(path))
	if len(matches) != 2 {
		return ""
	}
	return strings.ToLower(matches[1])
}
