package doctor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// Hook output is bounded by retained run count and age, not by bytes per run.
// Keep this advisory threshold in sync with docs/configuration.md.
const hookLogsWarnBytes int64 = 100 * 1024 * 1024

func checkHookLogs(report *Report, dir string) {
	blocking, target, err := hookLogBlockingPath(dir)
	if failHookLogPermission(report, dir, blocking, err) {
		return
	}
	if blocking != "" && (err == nil || errors.Is(err, syscall.ELOOP)) {
		detail := blocking + " is not a directory; configured hooks cannot start"
		remedy := "move or remove the file so af can create its hook log directory"
		if target != "" {
			detail = blocking + " is a dangling symlink to " + target + "; configured hooks cannot start"
			remedy = "fix or remove the link so af can create its hook log directory"
		}
		if errors.Is(err, syscall.ELOOP) {
			detail = blocking + " is a symlink loop; configured hooks cannot start"
			remedy = "fix or remove the link so af can create its hook log directory"
		}
		report.Fail(sectionConfig, "hook logs", detail, remedy)
		return
	}
	if err == nil {
		_, err = os.Stat(dir)
	}
	if os.IsNotExist(err) {
		report.Pass(sectionConfig, "hook logs", "not present; created on demand at "+dir)
		return
	}
	// Lookup/resolution errors concern creation until a nested scan says otherwise.
	creationPath := true
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
				creationPath = path == scanDir
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
				creationPath = path == scanDir
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
		if creationPath && failHookLogPermission(report, dir, "", err) {
			return
		}
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

// Inspect links before following them: ENOENT can mean either an absent path
// or a dangling link that MkdirAll cannot repair. Return the first blocking
// path and, for a dangling symlink, its target. Empty paths mean no obstruction;
// permission errors retain a blocking path; other inspection errors remain
// advisory incomplete scans rather than false PASSes.
func hookLogBlockingPath(dir string) (string, string, error) {
	for path := dir; ; path = filepath.Dir(path) {
		info, err := os.Lstat(path)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				info, err = os.Stat(path)
				if errors.Is(err, syscall.ELOOP) {
					return path, "", err
				}
				if os.IsNotExist(err) {
					target, readErr := os.Readlink(path)
					return path, target, readErr
				}
				if errors.Is(err, syscall.ENOTDIR) {
					return path, "", nil
				}
				if err != nil {
					return path, "", err
				}
			}
			if !info.IsDir() {
				return path, "", nil
			}
			// Creating logs needs write and search access to an existing leaf,
			// or to the nearest existing ancestor when the leaf is absent.
			// Resolve links so remediation names the directory to repair.
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return path, "", err
			}
			// Ask the kernel, honoring ACLs and root privileges.
			if err := unix.Access(resolved, unix.W_OK|unix.X_OK); err != nil {
				return resolved, "", err
			}
			return "", "", nil
		}
		// A lookup denied before reaching the leaf means its parent cannot
		// be traversed, so this prevents hooks from starting, not just scanning.
		if os.IsPermission(err) {
			return filepath.Dir(path), "", err
		}
		if (!os.IsNotExist(err) && !errors.Is(err, syscall.ENOTDIR) && !errors.Is(err, syscall.ELOOP)) || filepath.Dir(path) == path {
			return path, "", err
		}
	}
}

// Classify creation-path permissions at both error exits. Nested measurement
// failures bypass this classifier because they do not prevent new hook logs.
func failHookLogPermission(report *Report, dir, blocking string, err error) bool {
	if !errors.Is(err, fs.ErrPermission) && !errors.Is(err, syscall.EROFS) {
		return false
	}
	if blocking == "" {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			blocking = pathErr.Path
		}
		if blocking == "" {
			blocking = dir
		}
	}
	report.Fail(sectionConfig, "hook logs", blocking+" is not writable; af cannot create "+dir+" so configured hooks cannot start",
		"restore write and search access (chmod u+w "+blocking+"; chmod u+x "+blocking+") or fix directory ownership")
	return true
}
