package daemon

import (
	"crypto/rand"
	"encoding/hex"
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
// PID/start stamp, boot scope, PID namespace, and a random nonce inherited by
// the live editor. All four process proofs must agree before a persisted handle
// can authorize a signal.
type vscodeOwnerRecord struct {
	Key          string `json:"key"`
	InstanceID   string `json:"instance_id"`
	PID          int    `json:"pid"`
	StartID      uint64 `json:"start_id"`
	BootID       string `json:"boot_id"`
	PIDNamespace string `json:"pid_namespace_id"`
	ProcessNonce string `json:"process_nonce"`
}

const (
	vscodeOwnerExt      = ".owner.json"
	vscodeOwnerNonceEnv = "AF_VSCODE_OWNER_NONCE"
)

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

// writeVSCodeOwnerForStart is the startup persistence seam. Production uses the
// atomic writer above; tests can pause it to prove the editor does not begin
// executing before its durable owner exists.
var writeVSCodeOwnerForStart = writeVSCodeOwner

func newVSCodeOwnerNonce() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generating editor ownership nonce: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func captureVSCodeOwner(key, instanceID string, pid int, processNonce string) (vscodeOwnerRecord, error) {
	if processNonce == "" {
		return vscodeOwnerRecord{}, fmt.Errorf("could not determine editor process ownership nonce")
	}
	bootID, err := proctree.BootID()
	if err != nil {
		return vscodeOwnerRecord{}, fmt.Errorf("could not determine kernel boot identity: %w", err)
	}
	pidNamespace, err := proctree.PIDNamespaceID()
	if err != nil {
		return vscodeOwnerRecord{}, fmt.Errorf("could not determine editor PID namespace: %w", err)
	}
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
		BootID: bootID, PIDNamespace: pidNamespace, ProcessNonce: processNonce,
	}, nil
}

