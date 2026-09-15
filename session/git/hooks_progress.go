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
	leaseMu             *sync.Mutex
	lease               *os.File // Local runner only; never serialized or inherited by children.
	leaseHolds          int
	supersededDirectory string
	SessionID           string   `json:"session_id"`
	Commands            []string `json:"commands"`
	Passthrough         []string `json:"passthrough"`
	Worktree            string   `json:"worktree"`
	Prefix              string   `json:"scope_prefix"`
	Generation          string   `json:"generation"`
	Directory           string   `json:"directory"`
	// The checkout's Git administrative directory carries a random identity
	// that a later checkout at the same path cannot derive or inherit.
	WorktreeIdentity *hookWorktreeIdentity `json:"worktree_identity,omitempty"`
	// ResumeDisabled is positive publication-time evidence that identity
	// recording failed. Unlike a legacy tokenless journal, restore may safely
	// treat this list as unresumable instead of installing a permanent retry.
	ResumeDisabled bool `json:"resume_disabled,omitempty"`
	// PublicationVersion and ResumeReady form a positive recovery commit.
	// Version-one journals are written with ResumeReady=0 before their shared
	// name is synced, then the same durable inode is flipped to 1. A failed
	// publication therefore cannot become resumable if its rollback rename is
	// lost in a host crash. Version zero preserves legacy fail-closed handling.
	PublicationVersion int `json:"publication_version,omitempty"`
	ResumeReady        int `json:"resume_ready"`
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
	// Identity recording and owner discovery happen before the home-wide lock.
	// Identity failure must not veto the operator-requested run. Persist its
	// explicit downgrade so restore observes any survivor but never installs an
	// identity retry that cannot succeed from this immutable journal.
	checkoutIdentity, identityErr := boundedRecordHookWorktreeIdentity(run.repoPath, run.worktreePath)
	resumeDisabled := identityErr != nil || checkoutIdentity == nil
	if resumeDisabled {
		if identityErr == nil {
			identityErr = fmt.Errorf("checkout identity recorder returned no identity")
		}
		log.WarningLog.Printf("post-worktree hook journal for %s is not resumable: %v; the current run will continue, but an interrupted suffix cannot be restarted safely", run.worktreePath, identityErr)
	}
	ownerSnapshot, ownersErr := boundedHookProgressOwners()
	pruneNow := time.Now()
	var candidates []keptProgress
	var cleanups []hookProgressCleanup
	var progress *hookProgress
	var activation *preparedHookProgress
	var rollback *hookProgressPublicationRollback
	err = withHookProgressLock(filepath.Dir(path), func(dir string, identity os.FileInfo) error {
		if ownersErr == nil && dir != ownerSnapshot.hookDirectory {
			ownersErr = fmt.Errorf("hook owner snapshot belongs to %s, not locked directory %s", ownerSnapshot.hookDirectory, dir)
		}
		if ownersErr == nil {
			candidates, ownersErr = collectHookProgressCandidatesLocked(dir, pruneNow, ownerSnapshot.owners, &cleanups)
		}
		var publishErr error
		progress, activation, rollback, publishErr = publishHookProgress(run, commands, prefix, generation, filepath.Join(dir, filepath.Base(path)), identity, checkoutIdentity, resumeDisabled)
		return publishErr
	})
	if rollback != nil {
		err = errors.Join(err, rollback.cleanup())
	}
	if errors.Is(err, config.ErrLockTimeout) {
		return nil, fmt.Errorf("hook journal lock held by another process; hooks could not start; retry once the holder releases it: %w", err)
	}
	if err == nil && activation != nil {
		if activateErr := activation.enableResume(); activateErr != nil {
			log.WarningLog.Printf("post-worktree hook journal for %s could not confirm resumable publication: %v; the current run will continue without relying on restart recovery", run.worktreePath, activateErr)
		}
	}
	if ownersErr == nil {
		ownersErr = finishHookProgressPrune(filepath.Dir(path), pruneNow, ownerSnapshot, candidates, cleanups)
	} else {
		ownersErr = errors.Join(ownersErr, cleanupHookProgressArtifacts(cleanups))
	}
	if ownersErr != nil {
		log.WarningLog.Printf("cannot prune inactive hook journals: %v", ownersErr)
	}
	if err == nil && progress != nil {
		cleanupSupersededHookProgress(progress.supersededDirectory)
	}
	return progress, err
}

