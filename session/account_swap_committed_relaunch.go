package session

import (
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
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
// listed (#7596). It is a clause — the pending record's ResumeUnverifiedReason
// — that the notice and the log render after "because", and it names only the
// store's non-answer: never a claim that a listing af could not read proved
// the transcript (#7602 review).
const blindCommittedResumeReason = "its transcript store could not be listed"

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
// latched, so the retry answers at once, and in every case the retry gets only
// what is left of the first look's bound, so the two judges together never
// cost more than the one bound a single look was allowed (#7602 review) — and
// if the listing stays blind the record decides: only a record that PROVES
// Claude took an entry on id (the launched panes accepted the swap's prompt,
// committedConversationFiled) may resume it, since taking an entry is what
// files id's transcript and --resume then boots where a second --session-id
// id never does; blindResumed reports this so the record and the notice can
// say it. A record whose launch was never delivered to — the usual reason a
// committed re-launch happens — or whose launched id is another's describes
// no filing of id, so it starts on id exactly as it did before #7596.
//
// startOnUnfiled reports that a CLEAN listing found nothing filed under id —
// recordedClaudeStartOn — so the caller retires the record's delivered
// verdict: it proved a filing that no longer exists, and a later blind retry
// must not read it as proof Claude entered id (#7602 review). A blind start
// on id reports false — nothing contradicted the proof there.
func (i *Instance) committedConversationLaunch(program, workDir, account, id string) (launch string, conversation AgentConversationData, reminted, blindResumed, startOnUnfiled bool) {
	launch, conversation = planLaunchConversation(id, program)
	if !conversation.HasID() {
		// Not a Claude launch af names a conversation for.
		return launch, conversation, false, false, false
	}
	if !i.recordNamesAccount(account) {
		return launch, conversation, false, false, false
	}
	lookStarted := time.Now()
	verdict := i.judgeRecordedClaude(program, workDir, id)
	nameable := verdict == recordedClaudeBlind && i.claudeStoreNameable(program, workDir)
	if nameable {
		verdict = i.rejudgeRecordedClaude(program, workDir, id, lookStarted.Add(fsprobe.ProbeBound()))
	}
	switch verdict {
	case recordedClaudeAbandoned:
		launch, conversation = planLaunchConversation(newSessionID(), program)
		return launch, conversation, conversation.HasID(), false, false
	case recordedClaudeResume:
		if resumed, ok := tmux.ResumeProgramWithConversationID(program, tmux.ProgramClaude, id); ok {
			return resumed, AgentConversationData{
				Agent:       tmux.ProgramClaude,
				ID:          id,
				CapturedAt:  time.Now(),
				CaptureKind: ConversationCaptureInjected,
			}, false, false, false
		}
	}
	if verdict == recordedClaudeBlind && nameable && i.committedConversationFiled(account, id) {
		if resumed, ok := tmux.ResumeProgramWithConversationID(program, tmux.ProgramClaude, id); ok {
			return resumed, AgentConversationData{
				Agent:       tmux.ProgramClaude,
				ID:          id,
				CapturedAt:  time.Now(),
				CaptureKind: ConversationCaptureInjected,
			}, false, true, false
		}
	}
	// Nothing filed, or a store af cannot name, or a record that proves no
	// entry: start on id as before.
	return launch, conversation, false, false, verdict == recordedClaudeStartOn
}

// claudeStoreNameable reports whether the store the launch writes its
// transcripts to can be named at all — judgeRecordedClaude's first blind
// check, split out so the committed re-launch can tell "af cannot name the
// store" (nothing is known) from "a listing of it did not answer" (#7596).
func (i *Instance) claudeStoreNameable(program, workDir string) bool {
	configDir, launchDir, err := i.claudeStoreFor(program, workDir)
	return err == nil && strings.TrimSpace(configDir) != "" && strings.TrimSpace(launchDir) != ""
}

// committedConversationFiled reports whether the committed swap's durable
// record proves Claude took an entry on id — the only proof a blind re-launch
// may act on (#7596, #7602 review): the panes that launched on
// LaunchedConversationID accepted the swap's prompt (MissionDeliveryStatus ==
// PromptDelivered), and taking an entry is what files id's transcript. A pane
// that died before its first prompt filed nothing — the usual reason a
// committed re-launch happens — so a record without that delivery, whose
// panes never started, or whose committed id was reminted after the launch
// proves no filing of id and keeps the as-before start on it.
func (i *Instance) committedConversationFiled(account, id string) bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	pending := i.pendingAccountSwap
	return pending != nil && pending.To == account &&
		pending.LaunchedConversationID == id &&
		pending.MissionDeliveryStatus == PromptDelivered
}

// rejudgeRecordedClaude judges id again, giving the second look only what is
// left of the bound the first look began with (#7602 review): the two judges
// together cost no more than the one bound a single look was allowed, so a
// store whose listing errs slowly — an EIO after most of the bound — cannot
// make the re-launch wait twice. A second look still unanswered at the shared
// deadline is no look at all: its verdict is the blind verdict its own
// timed-out listing would have made. Its flights still finish under their own
// bounds — the re-launch just does not wait for them, the way fsprobe leaves
// any flight that outruns its deadline.
func (i *Instance) rejudgeRecordedClaude(program, workDir, id string, deadline time.Time) recordedClaudeVerdict {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return recordedClaudeBlind
	}
	done := make(chan recordedClaudeVerdict, 1)
	go func() { done <- i.judgeRecordedClaude(program, workDir, id) }()
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case verdict := <-done:
		return verdict
	case <-timer.C:
		return recordedClaudeBlind
	}
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
