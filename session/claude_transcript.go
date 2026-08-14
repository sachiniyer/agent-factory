package session

import (
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
// recorded Claude conversation before a root-agent restore. Latest is empty
// when the project has no transcript; RecordedExists reports the recorded ID
// independently so a caller can distinguish rotation from a missing carry.
type ClaudeProjectConversationState struct {
	Latest         AgentConversationData
	RecordedExists bool
}

// InspectClaudeProjectConversations checks the exact transcript store selected
// by program and returns the newest direct transcript for workingDir. Claude
// encodes every non-alphanumeric character in an absolute project path as '-'.
func InspectClaudeProjectConversations(program, workingDir string, recorded AgentConversationData) (ClaudeProjectConversationState, error) {
	configDir, err := tmux.ClaudeConfigDirFromCommand(program, workingDir)
	if err != nil {
		return ClaudeProjectConversationState{}, err
	}
	projectDir := filepath.Join(configDir, "projects", claudeProjectName(workingDir))
	entries, err := os.ReadDir(projectDir)
	if os.IsNotExist(err) {
		return ClaudeProjectConversationState{}, nil
	}
	if err != nil {
		return ClaudeProjectConversationState{}, err
	}

	state := ClaudeProjectConversationState{}
	var latestName string
	var latestModTime time.Time
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
			return ClaudeProjectConversationState{}, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		id := match[1]
		if recorded.Agent == tmux.ProgramClaude && strings.EqualFold(id, strings.TrimSpace(recorded.ID)) {
			state.RecordedExists = true
		}
		if latestName != "" && (info.ModTime().Before(latestModTime) ||
			(info.ModTime().Equal(latestModTime) && entry.Name() < latestName)) {
			continue
		}
		latestName = entry.Name()
		latestModTime = info.ModTime()
		state.Latest = AgentConversationData{
			Agent:       tmux.ProgramClaude,
			ID:          id,
			CapturedAt:  time.Now(),
			CaptureKind: ConversationCaptureClaudeTranscript,
		}
	}
	return state, nil
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
