package session

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

var codexRolloutFileRE = regexp.MustCompile(`(?i)^rollout-.+-([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$`)

// ConversationCaptureSnapshot records provider-local state before a pane is
// spawned. It lets post-spawn capture identify a newly-created conversation
// without reading transcript contents.
type ConversationCaptureSnapshot struct {
	startedAt   time.Time
	codexHome   string
	codexBefore map[string]struct{}
}

// BeginConversationCapture snapshots local provider transcript stores before
// spawning a tab. Today only Codex exposes a local file id we can safely observe;
// other providers degrade to their existing latest-session resume path unless a
// deterministic id was injected before spawn (Claude).
func BeginConversationCapture() ConversationCaptureSnapshot {
	home := codexHomeDir()
	return ConversationCaptureSnapshot{
		startedAt:   time.Now(),
		codexHome:   home,
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
	deadline := time.Now().Add(timeout)
	for {
		conv, retry, err := captureCodexConversationOnce(snap)
		if err != nil || conv.HasID() || !retry || timeout <= 0 || time.Now().After(deadline) {
			return conv, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func captureCodexConversationOnce(snap ConversationCaptureSnapshot) (AgentConversationData, bool, error) {
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
		if id := codexConversationIDFromPath(path); id != "" {
			candidates = append(candidates, id)
		}
	}
	switch len(candidates) {
	case 0:
		return AgentConversationData{}, true, nil
	case 1:
		return AgentConversationData{
			Agent:       tmux.ProgramCodex,
			ID:          candidates[0],
			CapturedAt:  time.Now(),
			CaptureKind: ConversationCaptureCodexRollout,
		}, false, nil
	default:
		return AgentConversationData{}, false, fmt.Errorf("codex conversation capture ambiguous: %d new rollout files appeared", len(candidates))
	}
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
