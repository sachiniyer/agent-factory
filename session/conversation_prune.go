package session

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// PruneFileIndex maps recorded provider conversation ids to the transcript
// files they own, for `af sessions prune` (#5136). Provider stores name the
// files after the conversation id itself — Claude's projects/<dir>/<id>.jsonl
// and Codex's sessions/**/rollout-*-<id>.jsonl — so locating a session's
// captures does not need the (long-gone) launch worktree path at all: the
// recorded uuid IS the filename. The index is built lazily per agent and
// shared across a whole prune run, so an --all pass over thousands of
// archived rows scans each provider root once, not once per session.
//
// Candidate roots are the ambient provider home plus every registered account
// home for that agent (<AF home>/accounts/<agent>/*/) — the full set a
// conversation could live in after account swaps, which copy transcripts
// between homes while keeping the id.
type PruneFileIndex struct {
	afHome string
	agents map[string]*pruneAgentIndex
}

// pruneAgentIndex is one agent's lazily scanned transcript map. built marks
// the scan complete (including "no roots exist"), so a fleet with no codex
// sessions never pays the walk twice.
type pruneAgentIndex struct {
	built bool
	// warnsEmitted marks the scan warnings as already reported: every session
	// of one agent shares this index, so without it the same unreadable root
	// would warn once per row of a multi-thousand-row fleet.
	warnsEmitted bool
	// files is conversation id (lowercase) -> transcript file paths. A carried
	// conversation exists in TWO account homes, hence the slice.
	files map[string][]string
	// dirs is conversation id -> per-conversation auxiliary directories
	// (Claude's tool-result/subagent tree beside the transcript).
	dirs  map[string][]string
	warns []string
}

var claudeConversationDirRE = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func NewPruneFileIndex(afHome string) *PruneFileIndex {
	return &PruneFileIndex{afHome: afHome, agents: make(map[string]*pruneAgentIndex)}
}

// ambientPruneHome resolves the provider home a session that ran on the
// ambient (unaccounted) identity used. Mirrors the env-var contracts the
// launch path honors: CLAUDE_CONFIG_DIR for claude, CODEX_HOME for codex.
func ambientPruneHome(agent string) string {
	switch agent {
	case tmux.ProgramClaude:
		if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
			return dir
		}
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, ".claude")
		}
	case tmux.ProgramCodex:
		if dir := strings.TrimSpace(os.Getenv("CODEX_HOME")); dir != "" {
			return dir
		}
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, ".codex")
		}
	}
	return ""
}

// accountPruneHomes lists every registered account's provider home for agent:
// <afHome>/accounts/<agent>/<name>. Directory children only — anything else in
// there is not an account and not af's to walk.
func (idx *PruneFileIndex) accountPruneHomes(agent string) ([]string, []string) {
	parent := filepath.Join(idx.afHome, agentaccount.DirName, agent)
	entries, err := os.ReadDir(parent)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []string{fmt.Sprintf("cannot list %s accounts under %s: %v", agent, parent, err)}
	}
	var homes []string
	for _, entry := range entries {
		if entry.IsDir() {
			homes = append(homes, filepath.Join(parent, entry.Name()))
		}
	}
	return homes, nil
}

// agentIndex returns the agent's index, building it on first use.
func (idx *PruneFileIndex) agentIndex(agent string) *pruneAgentIndex {
	sub, ok := idx.agents[agent]
	if !ok {
		sub = &pruneAgentIndex{
			files: make(map[string][]string),
			dirs:  make(map[string][]string),
		}
		idx.agents[agent] = sub
	}
	if !sub.built {
		sub.built = true
		idx.buildAgentIndex(agent, sub)
	}
	return sub
}

func (idx *PruneFileIndex) buildAgentIndex(agent string, sub *pruneAgentIndex) {
	var roots []string
	if ambient := ambientPruneHome(agent); ambient != "" {
		roots = append(roots, ambient)
	}
	accounts, warns := idx.accountPruneHomes(agent)
	sub.warns = append(sub.warns, warns...)
	roots = append(roots, accounts...)
	for _, root := range roots {
		switch agent {
		case tmux.ProgramClaude:
			idx.scanClaudeRoot(root, sub)
		case tmux.ProgramCodex:
			idx.scanCodexRoot(root, sub)
		}
	}
}

