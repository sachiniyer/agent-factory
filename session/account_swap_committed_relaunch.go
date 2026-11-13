package session

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// committedConversationLaunch plans the re-launch of a committed replacement on
// the fresh conversation id its commit recorded (#7214). The first launch
// started Claude with --session-id id under the target account's store, and
// Claude files that conversation's transcript there. Once it has, a second
// --session-id id never boots: Claude refuses to start a new conversation on an
// id a transcript already holds. So the re-launch is judged against that store
// by the account swap's own staleness rule (claudeCarryStaleness), which the
// restore guard shares:
//
//   - nothing is filed under id: start on it, exactly as the first launch did;
//   - id is filed and still the newest conversation in the worktree: resume it,
//     since it is this session's own conversation and the swap's prompt went
//     to it;
//   - id is filed but a newer conversation exists (a /clear in the
//     replacement): the user moved on, and resuming would continue a thread
//     they left. Start fresh on a newly minted id instead, which the caller
//     records in id's place; reminted reports this.
//
// A store af cannot resolve or list says nothing about id, so the re-launch
// starts on it as before.
func committedConversationLaunch(program, workDir, account, id string) (launch string, conversation AgentConversationData, reminted bool) {
	launch, conversation = planLaunchConversation(id, program)
	if !conversation.HasID() {
		// Not a Claude launch af names a conversation for.
		return launch, conversation, false
	}
	configDir, launchDir, ok := committedClaudeStore(program, workDir, account)
	if !ok {
		return launch, conversation, false
	}
	projects := claudeProjectCandidates(launchDir)
	transcripts, err := claudeWorktreeTranscripts(configDir, projects)
	if err != nil {
		return launch, conversation, false
	}
	filed, found := newestClaudeTranscriptFor(transcripts, id)
	if !found {
		return launch, conversation, false
	}
	rel, err := filepath.Rel(configDir, filepath.Join(filed.dir, filed.name))
	if err != nil {
		return launch, conversation, false
	}
	switch claudeCarryStaleness(configDir, projects, rel, id) {
	case "":
		if resumed, ok := tmux.ResumeProgramWithConversationID(program, tmux.ProgramClaude, id); ok {
			return resumed, AgentConversationData{
				Agent:       tmux.ProgramClaude,
				ID:          id,
				CapturedAt:  time.Now(),
				CaptureKind: ConversationCaptureInjected,
			}, false
		}
	case staleCarryReason:
		launch, conversation = planLaunchConversation(newSessionID(), program)
		return launch, conversation, conversation.HasID()
	}
	return launch, conversation, false
}

// committedClaudeStore is the Claude store and launch directory a committed
// replacement's launch writes: the target account's registry directory, which
// the session environment hands the pane as CLAUDE_CONFIG_DIR, and the
// directory the command runs Claude in. ok is false when either cannot be
// named.
func committedClaudeStore(program, workDir, account string) (configDir, launchDir string, ok bool) {
	configDir, launchDir, err := claudeTranscriptLaunchContext(program, workDir)
	if strings.TrimSpace(account) != "" {
		selected, serr := selectAccountInNamespace(tmux.ProgramClaude, account)
		if serr != nil {
			return "", "", false
		}
		configDir = selected.Dir
		if err != nil {
			// The account names the store; only the launch directory is the
			// command's, and the worktree is where an unwrapped launch runs.
			launchDir, err = workDir, nil
		}
	}
	if err != nil || strings.TrimSpace(configDir) == "" || strings.TrimSpace(launchDir) == "" {
		return "", "", false
	}
	return configDir, launchDir, true
}

// newestClaudeTranscriptFor is the most recently written copy of conversation
// id among transcripts, which may list it under more than one project-directory
// spelling: the copy a resume by id continues.
func newestClaudeTranscriptFor(transcripts []claudeTranscript, id string) (claudeTranscript, bool) {
	var copies []claudeTranscript
	for _, transcript := range transcripts {
		if strings.EqualFold(transcript.id, strings.TrimSpace(id)) {
			copies = append(copies, transcript)
		}
	}
	return newestClaudeTranscript(copies)
}
