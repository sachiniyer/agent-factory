package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// A supervisor's in-memory map dies with the daemon. The owner record is the
// cross-restart handle for the editor process group: stable session identity,
// PID, and the process start stamp that makes that PID safe to act on after a
// restart. It lives beside the private editor socket and shares its nonce.
type vscodeOwnerRecord struct {
	Key        string `json:"key"`
	InstanceID string `json:"instance_id"`
	PID        int    `json:"pid"`
	StartID    uint64 `json:"start_id"`
}

const vscodeOwnerExt = ".owner.json"

var vscodeOwnerNamePattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{8}\.owner\.json$`)

func vscodeOwnerPath(socketPath string) string {
	return strings.TrimSuffix(socketPath, vscodeSocketExt) + vscodeOwnerExt
}

func writeVSCodeOwner(path string, owner vscodeOwnerRecord) error {
	raw, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	return config.AtomicWriteFile(path, append(raw, '\n'), 0o600)
}

func captureVSCodeOwner(key, instanceID string, pid int) (vscodeOwnerRecord, error) {
	process, err := proctree.Lookup(pid)
	if err != nil {
		if errors.Is(err, proctree.ErrProcessExited) {
			// A child that is already a zombie is known to have exited, not an
			// unknown identity. Preserve the existing startup-exit sentinel so
			// the proxy renders its actionable notice and cooldowns the retry.
			return vscodeOwnerRecord{}, errVSCodeStartExited
		}
		return vscodeOwnerRecord{}, fmt.Errorf("could not determine editor process identity: %w", err)
	}
	if process.StartID == 0 {
		return vscodeOwnerRecord{}, fmt.Errorf("could not determine editor process identity for pid %d", pid)
	}
	return vscodeOwnerRecord{
		Key: key, InstanceID: instanceID, PID: pid, StartID: process.StartID,
	}, nil
}

// persistedVSCodeOwners returns only regular owner files whose hash prefix
// matches key. The full key and stable instance id are verified after decoding;
// the short filename hash is discovery, never destructive authority.
func persistedVSCodeOwners(key string) ([]string, error) {
	dir, err := vscodeSocketDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	prefix := vscodeSocketKeyPrefix(key) + "-"
	var paths []string
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) || !vscodeOwnerNamePattern.MatchString(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("stat editor owner %s: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		paths = append(paths, filepath.Join(dir, entry.Name()))
	}
	return paths, nil
}

func readVSCodeOwner(path string) (vscodeOwnerRecord, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return vscodeOwnerRecord{}, err
	}
	var owner vscodeOwnerRecord
	if err := json.Unmarshal(raw, &owner); err != nil {
		return vscodeOwnerRecord{}, err
	}
	if owner.Key == "" || owner.InstanceID == "" || owner.PID <= 1 || owner.StartID == 0 {
		return vscodeOwnerRecord{}, fmt.Errorf("invalid editor owner record")
	}
	return owner, nil
}

func processGroupAlive(pgid int) (bool, error) {
	err := syscall.Kill(-pgid, 0)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	default:
		return false, err
	}
}

func waitForProcessGroupExit(pgid int, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		alive, err := processGroupAlive(pgid)
		if err != nil || !alive {
			return !alive, err
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// stopPersistedOwner reaps a previous daemon's editor without trusting a bare
// PID. A matching (PID, StartID) proves a live leader is ours. If the leader is
// gone but its process group survives, POSIX pins the numeric PGID and prevents
// that PID from being reused, so an absent PID plus a live -PGID is also safe.
// Any unreadable live PID remains UNKNOWN and is never signalled.
func (v *vscodeSupervisor) stopPersistedOwner(owner vscodeOwnerRecord) error {
	snapshot, err := proctree.Snapshot()
	if err != nil {
		return fmt.Errorf("could not determine editor ownership for pid %d: %w", owner.PID, err)
	}
	if current, ok := snapshot[owner.PID]; ok {
		if current.StartID != owner.StartID {
			// PID reuse proves the old process group emptied first; POSIX does not
			// reuse a PID while a group with that numeric PGID still exists.
			return nil
		}
	} else {
		// Snapshot may omit a process it could not inspect. Distinguish a truly
		// absent leader from that unknown before treating the surviving group as
		// the pinned group recorded by the previous daemon.
		if err := syscall.Kill(owner.PID, 0); err == nil || !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("could not determine whether pid %d still has the recorded editor identity", owner.PID)
		}
		alive, groupErr := processGroupAlive(owner.PID)
		if groupErr != nil {
			return fmt.Errorf("could not determine whether editor process group %d survived: %w", owner.PID, groupErr)
		}
		if !alive {
			return nil
		}
	}

	signal := func(sig syscall.Signal) error { return syscall.Kill(-owner.PID, sig) }
	if v.killGroup != nil {
		signal = func(sig syscall.Signal) error { return v.killGroup(owner.PID, sig) }
	}
	if err := signal(syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("stopping previous editor process group %d: %w", owner.PID, err)
	}
	if gone, err := waitForProcessGroupExit(owner.PID, vscodeStopGrace); err != nil {
		return fmt.Errorf("confirming previous editor process group %d stopped: %w", owner.PID, err)
	} else if gone {
		return nil
	}
	if err := signal(syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("killing previous editor process group %d: %w", owner.PID, err)
	}
	if gone, err := waitForProcessGroupExit(owner.PID, vscodeStopGrace); err != nil {
		return fmt.Errorf("confirming previous editor process group %d was killed: %w", owner.PID, err)
	} else if !gone {
		return fmt.Errorf("previous editor process group %d still exists after SIGKILL", owner.PID)
	}
	return nil
}

// stopPersistedForInstanceLocked reaps owner records for exactly one stable
// session id. Caller holds v.mu, serializing this restart reconciliation against
// any new spawn for the same supervisor.
func (v *vscodeSupervisor) stopPersistedForInstanceLocked(key, instanceID string) error {
	paths, err := persistedVSCodeOwners(key)
	if err != nil {
		return err
	}
	var errs []error
	for _, path := range paths {
		owner, err := readVSCodeOwner(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("could not determine ownership from %s: %w", path, err))
			continue
		}
		if owner.Key != key || owner.InstanceID != instanceID {
			continue
		}
		if err := v.stopPersistedOwner(owner); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("removing stopped editor owner %s: %w", path, err))
		}
		socketPath := strings.TrimSuffix(path, vscodeOwnerExt) + vscodeSocketExt
		if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("removing stopped editor socket %s: %w", socketPath, err))
		}
	}
	return errors.Join(errs...)
}
