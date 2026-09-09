package git

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// hookProgress snapshots the list before its first launch. Entry receipts are
// written by the scoped shell, not its daemon, so a daemon exit cannot lose the
// index of a command that actually started. Never replay a claimed command:
// arbitrary provisioning commands need not be idempotent.
type hookProgress struct {
	leaseMu     *sync.Mutex
	lease       *os.File // Local runner only; never serialized or inherited by children.
	leaseHolds  int
	SessionID   string   `json:"session_id"`
	Commands    []string `json:"commands"`
	Passthrough []string `json:"passthrough"`
	Worktree    string   `json:"worktree"`
	Prefix      string   `json:"scope_prefix"`
	Generation  string   `json:"generation"`
	Directory   string   `json:"directory"`
	// The linked worktree's .git pointer file survives an ordinary rename but
	// gets a new device/inode identity when a different worktree replaces it.
	// Resume therefore requires this positive identity, not merely the absence
	// of evidence that the path changed.
	WorktreeIdentity *hookWorktreeIdentity `json:"worktree_identity,omitempty"`
}

func hookProgressPath(worktree string) (string, error) {
	home, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "logs", "hooks")
	return filepath.Join(dir, "progress-"+worktreePathScopeIdentity(worktree)+".json"), nil
}

func newHookProgress(run hookRun, commands []string, prefix, generation string) (*hookProgress, error) {
	path, err := hookProgressPath(run.worktreePath)
	if err != nil {
		return nil, err
	}
	if err := config.MkdirAllUnderAFHome(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	var progress *hookProgress
	err = withHookProgressLock(filepath.Dir(path), func(dir string, identity os.FileInfo) error {
		// Share one acquisition budget for GC and publication; a contended home
		// must not pay the timeout twice before reporting that hooks could not start.
		if err := pruneHookProgressLocked(dir, time.Now()); err != nil {
			log.WarningLog.Printf("cannot prune inactive hook journals: %v", err)
		}
		var publishErr error
		progress, publishErr = publishHookProgress(run, commands, prefix, generation, filepath.Join(dir, filepath.Base(path)), identity)
		return publishErr
	})
	if errors.Is(err, config.ErrLockTimeout) {
		return nil, fmt.Errorf("hook journal lock held by another process; hooks could not start; retry once the holder releases it: %w", err)
	}
	return progress, err
}

// Publication and standalone pruning share the existing identity-probe budget.
// Teardown remains nonblocking through TryWithFileLock in the retirement path.
func withHookProgressLock(dir string, fn func(string, os.FileInfo) error) error {
	pinned, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("resolve hook journal directory: %w", err)
	}
	identity, err := BoundedLstat(pinned)
	if err != nil {
		return fmt.Errorf("identify hook journal directory: %w", err)
	}
	if !identity.IsDir() {
		return fmt.Errorf("hook journal path is not a directory: %s", pinned)
	}
	return config.WithFileLockTimeout(filepath.Join(pinned, ".progress"), relocationIdentityTimeout, func() error {
		hookProgressLockAcquired()
		return fn(pinned, identity)
	})
}

var hookProgressLockAcquired = func() {}

// Kept separate from the journal reader so publication can inspect the
// superseded receipt directory without changing the journal being published.
var previousHookProgressReadFile = BoundedReadFile

