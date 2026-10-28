package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// skillLedgerFile records every skill directory af has written an af-managed
// SKILL.md into. A launch's write root follows the command (program_overrides,
// env wrappers, #4501), so it can move between launches; the declined-consent
// cleanup cannot re-derive a root the operator has since changed away from, and
// without a record would leave af's marked file loading under it after consent
// was withdrawn (#4510 review).
const skillLedgerFile = "agent-skill-dirs.json"

var skillLedgerMu sync.Mutex

func skillLedgerPath() (string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, skillLedgerFile), nil
}

func readSkillLedger(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.WarningLog.Printf("af skill: could not read the managed-skill ledger %s (%v); cleanup covers only this launch's roots", path, err)
		}
		return nil
	}
	var dirs []string
	if err := json.Unmarshal(data, &dirs); err != nil {
		log.WarningLog.Printf("af skill: managed-skill ledger %s is malformed (%v); ignoring it", path, err)
		return nil
	}
	return dirs
}

func writeSkillLedger(path string, dirs []string) {
	sort.Strings(dirs)
	data, err := json.Marshal(dirs)
	if err != nil {
		return
	}
	if err := config.AtomicWriteFile(path, data, 0644); err != nil {
		log.WarningLog.Printf("af skill: could not update the managed-skill ledger %s: %v", path, err)
	}
}

// recordManagedSkillDir remembers skillDir as one af has written into.
func recordManagedSkillDir(skillDir string) {
	path, err := skillLedgerPath()
	if err != nil {
		return
	}
	skillLedgerMu.Lock()
	defer skillLedgerMu.Unlock()
	dirs := readSkillLedger(path)
	for _, d := range dirs {
		if d == skillDir {
			return
		}
	}
	writeSkillLedger(path, append(dirs, skillDir))
}

// cleanManagedSkillDirs removes af's skill from every recorded directory and
// forgets the ones that no longer hold a SKILL.md. A directory whose removal was
// refused (a user's own file, a symlink, an I/O error) stays recorded so a later
// opt-out retries it.
func cleanManagedSkillDirs() {
	path, err := skillLedgerPath()
	if err != nil {
		return
	}
	skillLedgerMu.Lock()
	defer skillLedgerMu.Unlock()
	dirs := readSkillLedger(path)
	if len(dirs) == 0 {
		return
	}
	kept := dirs[:0]
	for _, d := range dirs {
		file := filepath.Join(d, "SKILL.md")
		removeAfSkillDir(d, file)
		if _, err := os.Lstat(file); err == nil || !os.IsNotExist(err) {
			kept = append(kept, d)
		}
	}
	if len(kept) != len(dirs) {
		writeSkillLedger(path, kept)
	}
}
