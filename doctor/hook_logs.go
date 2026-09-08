package doctor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Hook output is bounded by retained run count and age, not by bytes per run.
// Keep this advisory threshold in sync with docs/configuration.md.
const hookLogsWarnBytes int64 = 100 * 1024 * 1024

func checkHookLogs(report *Report, dir string) {
	info, err := os.Stat(dir)
	if os.IsNotExist(err) {
		report.Pass(sectionConfig, "hook logs", "not present; created on demand at "+dir)
		return
	}
	if (err == nil && !info.IsDir()) || errors.Is(err, syscall.ENOTDIR) {
		report.Fail(sectionConfig, "hook logs", hookLogBlockingPath(dir)+" is not a directory; configured hooks cannot start",
			"move or remove the file so af can create its hook log directory")
		return
	}
	var total int64
	var scanDir string
	if err == nil {
		// Hook creation follows directory symlinks at the root. Resolve that
		// root for WalkDir, which otherwise visits only the link itself.
		// Symlinks encountered inside the measured tree remain excluded.
		scanDir, err = filepath.EvalSymlinks(dir)
	}
	if err == nil {
		err = filepath.WalkDir(scanDir, func(path string, entry fs.DirEntry, walkErr error) error {
			if os.IsNotExist(walkErr) {
				return nil // A completed hook or concurrent retention pass removed it.
			}
			if walkErr != nil {
				return walkErr
			}
			if !entry.Type().IsRegular() {
				return nil // Do not follow symlinks or count directory metadata.
			}
			info, err := entry.Info()
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				total += info.Size()
			}
			return nil
		})
	}
	if err != nil {
		report.markIncomplete("hook logs")
		report.Warn(sectionConfig, "hook logs", fmt.Sprintf("cannot measure %s: %v", dir, err),
			"check the hook log directory and its permissions, then rerun `af doctor`", false)
		return
	}
	detail := fmt.Sprintf("%s: %d bytes of hook logs", dir, total)
	if total > hookLogsWarnBytes {
		report.Warn(sectionConfig, "hook logs", detail+" (exceeds 100 MiB)",
			"inspect failed hook output and fix recurring hook failures; remove unneeded logs only after their hooks have stopped (automatic retention runs when a hook opens its next log)", false)
		return
	}
	report.Pass(sectionConfig, "hook logs", detail)
}

// ENOTDIR may identify a blocked ancestor rather than the leaf. The first
// existing non-directory on the way up is the path the operator must fix.
func hookLogBlockingPath(dir string) string {
	for path := dir; ; path = filepath.Dir(path) {
		info, err := os.Stat(path)
		if err == nil {
			if !info.IsDir() {
				return path
			}
			return dir
		}
		if !errors.Is(err, syscall.ENOTDIR) || filepath.Dir(path) == path {
			return dir
		}
	}
}
