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

// blindCommittedResumeReason is why a committed replacement re-launches on
// --resume of its committed id although the transcript store could not be
// listed: the record says the first launch already filed the id there, so
// resuming is the blind answer that boots (#7596). It is the pending record's
// carry fallback, so it reads as a clause the way the other reasons do.
const blindCommittedResumeReason = "its transcript store could not be listed, " +
	"so the re-launch resumes the committed conversation af's record says the first launch filed"

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
// A store af cannot name at all says nothing about id, so the re-launch starts
// on it as before. A store af can name but not list is different (#7596): the
// listing is judged again — a still-wedged mount's timed-out flights are
// latched, so the retry answers at once inside the bound the first judge
// already spent, and only a store that recovered pays a fresh bound — and if
// the listing stays blind the record decides: it says the first launch ran
// (ReplacementPanesStarted), so id's transcript is filed and --resume is the
// blind answer that boots, where a second --session-id id never boots;
// blindResumed reports this so the record and the notice can say it. A record
// whose panes never started describes no filing, so it, too, starts on id.
func (i *Instance) committedConversationLaunch(program, workDir, account, id string) (launch string, conversation AgentConversationData, reminted, blindResumed bool) {
	launch, conversation = planLaunchConversation(id, program)
	if !conversation.HasID() {
		// Not a Claude launch af names a conversation for.
		return launch, conversation, false, false
	}
	if !i.recordNamesAccount(account) {
		return launch, conversation, false, false
	}
	verdict := i.judgeRecordedClaude(program, workDir, id)
	nameable := verdict == recordedClaudeBlind && i.claudeStoreNameable(program, workDir)
	if nameable {
		verdict = i.judgeRecordedClaude(program, workDir, id)
	}
	switch verdict {
	case recordedClaudeAbandoned:
		launch, conversation = planLaunchConversation(newSessionID(), program)
		return launch, conversation, conversation.HasID(), false
	case recordedClaudeResume:
		if resumed, ok := tmux.ResumeProgramWithConversationID(program, tmux.ProgramClaude, id); ok {
			return resumed, AgentConversationData{
				Agent:       tmux.ProgramClaude,
				ID:          id,
				CapturedAt:  time.Now(),
				CaptureKind: ConversationCaptureInjected,
			}, false, false
		}
	}
	if verdict == recordedClaudeBlind && nameable && i.replacementPanesStarted(account) {
		if resumed, ok := tmux.ResumeProgramWithConversationID(program, tmux.ProgramClaude, id); ok {
			return resumed, AgentConversationData{
				Agent:       tmux.ProgramClaude,
				ID:          id,
				CapturedAt:  time.Now(),
				CaptureKind: ConversationCaptureInjected,
			}, false, true
		}
	}
	// Nothing filed, or a store af cannot name, or a record that shows no
	// first launch ran: start on id as before.
	return launch, conversation, false, false
}

// claudeStoreNameable reports whether the store the launch writes its
// transcripts to can be named at all — judgeRecordedClaude's first blind
// check, split out so the committed re-launch can tell "af cannot name the
// store" (nothing is known) from "a listing of it did not answer" (#7596).
func (i *Instance) claudeStoreNameable(program, workDir string) bool {
	configDir, launchDir, err := i.claudeStoreFor(program, workDir)
	return err == nil && strings.TrimSpace(configDir) != "" && strings.TrimSpace(launchDir) != ""
}

// replacementPanesStarted reports whether the committed swap's durable record
// says the replacement's panes were started — the first launch ran, so the
// committed id's transcript is filed wherever the store kept it.
func (i *Instance) replacementPanesStarted(account string) bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	pending := i.pendingAccountSwap
	return pending != nil && pending.To == account && pending.ReplacementPanesStarted
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

// pendingAccountSwapUnwrittenFor reports whether account's pending swap record
// was rewritten in memory since it last reached disk.
func (i *Instance) pendingAccountSwapUnwrittenFor(account string) bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.pendingAccountSwapUnwritten && i.pendingAccountSwap != nil && i.pendingAccountSwap.To == account
}

// markPendingAccountSwapWritten records that account's pending swap record has
// reached disk.
func (i *Instance) markPendingAccountSwapWritten(account string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.pendingAccountSwap != nil && i.pendingAccountSwap.To == account {
		i.pendingAccountSwapUnwritten = false
	}
}
