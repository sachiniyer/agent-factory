package git

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// hookProgress snapshots the list before its first launch. Entry receipts are
// written by the scoped shell, not its daemon, so a daemon exit cannot lose the
// index of a command that actually started. Never replay a claimed command:
// arbitrary provisioning commands need not be idempotent.
type hookProgress struct {
	lease       *os.File // Local runner only; never serialized or inherited by children.
	SessionID   string   `json:"session_id"`
	Commands    []string `json:"commands"`
	Passthrough []string `json:"passthrough"`
	Worktree    string   `json:"worktree"`
	Prefix      string   `json:"scope_prefix"`
	Generation  string   `json:"generation"`
	Directory   string   `json:"directory"`
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
	pruneHookProgress(filepath.Dir(path), time.Now())
	var progress *hookProgress
	err = config.WithFileLock(filepath.Join(filepath.Dir(path), ".progress"), func() error {
		var publishErr error
		progress, publishErr = publishHookProgress(run, commands, prefix, generation, path)
		return publishErr
	})
	return progress, err
}

// The directory and journal are published under the same lock used by pruning,
// so even a publisher stalled longer than the grace period retains its receipts.
func publishHookProgress(run hookRun, commands []string, prefix, generation, path string) (*hookProgress, error) {
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
	if data, readErr := os.ReadFile(path); readErr == nil {
		_ = json.Unmarshal(data, &previous)
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

// mkdir is an atomic claim. It also protects against a delayed launcher and a
// successor racing: only one shell can execute the entry. Arguments keep shell
// source, filenames and command text separate. A receipt directory means
// started; its exit file means the shell finished, even if its daemon died.
func (p *hookProgress) command(index int, command string) []string {
	return []string{"-c", `if [ -d "$1" ]; then exit 0; fi
mkdir -- "$1" || exit 125
sh -c "$2"
status=$?
printf '%s\n' "$status" > "$1/exit"
exit "$status"`, "af-hook-entry", p.receipt(index), command}
}

func (p *hookProgress) finish() {
	// Publish terminal evidence before releasing the local runner's lease.
	if p.lease != nil {
		defer p.lease.Close()
	}

	if err := os.WriteFile(filepath.Join(p.Directory, "finished"), nil, 0600); err != nil {
		log.ErrorLog.Printf("cannot record post-worktree hook completion: %v", err)
	}
}
