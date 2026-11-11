package session

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// ErrCodexConversationAmbiguous means the pane's runtime holds more than one
// top-level rollout for its launch directory, so af cannot say which thread
// the pane is on and keeps whatever it has recorded (#7101).
var ErrCodexConversationAmbiguous = errors.New("codex conversation ambiguous")

// HeldAgentConversation names the conversation a bound snapshot's runtime is
// in RIGHT NOW: for Codex, the one top-level rollout in the snapshot's store,
// correlated to its launch directory, that the pane's process set holds open.
// It is the baseline-free half of capture (#7101). The new-since-baseline diff
// can only see a rollout minted after the snapshot was taken, so a capture
// armed after the daemon restarted, a deferred arm that claimed a swap's
// replacement generation after its mission minted the file, or a pane that
// `/resume`d an older thread could never be recorded — and a recorded id
// could never follow the pane to a thread it started later (`/new`).
//
// Codex keeps its recorder's descriptor on the active thread's rollout for
// the life of that thread (codex-rs core/rollout/recorder.rs), alongside one
// per running sub-agent, which classification excludes (#6766). A file held
// open by the pane's own runtime is that runtime's thread; nothing about
// when it was minted matters.
//
// An empty result with a nil error means nothing to record: an unbound or
// store-less snapshot, a provider with no inspectable store, or a runtime
// that holds no matching rollout (a Codex pane that has not prompted yet). A
// non-nil error is a real failure to answer and callers surface it: the
// process walk failed, a held header is unclassifiable, or the runtime holds
// more than one candidate (ErrCodexConversationAmbiguous).
//
// The cost is bounded and store-independent: one process-table read plus the
// pane members' descriptor lists, then one stat per held rollout, and a
// header read only for a rollout this daemon has not classified before.
func HeldAgentConversation(agent string, snap ConversationCaptureSnapshot) (AgentConversationData, error) {
	if agent != tmux.ProgramCodex {
		return AgentConversationData{}, nil
	}
	return snap.heldCodexConversation()
}

func (s ConversationCaptureSnapshot) heldCodexConversation() (AgentConversationData, error) {
	if strings.TrimSpace(s.codexHome) == "" || s.runtime == nil || s.runtime.answerHeld == nil {
		return AgentConversationData{}, nil
	}
	conv, err := s.runtime.answerHeld(s.judgeHeldCodexRollouts)
	if err != nil && !errors.Is(err, ErrCodexConversationAmbiguous) && !errors.Is(err, errCodexCaptureUndecided) {
		return AgentConversationData{}, fmt.Errorf("could not read which Codex rollout the runtime holds open: %w", err)
	}
	return conv, err
}

// judgeHeldCodexRollouts picks the conversation out of a runtime's held path
// set and names the file it came from.
func (s ConversationCaptureSnapshot) judgeHeldCodexRollouts(held map[string]struct{}) (AgentConversationData, string, error) {
	root := pathutil.ResolveForCompare(filepath.Join(s.codexHome, "sessions")) + string(filepath.Separator)
	var matches []string
	unclassified := 0
	for path := range held {
		// The name filter runs first: it is free, and it leaves only rollout
		// files for the symlink resolution and header classification below.
		if codexConversationIDFromPath(path) == "" {
			continue
		}
		if !strings.HasPrefix(pathutil.ResolveForCompare(path), root) {
			// Another store — never this snapshot's conversation.
			continue
		}
		cwd, subagent, ok := classifyHeldCodexRollout(path)
		switch {
		case !ok:
			unclassified++
		case subagent:
			// A sub-agent's thread, held by the same process (#6766).
		case s.workingDir == "" || pathutil.ResolveForCompare(cwd) == s.workingDir:
			matches = append(matches, path)
		}
	}
	switch {
	case len(matches) > 1:
		ids := make([]string, 0, len(matches))
		for _, path := range matches {
			ids = append(ids, codexConversationIDFromPath(path))
		}
		sort.Strings(ids)
		return AgentConversationData{}, "", fmt.Errorf("%w: the runtime holds %d top-level rollouts for this worktree (%s)",
			ErrCodexConversationAmbiguous, len(matches), strings.Join(ids, ", "))
	case unclassified > 0:
		return AgentConversationData{}, "", fmt.Errorf("%w: the runtime holds %d Codex rollouts whose session metadata cannot be classified",
			errCodexCaptureUndecided, unclassified)
	case len(matches) == 1:
		return AgentConversationData{
			Agent:       tmux.ProgramCodex,
			ID:          codexConversationIDFromPath(matches[0]),
			CapturedAt:  time.Now(),
			CaptureKind: ConversationCaptureCodexRollout,
		}, matches[0], nil
	}
	return AgentConversationData{}, "", nil
}