// The directory and journal are published under the same lock used by pruning,
// so even a publisher stalled longer than the grace period retains its receipts.
func publishHookProgress(run hookRun, commands []string, prefix, generation, path string, parentIdentity os.FileInfo) (*hookProgress, error) {
	dir, err := os.MkdirTemp(filepath.Dir(path), "entries-")
	if err != nil {
		return nil, err
	}
	var lease *os.File
	published := false
	defer func() {
		if !published {
			if lease != nil {
				_ = lease.Close()
			}
			_ = os.RemoveAll(dir)
		}
	}()
	if run.leaseProgress {
		lease, err = newHookProgressLease(dir)
		if err != nil {
			return nil, err
		}
	}
	p := &hookProgress{
		lease:     lease,
		SessionID: run.scopeSessionID, Commands: commands, Passthrough: run.passthrough, Worktree: run.worktreePath,
		Prefix: prefix, Generation: generation, Directory: dir,
	}
	p.WorktreeIdentity, err = readHookWorktreeIdentity(run.worktreePath)
	if err != nil && run.repoPath != "" {
		return nil, fmt.Errorf("record linked worktree identity for hook journal: %w", err)
	}
	if lease != nil {
		p.leaseMu = &sync.Mutex{}
		p.leaseHolds = 1
	}
	data, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".progress-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	var previous hookProgress
	if data, readErr := previousHookProgressReadFile(path); readErr == nil {
		_ = json.Unmarshal(data, &previous)
	}
	currentParent, err := BoundedLstat(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if !currentParent.IsDir() || !os.SameFile(parentIdentity, currentParent) {
		return nil, fmt.Errorf("hook journal directory changed while publication lock was held")
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return nil, err
	}
	published = true
	if filepath.Dir(previous.Directory) == filepath.Dir(path) && strings.HasPrefix(filepath.Base(previous.Directory), "entries-") && previous.Directory != p.Directory {
		if _, statErr := os.Stat(filepath.Join(previous.Directory, "finished")); statErr == nil {
			_ = os.RemoveAll(previous.Directory)
		}
	}
	return p, nil
}

func (p *hookProgress) receipt(index int) string {
	return filepath.Join(p.Directory, strconv.Itoa(index))
}

func (p *hookProgress) claimed(index int) bool {
	_, err := os.Stat(p.receipt(index))
	return err == nil
}

func (p *hookProgress) entryFinished(index int) bool {
	info, err := os.Stat(filepath.Join(p.receipt(index), "exit"))
	return err == nil && info.Mode().IsRegular()
}

// mkdir is an atomic claim. It also protects against a delayed launcher and a
// successor racing: only one shell can execute the entry. Arguments keep shell
// source, filenames and command text separate. A receipt directory means
// started; its exit file means the shell finished, even if its daemon died.
// hookStopTimeout is already the bound for uncertain hook-scope ownership. Use
// that same budget here, rounded up to whole seconds because POSIX sleep only
// specifies integer operands. The winner publishes its short status through a
// same-directory rename, so a successor cannot observe a partial receipt.
func (p *hookProgress) command(index int, command string) []string {
	return []string{"-c", `if [ -d "$1" ]; then
	remaining=$3
	while :; do
		if [ -f "$1/exit" ]; then
			if IFS= read -r status < "$1/exit"; then
				case "$status" in ''|*[!0-9]*) ;; *) exit "$status" ;; esac
			fi
		fi
		if [ "$remaining" -le 0 ]; then exit 125; fi
		sleep 1 || exit 125
		remaining=$((remaining - 1))
	done
fi
mkdir -- "$1" || exit 125
sh -c "$2"
status=$?
exit_tmp=$1/.exit-$$
printf '%s\n' "$status" > "$exit_tmp" || exit 125
mv "$exit_tmp" "$1/exit" || exit 125
exit "$status"`, "af-hook-entry", p.receipt(index), command, hookReceiptWaitSeconds()}
}

func hookReceiptWaitSeconds() string {
	seconds := hookStopTimeout / time.Second
	if hookStopTimeout%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}
	return strconv.FormatInt(int64(seconds), 10)
}

func (p *hookProgress) finish() {
	// Publish terminal evidence before releasing the local runner's lease.
	if err := p.markFinished(); err != nil {
		log.ErrorLog.Printf("cannot record post-worktree hook completion: %v", err)
	}
	p.releaseLease()
}

func (p *hookProgress) releaseLease() {
	if p.leaseMu == nil {
		if p.lease != nil {
			_ = p.lease.Close()
			p.lease = nil
		}
		p.leaseHolds = 0
		return
	}
	p.leaseMu.Lock()
	defer p.leaseMu.Unlock()
	if p.leaseHolds > 1 {
		p.leaseHolds--
		return
	}
	if p.lease != nil {
		_ = p.lease.Close()
		p.lease = nil
	}
	p.leaseHolds = 0
}

func (p *hookProgress) retainLease() bool {
	if p.leaseMu == nil {
		return false
	}
	p.leaseMu.Lock()
	defer p.leaseMu.Unlock()
	if p.lease == nil || p.leaseHolds == 0 {
		return false
	}
	p.leaseHolds++
	return true
}

// Callers that authorize teardown must observe failure to persist cancellation.
func (p *hookProgress) markFinished() error {
	return hookProgressMarkFinished(filepath.Join(p.Directory, "finished"), nil, 0600)
}

var hookProgressMarkFinished = os.WriteFile
