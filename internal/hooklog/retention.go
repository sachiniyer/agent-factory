package hooklog

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	keptLogLimit = 20
	keptLogAge   = 14 * 24 * time.Hour
	logGraceAge  = 5 * time.Second
)

// prune is best-effort housekeeping, never a prerequisite for starting a hook.
// Only recognized regular log files participate. Active descriptor locks,
// recent files, and the file Open just created are excluded from both deletion
// and the kept-log quota. Retention is checked
// on the next Open, not on a timer, and does not impose a byte cap on a run.
func prune(dir, opened string, now time.Time) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	byKind := make(map[Kind][]os.FileInfo)
	for _, entry := range entries {
		name := entry.Name()
		if filepath.Join(dir, name) == opened || !entry.Type().IsRegular() {
			continue
		}
		kind := logKind(name)
		if kind == "" {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || now.Sub(info.ModTime()) < logGraceAge {
			continue
		}
		// A quiet hook can be older than the grace period. Its inherited
		// descriptor lock, rather than output activity, proves it is live.
		file, err := lockKeptLog(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		_ = file.Close()
		byKind[kind] = append(byKind[kind], info)
	}
	var pruneErr error
	pruned := 0
	for _, kind := range []Kind{PostWorktree, OnArchive} {
		logs := byKind[kind]
		sort.Slice(logs, func(i, j int) bool {
			if logs[i].ModTime().Equal(logs[j].ModTime()) {
				return logs[i].Name() < logs[j].Name()
			}
			return logs[i].ModTime().After(logs[j].ModTime())
		})
		for i, info := range logs {
			if i < keptLogLimit && now.Sub(info.ModTime()) <= keptLogAge {
				continue
			}
			removed, err := removeKeptLog(filepath.Join(dir, info.Name()), info, now)
			if removed {
				pruned++
			}
			if err != nil {
				pruneErr = errors.Join(pruneErr, err)
			}
		}
	}
	return pruned, pruneErr
}

func logKind(name string) Kind {
	for _, kind := range []Kind{PostWorktree, OnArchive} {
		prefix := string(kind) + "-"
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".log") && len(name) > len(prefix)+len(".log") {
			return kind
		}
	}
	return ""
}
