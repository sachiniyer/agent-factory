package session

import (
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// staleCommittedConversationReason is why a committed replacement re-launches
// on a fresh conversation (committedConversationLaunch). It is the pending
// record's carry fallback, so it reads as a clause: the notice renders it after
// "but" and the log after "because".
const staleCommittedConversationReason = "the conversation the replacement started is no longer the newest one in this worktree " +
	"(a new one was started there, for example with /clear)"

// committedConversationLaunch plans the re-launch of a committed replacement on
// the fresh conversation id its commit recorded (#7214). The first launch
// started Claude with --session-id id under the target account's store, and
// Claude files that conversation's transcript there. Once it has, a second
// --session-id id never boots: Claude refuses to start a new conversation on an
// id a transcript already holds. So the re-launch is judged against that store,
// from one listing, by the rule the restore guard judges a recorded Claude
// conversation by (judgeClaudeRecorded, staleRecordedClaudeDecline):
//
//   - nothing is filed under id: start on it, exactly as the first launch did;
//   - id is filed and still the newest conversation in the worktree: resume it,
//     since it is this session's own conversation and the swap's prompt went
//     to it;
//   - id is filed but a newer conversation exists: resume it too, unless the
//     newer one is evidence that this session moved on, which only a worktree
//     af created for this session alone gives (newerConversationIsEvidence,
//     #7216 review R1). In the user's checkout, an in-place row or an adopted
//     worktree, a sibling on the same account files there too. With that
//     evidence (a /clear in the replacement), resuming would continue a thread
//     the user left: start fresh on a newly minted id instead, which the caller
//     records in id's place; reminted reports this.
//
// A store af cannot name or list says nothing about id, so the re-launch starts
// on it as before.
func (i *Instance) committedConversationLaunch(program, workDir, account, id string) (launch string, conversation AgentConversationData, reminted bool) {
	launch, conversation = planLaunchConversation(id, program)
	if !conversation.HasID() {
		// Not a Claude launch af names a conversation for.
		return launch, conversation, false
	}
	configDir, launchDir, ok := i.committedClaudeStore(program, workDir, account)
	if !ok {
		return launch, conversation, false
	}
	verdict := judgeClaudeRecorded(configDir, claudeProjectCandidates(launchDir), id, "")
	if verdict == claudeRecordedStale && i.newerConversationIsEvidence(launchDir) {
		launch, conversation = planLaunchConversation(newSessionID(), program)
		return launch, conversation, conversation.HasID()
	}
	if verdict == claudeRecordedCurrent || verdict == claudeRecordedStale {
		if resumed, ok := tmux.ResumeProgramWithConversationID(program, tmux.ProgramClaude, id); ok {
			return resumed, AgentConversationData{
				Agent:       tmux.ProgramClaude,
				ID:          id,
				CapturedAt:  time.Now(),
				CaptureKind: ConversationCaptureInjected,
			}, false
		}
	}
	return launch, conversation, false
}

// committedClaudeStore is the Claude store and launch directory a committed
// replacement's launch writes: the target account's registry directory, which
// the session environment hands the pane as CLAUDE_CONFIG_DIR (claudeStoreFor),
// and the directory the command runs Claude in. The commit already installed
// account on the record, so the record's account is the target; ok is false
// when it is not, or when either directory cannot be named.
func (i *Instance) committedClaudeStore(program, workDir, account string) (configDir, launchDir string, ok bool) {
	i.mu.RLock()
	recorded := strings.TrimSpace(i.Account)
	i.mu.RUnlock()
	if recorded == "" || recorded != strings.TrimSpace(account) {
		return "", "", false
	}
	configDir, launchDir, err := i.claudeStoreFor(program, workDir)
	if err != nil || strings.TrimSpace(configDir) == "" || strings.TrimSpace(launchDir) == "" {
		return "", "", false
	}
	return configDir, launchDir, true
}