// scanClaudeRoot indexes one Claude home's projects/<dir>/ tree. Filenames
// carry the conversation id outright; a sibling <id>/ directory is the
// conversation's auxiliary tree (tool results, subagent transcripts).
func (idx *PruneFileIndex) scanClaudeRoot(root string, sub *pruneAgentIndex) {
	projects := filepath.Join(root, "projects")
	projectDirs, err := os.ReadDir(projects)
	if err != nil {
		if !os.IsNotExist(err) {
			sub.warns = append(sub.warns, fmt.Sprintf("cannot list Claude projects under %s: %v", projects, err))
		}
		return
	}
	for _, projectDir := range projectDirs {
		if !projectDir.IsDir() {
			continue
		}
		dir := filepath.Join(projects, projectDir.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			sub.warns = append(sub.warns, fmt.Sprintf("cannot list Claude project %s: %v", dir, err))
			continue
		}
		for _, entry := range entries {
			path := filepath.Join(dir, entry.Name())
			if match := claudeConversationFileRE.FindStringSubmatch(entry.Name()); match != nil {
				sub.files[strings.ToLower(match[1])] = append(sub.files[strings.ToLower(match[1])], path)
				continue
			}
			if entry.IsDir() && claudeConversationDirRE.MatchString(entry.Name()) {
				sub.dirs[strings.ToLower(entry.Name())] = append(sub.dirs[strings.ToLower(entry.Name())], path)
			}
		}
	}
}

// scanCodexRoot indexes one Codex home's sessions/ and archived_sessions/
// trees. Rollouts are nested by date, so this is a recursive walk — the same
// layout codexRolloutFiles scans at capture time.
func (idx *PruneFileIndex) scanCodexRoot(root string, sub *pruneAgentIndex) {
	for _, store := range []string{"sessions", "archived_sessions"} {
		base := filepath.Join(root, store)
		_ = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				if path == base {
					return filepath.SkipDir
				}
				return nil
			}
			if d == nil || d.IsDir() {
				return nil
			}
			if id := codexConversationIDFromPath(path); id != "" {
				sub.files[id] = append(sub.files[id], path)
			}
			return nil
		})
	}
}

// PruneConversationFiles is the set of provider-side paths one session's
// recorded conversations own: transcript files plus Claude's per-conversation
// auxiliary directories.
type PruneConversationFiles struct {
	Files []string
	Dirs  []string
	// Bytes is the summed size of Files alone; Dirs are counted separately by
	// the caller's size pass, which walks each anyway for the worktree.
	Bytes int64
}

// FilesFor resolves the session's recorded conversations to on-disk paths.
// A session with no recorded conversation ids owns nothing af can identify —
// its provider files (if any) stay, which is the honest answer when the
// record cannot name them.
func (idx *PruneFileIndex) FilesFor(data InstanceData) (PruneConversationFiles, []string) {
	convs := RecordedConversations(data)
	var out PruneConversationFiles
	var warns []string
	for _, conv := range convs {
		switch conv.Agent {
		case tmux.ProgramClaude, tmux.ProgramCodex:
		default:
			warns = append(warns, fmt.Sprintf("conversation %s belongs to agent %q, whose transcript store af does not manage — left in place", conv.ID, conv.Agent))
			continue
		}
		sub := idx.agentIndex(conv.Agent)
		if !sub.warnsEmitted {
			sub.warnsEmitted = true
			warns = append(warns, sub.warns...)
		}
		id := strings.ToLower(strings.TrimSpace(conv.ID))
		out.Files = append(out.Files, sub.files[id]...)
		out.Dirs = append(out.Dirs, sub.dirs[id]...)
	}
	for _, path := range out.Files {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			out.Bytes += info.Size()
		}
	}
	return out, warns
}

// DeleteConversationFiles removes the paths FilesFor resolved, tolerating
// already-missing entries (a retry, or a file the provider moved between the
// scan and the apply). It returns only genuine problems as warnings: the
// session tombstone still lands, and the warning is what keeps an undeleted
// remainder visible rather than silent.
func DeleteConversationFiles(files PruneConversationFiles) []string {
	var warns []string
	for _, path := range files.Files {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			warns = append(warns, fmt.Sprintf("could not delete transcript %s: %v", path, err))
		}
	}
	for _, path := range files.Dirs {
		if err := os.RemoveAll(path); err != nil {
			warns = append(warns, fmt.Sprintf("could not delete conversation directory %s: %v", path, err))
		}
	}
	return warns
}