// heldCodexHeader is one classified session_meta. Codex writes the header
// once, as the first line, and never rewrites it, so a classification keyed
// to the file's identity is final for that file.
type heldCodexHeader struct {
	cwd      string
	subagent bool
}

// heldCodexHeaderCacheLimit bounds the memo. A daemon sees one entry per
// rollout a live pane ever held; the limit only stops a pathological store
// from growing it without bound, and clearing costs a header re-read each.
const heldCodexHeaderCacheLimit = 4096

// heldCodexHeaderKey pairs the path with its (device, inode): an inode freed
// by a deleted rollout can be reused, but never under the same name, since
// the name carries the thread's uuid.
type heldCodexHeaderKey struct {
	path string
	id   fileID
}

var heldCodexHeaders = struct {
	sync.Mutex
	byKey map[heldCodexHeaderKey]heldCodexHeader
}{byKey: make(map[heldCodexHeaderKey]heldCodexHeader)}

// classifyHeldCodexRollout is codexRolloutSessionMeta memoized by path and
// file identity: the follow check asks it on every rearm for every recorded Codex
// session, and re-reading a header that cannot change would turn a quiet
// check into steady file I/O. Only a complete classification is cached; a
// header Codex has not finished writing is read again next time.
func classifyHeldCodexRollout(path string) (cwd string, subagent, ok bool) {
	id, idOK := fileIdentityOf(path)
	key := heldCodexHeaderKey{path: path, id: id}
	if idOK {
		heldCodexHeaders.Lock()
		header, hit := heldCodexHeaders.byKey[key]
		heldCodexHeaders.Unlock()
		if hit {
			return header.cwd, header.subagent, true
		}
	}
	cwd, subagent, ok = codexRolloutSessionMeta(path)
	if ok && idOK {
		heldCodexHeaders.Lock()
		if len(heldCodexHeaders.byKey) >= heldCodexHeaderCacheLimit {
			heldCodexHeaders.byKey = make(map[heldCodexHeaderKey]heldCodexHeader)
		}
		heldCodexHeaders.byKey[key] = heldCodexHeader{cwd: cwd, subagent: subagent}
		heldCodexHeaders.Unlock()
	}
	return cwd, subagent, ok
}

// ConversationFollowFrozen reports why the recorded conversation must not be
// re-pointed right now, or "" when it may be. An account swap or handoff that
// is still pending, or one whose launch plan (and with it any conversation
// carry) is still staged, reads the recorded id as the conversation it carries
// or retires; moving the record under it could carry the wrong thread. Any
// in-flight operation is excluded too: it owns the runtime the evidence would
// come from (#7101 lift condition).
func (i *Instance) ConversationFollowFrozen() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.conversationFollowFrozenLocked()
}

func (i *Instance) conversationFollowFrozenLocked() string {
	switch {
	case i.userKilled:
		return "the session is being killed"
	case i.inFlightOp != OpNone:
		return fmt.Sprintf("an operation (%s) is in flight", opLabel(i.inFlightOp))
	case i.pendingAccountSwap != nil:
		return "an account swap is still pending"
	case i.accountSwapLaunch != nil:
		return "an account swap's launch plan is still staged"
	case strings.TrimSpace(i.pendingHandoffMission) != "":
		return "a handoff mission is still pending"
	}
	return ""
}

// FollowAgentConversationForRuntime re-points the recorded conversation from
// prev to conv — the thread the live runtime now holds open — and reports
// whether it changed. It refuses unless token still names the live
// generation, conv belongs to its provider, the record is still exactly prev
// (an operator's set-conversation or a sibling write since the caller read it
// is newer than this judgement), and nothing has frozen the record.
func (i *Instance) FollowAgentConversationForRuntime(token AgentRuntimeToken, prev, conv AgentConversationData) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !conv.HasID() || !i.runtimeTokenCurrentLocked(token, conv) {
		return false
	}
	if i.conversationFollowFrozenLocked() != "" {
		return false
	}
	if len(i.Tabs) == 0 || i.Tabs[0].Conversation != prev {
		return false
	}
	return i.setAgentConversationLocked(conv)
}
