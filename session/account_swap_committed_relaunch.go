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
// id a transcript already holds. So the re-launch is judged against that store
// by the rule every launch of a recorded Claude id uses (judgeRecordedClaude):
//
//   - nothing is filed under id: start on it, exactly as the first launch did;
//   - id is filed, and no newer conversation is evidence the session moved on:
//     resume it, since it is this session's own conversation and the swap's
//     prompt went to it;
//   - a newer conversation is that evidence (a /clear in a worktree af owns
//     exclusively): resuming would continue a thread the user left, so start
//     fresh on a newly minted id, which the caller records in id's place;
//     reminted reports this.
//
// A store af cannot name or list says nothing about id, so the re-launch starts
// on it as before.
func (i *Instance) committedConversationLaunch(program, workDir, account, id string) (launch string, conversation AgentConversationData, reminted bool) {
	launch, conversation = planLaunchConversation(id, program)
	if !conversation.HasID() {
		// Not a Claude launch af names a conversation for.
		return launch, conversation, false
	}
	if !i.recordNamesAccount(account) {
		return launch, conversation, false
	}
	switch i.judgeRecordedClaude(program, workDir, id) {
	case recordedClaudeAbandoned:
		launch, conversation = planLaunchConversation(newSessionID(), program)
		return launch, conversation, conversation.HasID()
	case recordedClaudeResume:
		if resumed, ok := tmux.ResumeProgramWithConversationID(program, tmux.ProgramClaude, id); ok {
			return resumed, AgentConversationData{
				Agent:       tmux.ProgramClaude,
				ID:          id,
				CapturedAt:  time.Now(),
				CaptureKind: ConversationCaptureInjected,
			}, false
		}
	}
	// Nothing filed, or a store af cannot look in: start on id as before.
	return launch, conversation, false
}

// recordNamesAccount reports whether the record's account is account: the
// commit installs the target account on the record, so a committed re-launch
// judges the target account's store (claudeStoreFor) only while it does.
func (i *Instance) recordNamesAccount(account string) bool {
	i.mu.RLock()
	recorded := strings.TrimSpace(i.Account)
	i.mu.RUnlock()
	return recorded != "" && recorded == strings.TrimSpace(account)
}
