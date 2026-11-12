package session

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// codexConversationIDRe is the shape a Codex rollout UUID takes — the same
// spelling codexConversationIDFromPath extracts from a rollout filename.
var codexConversationIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidateSettableConversation proves an operator-supplied provider
// conversation id belongs to THIS session's live store and launch, and returns
// the record to write. It is the validation half of `af sessions
// set-conversation` (#6769): the repair for a conversation that was never
// recorded, or recorded stale, before the store had a capture arm on it.
//
// It fails closed at every step: a session whose store cannot be inspected, an
// id that is not a well-formed Codex UUID, a rollout that is absent, a first
// record that cannot be classified, a provable sub-agent thread, and a rollout
// whose launch directory is not this session's all refuse. Only the rollout's
// first session_meta line is ever read — prompt and event content is not
// scanned — and the running pane is never touched; the recorded id only
// matters the next time af builds a resume command.
func ValidateSettableConversation(i *Instance, id string) (AgentConversationData, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return AgentConversationData{}, fmt.Errorf("a conversation id is required")
	}
	if i.Capabilities().Workspace == WorkspaceRemote {
		return AgentConversationData{}, fmt.Errorf(
			"session %q runs off this host: its conversation store is not inspectable from here — run set-conversation on the host that owns the rollout store",
			i.Title)
	}
	// Bind to the command that launched the live runtime, not a re-resolution
	// of current config: a program_overrides edit after launch must not move
	// the store or directory this repair validates against — the same
	// evidence rule SessionCodexCaptureBaseline applies (#6769 review).
	program := i.RuntimeProgram()
	if strings.TrimSpace(program) == "" {
		resolution, err := resolveLaunchProgramForInstance(i)
		if err != nil {
			return AgentConversationData{}, fmt.Errorf("session %q: %w", i.Title, err)
		}
		program = resolution.command
	}
	// The provider is what the launch command resolves to, not the configured
	// enum: a program_overrides entry pointing another agent's name at codex,
	// or a legacy row whose Program IS the binary path, is a Codex session
	// whose rollout store af can prove. resolvedAgentLocked identifies the
	// same evidence for capture (#6769 review).
	if detected := tmux.DetectAgentFromCommand(program); detected != tmux.ProgramCodex {
		shown := detected
		if shown == "" {
			shown = i.AgentProgram()
		}
		if shown == "" {
			shown = "its agent"
		}
		return AgentConversationData{}, fmt.Errorf(
			"session %q uses %s: af can only inspect a Codex rollout store, so its conversation id cannot be set this way",
			i.Title, shown)
	}
	if !codexConversationIDRe.MatchString(id) {
		return AgentConversationData{}, fmt.Errorf(
			"%q is not a Codex conversation id (a UUID, as it appears in a rollout-…-<uuid>.jsonl filename)", id)
	}
	workDir := i.GetWorktreePath()
	home, err := i.codexHomeForLaunch(program, workDir)
	if err != nil {
		return AgentConversationData{}, fmt.Errorf("resolve the session's Codex store: %w", err)
	}
	if strings.TrimSpace(home) == "" {
		return AgentConversationData{}, fmt.Errorf(
			"session %q's Codex store cannot be located, so the id cannot be proven to belong to it", i.Title)
	}
	launchDir := LaunchCaptureWorkingDir(program, workDir)
	if launchDir == "" {
		return AgentConversationData{}, fmt.Errorf(
			"session %q's launch working directory cannot be determined, so a rollout cannot be proven to belong to it", i.Title)
	}
	launchDir = pathutil.ResolveForCompare(launchDir)
	files, err := codexRolloutFiles(home)
	if err != nil {
		return AgentConversationData{}, fmt.Errorf(
			"the session's Codex store at %s cannot be read: %w", home, err)
	}
	var rolloutPath string
	matches := 0
	for p := range files {
		if codexConversationIDFromPath(p) == strings.ToLower(id) {
			rolloutPath = p
			matches++
		}
	}
	switch {
	case matches > 1:
		// Two rollouts claim the id: the repair cannot prove which conversation
		// it names, and the restore guard refuses the same ambiguity — an
		// accepted record would be refused again at the next resume (#6769
		// review).
		return AgentConversationData{}, fmt.Errorf(
			"%s matches %d rollout files in the session's Codex store at %s — af cannot prove which conversation it names; remove the duplicate rollout, then retry", id, matches, home)
	case matches == 0:
		return AgentConversationData{}, fmt.Errorf(
			"no rollout named %s exists in the session's Codex store at %s — check the id with 'codex resume' there", id, home)
	}
	cwd, subagent, ok := codexRolloutSessionMeta(rolloutPath)
	switch {
	case !ok:
		return AgentConversationData{}, fmt.Errorf(
			"the rollout for %s cannot be read or classified (its first session_meta record is missing or incomplete)", id)
	case subagent:
		return AgentConversationData{}, fmt.Errorf(
			"rollout %s is a Codex sub-agent thread, not the session's own top-level conversation", id)
	case pathutil.ResolveForCompare(cwd) != launchDir:
		return AgentConversationData{}, fmt.Errorf(
			"rollout %s launched in %s, not in session %q's working directory %s — it belongs to a different session",
			id, cwd, i.Title, launchDir)
	}
	return AgentConversationData{
		Agent:       tmux.ProgramCodex,
		ID:          strings.ToLower(id),
		CapturedAt:  time.Now(),
		CaptureKind: ConversationCaptureOperator,
	}, nil
}
