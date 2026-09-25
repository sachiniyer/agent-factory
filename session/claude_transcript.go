package session

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

var claudeConversationFileRE = regexp.MustCompile(`(?i)^([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$`)

// ClaudeProjectConversationState is the on-disk evidence used to validate a
// recorded Claude conversation before a root-agent restore. Resume is the
// recorded conversation while its transcript exists, otherwise the newest
// project transcript; it is empty when the project has no transcript.
type ClaudeProjectConversationState struct {
	Resume         AgentConversationData
	RecordedExists bool
}

// InspectClaudeProjectConversations checks the exact transcript store selected
// by program and returns the newest direct transcript for workingDir. Claude
// encodes every non-alphanumeric character in an absolute project path as '-'.
func InspectClaudeProjectConversations(program, workingDir string, recorded AgentConversationData) (ClaudeProjectConversationState, error) {
	configDir, launchDir, err := claudeTranscriptLaunchContext(program, workingDir)
	if err != nil {
		return ClaudeProjectConversationState{}, err
	}
	// Claude names a project after the directory it was launched in. A symlinked
	// env -C chdir records the link as the launch directory, but on chdir the
	// kernel resolves it and Claude's getcwd reports the resolved path, so its
	// transcripts land under the resolved path's project name. Scan both
	// candidates, matching conversationCarry.locateClaude, so the verifier the
	// root-agent restore and live poller gate on finds the transcript the carry
	// feature already knew to look for.
	launchDirs := []string{launchDir}
	if resolved, err := filepath.EvalSymlinks(launchDir); err == nil && resolved != launchDir {
		launchDirs = append(launchDirs, resolved)
	}
	var transcripts []claudeTranscript
	for _, dir := range launchDirs {
		ts, err := claudeProjectTranscripts(filepath.Join(configDir, "projects", claudeProjectName(dir)))
		if err != nil {
			return ClaudeProjectConversationState{}, err
		}
		transcripts = append(transcripts, ts...)
	}
	conversation := func(id string) AgentConversationData {
		return AgentConversationData{
			Agent:       tmux.ProgramClaude,
			ID:          id,
			CapturedAt:  time.Now(),
			CaptureKind: ConversationCaptureClaudeTranscript,
		}
	}
	if recorded.Agent == tmux.ProgramClaude {
		for _, transcript := range transcripts {
			if strings.EqualFold(transcript.id, strings.TrimSpace(recorded.ID)) {
				return ClaudeProjectConversationState{Resume: conversation(transcript.id), RecordedExists: true}, nil
			}
		}
	}
	state := ClaudeProjectConversationState{}
	if newest, ok := newestClaudeTranscript(transcripts); ok {
		state.Resume = conversation(newest.id)
	}
	return state, nil
}

// claudeTranscript is one direct conversation file in a Claude project
// directory.
type claudeTranscript struct {
	id      string
	name    string
	modTime time.Time
}

// claudeProjectTranscripts lists projectDir's direct, regular conversation
// files. A missing directory has none.
func claudeProjectTranscripts(projectDir string) ([]claudeTranscript, error) {
	entries, err := os.ReadDir(projectDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var transcripts []claudeTranscript
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		match := claudeConversationFileRE.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		transcripts = append(transcripts, claudeTranscript{id: match[1], name: entry.Name(), modTime: info.ModTime()})
	}
	return transcripts, nil
}

// newestClaudeTranscript picks the most recently written transcript, breaking
// an exact tie by the greater file name.
func newestClaudeTranscript(transcripts []claudeTranscript) (claudeTranscript, bool) {
	var newest claudeTranscript
	found := false
	for _, transcript := range transcripts {
		if found && (transcript.modTime.Before(newest.modTime) ||
			(transcript.modTime.Equal(newest.modTime) && transcript.name < newest.name)) {
			continue
		}
		newest, found = transcript, true
	}
	return newest, found
}

func claudeTranscriptLaunchContext(command, workingDir string) (string, string, error) {
	launch, err := tmux.CommandEnvironmentFromCommand(command, workingDir)
	if err != nil {
		return "", "", err
	}
	if launch.Agent != tmux.ProgramClaude {
		return "", "", fmt.Errorf("command does not resolve to claude")
	}
	if !launch.WorkingDirKnown() {
		return "", "", fmt.Errorf("claude launch directory cannot be resolved statically")
	}
	effective := func(name string) (string, bool) {
		override := launch.Override(name)
		if !override.Present {
			value, set := os.LookupEnv(name)
			return value, set
		}
		return override.Value, override.Set
	}
	resolve := func(path string) string {
		if filepath.IsAbs(path) {
			return filepath.Clean(path)
		}
		return filepath.Clean(filepath.Join(launch.WorkingDir, path))
	}
	if configDir, set := effective("CLAUDE_CONFIG_DIR"); set && strings.TrimSpace(configDir) != "" {
		return resolve(configDir), launch.WorkingDir, nil
	}
	if home, set := effective("HOME"); set && strings.TrimSpace(home) != "" {
		return filepath.Join(resolve(home), ".claude"), launch.WorkingDir, nil
	}
	return "", "", os.ErrNotExist
}

func claudeProjectName(path string) string {
	path = filepath.Clean(path)
	units := utf16.Encode([]rune(path))
	var sanitized strings.Builder
	sanitized.Grow(len(units))
	var hash int32
	for _, unit := range units {
		if unit >= 'a' && unit <= 'z' || unit >= 'A' && unit <= 'Z' || unit >= '0' && unit <= '9' {
			sanitized.WriteByte(byte(unit))
		} else {
			sanitized.WriteByte('-')
		}
		// Claude's sanitizer uses JavaScript's 32-bit string hash over UTF-16
		// code units: hash = (hash << 5) - hash + charCode.
		hash = hash*31 + int32(unit)
	}
	name := sanitized.String()
	if len(name) <= 200 {
		return name
	}
	absHash := int64(hash)
	if absHash < 0 {
		absHash = -absHash
	}
	return name[:200] + "-" + strconv.FormatInt(absHash, 36)
}