// Publication and standalone pruning share the existing identity-probe budget.
// Journal construction, optional reads, lease inspection, and directory
// durability barriers reached by the callback are bounded. Atomic namespace
// mutations remain synchronous: abandoning one in a worker could publish after
// the caller had released this lock and rejected the transaction. Teardown
// remains nonblocking through TryWithFileLock in the retirement path.
func withHookProgressLock(dir string, fn func(string, os.FileInfo) error) error {
	pinned, err := boundedResolveForCompare(dir)
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
	return withBoundedHookProgressFileLock(filepath.Join(pinned, ".progress"), relocationIdentityTimeout, func() error {
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
func publishHookProgress(run hookRun, commands []string, prefix, generation, path string, parentIdentity os.FileInfo, worktreeIdentity *hookWorktreeIdentity, resumeDisabled bool) (*hookProgress, *preparedHookProgress, *hookProgressPublicationRollback, error) {
	prepared, err := boundedPrepareHookProgress(run, commands, prefix, generation, path, worktreeIdentity, resumeDisabled)
	if err != nil {
		return nil, nil, nil, err
	}
	p := prepared.progress
	rollback := &hookProgressPublicationRollback{prepared: prepared, path: path}
	var previous hookProgress
	if data, readErr := previousHookProgressReadFile(path); readErr == nil {
		_ = json.Unmarshal(data, &previous)
	}
	currentParent, err := BoundedLstat(filepath.Dir(path))
	if err != nil {
		return nil, nil, rollback, err
	}
	if !currentParent.IsDir() || !os.SameFile(parentIdentity, currentParent) {
		return nil, nil, rollback, fmt.Errorf("hook journal directory changed while publication lock was held")
	}
	if err := os.Rename(prepared.temporary, path); err != nil {
		return nil, nil, rollback, err
	}
	rollback.renamed = true
	if err := boundedSyncHookProgressDirectory(filepath.Dir(path)); err != nil {
		return nil, nil, rollback, fmt.Errorf("sync published hook journal directory: %w", err)
	}
	if filepath.Dir(previous.Directory) == filepath.Dir(path) && strings.HasPrefix(filepath.Base(previous.Directory), "entries-") && previous.Directory != p.Directory {
		p.supersededDirectory = previous.Directory
	}
	return p, prepared, nil, nil
}

func (p *hookProgress) receipt(index int) string {
	return filepath.Join(p.Directory, strconv.Itoa(index))
}

func (p *hookProgress) claimed(index int) bool {
	_, err := os.Stat(p.receipt(index))
	return err == nil
}

// mkdir is an atomic claim. It also protects against a delayed launcher and a
// successor racing: only one shell can execute the entry. The scoped wrapper is
// Linux-only, so coreutils sync fsyncs the receipt parent before the operator
// command starts; failure stops without executing the command. Arguments keep
// shell source, filenames and command text separate. A receipt directory means
// started; its exit file means the shell finished, even if its daemon died.
// hookStopTimeout is already the bound for uncertain hook-scope ownership. Use
// that same budget here, rounded up to whole seconds because POSIX sleep only
// specifies integer operands. The winner publishes its short status through a
// same-directory rename, so a successor cannot observe a partial receipt.
func (p *hookProgress) command(index int, command string) []string {
	return []string{"-c", `if [ -d "$1" ]; then
	remaining=$4
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
sync "$3" || exit 125
sh -c "$2"
status=$?
exit_tmp=$1/.exit-$$
printf '%s\n' "$status" > "$exit_tmp" || exit 125
mv "$exit_tmp" "$1/exit" || exit 125
exit "$status"`, "af-hook-entry", p.receipt(index), command, p.Directory, hookReceiptWaitSeconds()}
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
	p.releaseLeaseWith(closeHookProgressFile)
}

func (p *hookProgress) releaseLeaseWith(closeFile func(*os.File)) {
	if p.leaseMu == nil {
		if p.lease != nil {
			unlockAndCloseHookProgressFileWith(p.lease, closeFile)
			p.lease = nil
		}
		p.leaseHolds = 0
		return
	}
	p.leaseMu.Lock()
	if p.leaseHolds > 1 {
		p.leaseHolds--
		p.leaseMu.Unlock()
		return
	}
	lease := p.lease
	p.lease = nil
	p.leaseHolds = 0
	p.leaseMu.Unlock()
	unlockAndCloseHookProgressFileWith(lease, closeFile)
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
	return boundedMarkHookProgressFinished(filepath.Join(p.Directory, "finished"))
}