// allPersistedVSCodeOwners returns only regular files with the exact owner-file
// shape. Callers decode and validate their contents before acting; the filename
// is discovery, never destructive authority.
func allPersistedVSCodeOwners() ([]string, error) {
	dir, err := vscodeSocketDirPath()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var paths []string
	for _, entry := range entries {
		if !vscodeOwnerNamePattern.MatchString(entry.Name()) {
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

func persistedVSCodeOwners(key string) ([]string, error) {
	paths, err := allPersistedVSCodeOwners()
	if err != nil {
		return nil, err
	}
	prefix := vscodeSocketKeyPrefix(key) + "-"
	kept := paths[:0]
	for _, path := range paths {
		if strings.HasPrefix(filepath.Base(path), prefix) {
			kept = append(kept, path)
		}
	}
	return kept, nil
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
	if owner.Key == "" || owner.InstanceID == "" || owner.PID <= 1 || owner.StartID == 0 ||
		owner.BootID == "" || owner.PIDNamespace == "" || owner.ProcessNonce == "" {
		return vscodeOwnerRecord{}, fmt.Errorf("invalid editor owner record")
	}
	return owner, nil
}

func verifyVSCodeOwnerNonce(owner vscodeOwnerRecord) (bool, error) {
	value, status := proctree.LookupEnv(owner.PID, vscodeOwnerNonceEnv)
	switch status {
	case proctree.EnvFound:
		return value == owner.ProcessNonce, nil
	case proctree.EnvAbsent:
		return false, nil
	default:
		return false, fmt.Errorf("could not determine editor ownership nonce for pid %d", owner.PID)
	}
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

func (v *vscodeSupervisor) processGroupAlive(pgid int) (bool, error) {
	if v.groupAlive != nil {
		return v.groupAlive(pgid)
	}
	return processGroupAlive(pgid)
}

func (v *vscodeSupervisor) waitForProcessGroupExit(pgid int, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		alive, err := v.processGroupAlive(pgid)
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
// PID. A live leader must match its boot scope, PID/start stamp, and inherited
// random nonce. If the leader is gone under a strong boot identity, POSIX pins
// the numeric PGID and prevents that PID from being reused while the group
// survives. Once the recorded leader is absent, however, a numeric group alone
// cannot prove continuous ownership: the old group may have emptied and that id
// may have been reused within the same boot. That case and any unreadable
// identity remain UNKNOWN, so neither is ever signalled.
func (v *vscodeSupervisor) stopPersistedOwner(owner vscodeOwnerRecord) error {
	pidNamespace, err := proctree.PIDNamespaceID()
	if err != nil {
		return fmt.Errorf("could not determine PID namespace for editor pid %d: %w", owner.PID, err)
	}
	if owner.PIDNamespace != pidNamespace {
		return nil
	}
	bootID, err := proctree.BootID()
	if err != nil {
		return fmt.Errorf("could not determine kernel boot identity for editor pid %d: %w", owner.PID, err)
	}
	if owner.BootID != bootID {
		// Linux start stamps are ticks since boot. A record from another boot
		// carries no authority over a PID in this one, even when both numeric
		// fields match; discarding it without signaling is the only safe result.
		return nil
	}
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
		matches, nonceErr := verifyVSCodeOwnerNonce(owner)
		if nonceErr != nil {
			return nonceErr
		}
		if !matches {
			return nil
		}
	} else {
		// Snapshot may omit a process it could not inspect. Distinguish a truly
		// absent leader from that unknown before treating the surviving group as
		// the pinned group recorded by the previous daemon.
		if err := syscall.Kill(owner.PID, 0); err == nil || !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("could not determine whether pid %d still has the recorded editor identity", owner.PID)
		}
		alive, groupErr := v.processGroupAlive(owner.PID)
		if groupErr != nil {
			return fmt.Errorf("could not determine whether editor process group %d survived: %w", owner.PID, groupErr)
		}
		if !alive {
			return nil
		}
		return fmt.Errorf("could not safely verify editor process group %d without its recorded leader", owner.PID)
	}

	signal := func(sig syscall.Signal) error { return syscall.Kill(-owner.PID, sig) }
	if v.killGroup != nil {
		signal = func(sig syscall.Signal) error { return v.killGroup(owner.PID, sig) }
	}
	if err := signal(syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("stopping previous editor process group %d: %w", owner.PID, err)
	}
	if gone, err := v.waitForProcessGroupExit(owner.PID, v.stopGrace); err != nil {
		return fmt.Errorf("confirming previous editor process group %d stopped: %w", owner.PID, err)
	} else if gone {
		return nil
	}
	// TERM may have removed the recorded leader while a numeric group with the
	// same id remains visible. Do not trust that number: the old group may have
	// emptied and the PGID may already belong to an unrelated process. Only the
	// exact live (boot, PID, StartID) leader pins the group for a safe escalation.
	pidNamespace, err = proctree.PIDNamespaceID()
	if err != nil {
		return fmt.Errorf("could not revalidate PID namespace before escalating editor pid %d: %w", owner.PID, err)
	}
	if pidNamespace != owner.PIDNamespace {
		return nil
	}
	bootID, err = proctree.BootID()
	if err != nil {
		return fmt.Errorf("could not revalidate kernel boot identity before escalating editor pid %d: %w", owner.PID, err)
	}
	if bootID != owner.BootID {
		return nil
	}
	current, identityErr := proctree.Lookup(owner.PID)
	if identityErr != nil {
		alive, groupErr := v.processGroupAlive(owner.PID)
		if groupErr != nil {
			return fmt.Errorf("could not determine whether editor process group %d survived before escalation: %w", owner.PID, groupErr)
		}
		if !alive {
			return nil
		}
		return fmt.Errorf("could not safely escalate editor process group %d after losing its recorded leader identity: %w", owner.PID, identityErr)
	}
	if current.StartID != owner.StartID {
		return nil
	}
	matches, nonceErr := verifyVSCodeOwnerNonce(owner)
	if nonceErr != nil {
		return nonceErr
	}
	if !matches {
		return nil
	}
	if err := signal(syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("killing previous editor process group %d: %w", owner.PID, err)
	}
	if gone, err := v.waitForProcessGroupExit(owner.PID, v.stopGrace); err != nil {
		return fmt.Errorf("confirming previous editor process group %d was killed: %w", owner.PID, err)
	} else if !gone {
		return fmt.Errorf("previous editor process group %d still exists after SIGKILL", owner.PID)
	}
	return nil
}

// stopPersistedForInstance reaps owner records for exactly one stable session
// id. The caller reserves key under v.mu before entering; process waits happen
// without the supervisor-wide mutex so unrelated editor operations stay live.
func (v *vscodeSupervisor) stopPersistedForInstance(key, instanceID string) error {
	_, err := v.reconcilePersistedForInstance(key, instanceID)
	return err
}

// reconcilePersistedForInstance additionally reports whether an owner for the
// exact stable instance was found. Callers recovering from an in-memory UNKNOWN
// may treat a matching, conclusively cleaned durable owner as the stronger
// result; a clean scan that found nothing is not evidence of cleanup.
func (v *vscodeSupervisor) reconcilePersistedForInstance(key, instanceID string) (bool, error) {
	paths, err := persistedVSCodeOwners(key)
	if err != nil {
		return false, err
	}
	matched := false
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
		matched = true
		if err := v.stopPersistedOwner(owner); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := removePersistedVSCodeOwner(path); err != nil {
			errs = append(errs, err)
		}
	}
	return matched, errors.Join(errs...)
}

func removePersistedVSCodeOwner(path string) error {
	var errs []error
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("removing stopped editor owner %s: %w", path, err))
	}
	socketPath := strings.TrimSuffix(path, vscodeOwnerExt) + vscodeSocketExt
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("removing stopped editor socket %s: %w", socketPath, err))
	}
	if err := os.Remove(vscodeStartGatePath(socketPath)); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("removing stopped editor start gate for %s: %w", socketPath, err))
	}
	return errors.Join(errs...)
}

// stopAllPersisted reaps editors that survived an earlier daemon before this
// supervisor finishes a graceful shutdown. At this point admission is closed
// and current in-memory servers have already stopped, so every remaining valid
// owner is stale daemon infrastructure rather than an active editor to preserve.
func (v *vscodeSupervisor) stopAllPersisted() error {
	paths, err := allPersistedVSCodeOwners()
	if err != nil {
		return err
	}
	var errs []error
	for _, path := range paths {
		owner, err := readVSCodeOwner(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("could not determine ownership from %s during shutdown: %w", path, err))
			continue
		}
		if err := v.stopPersistedOwner(owner); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := removePersistedVSCodeOwner(path); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
