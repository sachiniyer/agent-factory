package move

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Host is a move destination.
type Host struct {
	Name string
	// SSH is the ssh command af uses for the host WITHOUT the destination
	// (options only); SSHDest is the destination. Both are empty for a free-form
	// sandbox.ssh command that embeds the host, which sets SSHFullCmd instead.
	SSH, SSHDest string
	// SSHFullCmd is the complete command when it cannot be split; otherwise it is
	// derived as SSH + " " + SSHDest.
	SSHFullCmd string
	// MoveCmd is the operator's move script command line; empty falls back to
	// the global default, and empty again means no script.
	MoveCmd string
}

// HostLookup resolves a destination name. It is the single seam W6.1 (the host
// list) replaces; until then DefaultHostLookup serves the one configured
// sandbox.
type HostLookup func(name string) (Host, error)

func (h Host) fullSSH() string {
	if h.SSHFullCmd != "" {
		return h.SSHFullCmd
	}
	return strings.TrimSpace(h.SSH + " " + h.SSHDest)
}

// Session is the part of a live local session a move needs.
type Session struct {
	ID, Title, Branch, Worktree, Account string
}

// Steps is every effect a move has. The orchestrator owns only ordering and
// abort; implementations own the daemon, storage and sandbox.
type Steps interface {
	// Quiesce waits for the agent to go idle (or proceeds at once when force).
	Quiesce(ctx context.Context, s Session, force bool) error
	// Provision creates the sandbox workspace seeded from snap (not a fresh
	// clone) and returns its workspace dir plus a reap that removes the
	// half-built or unwanted sandbox by its persisted identity.
	Provision(ctx context.Context, s Session, h Host, snap *Snapshot) (workspace string, reap func() error, err error)
	// VerifyDestination runs snap.VerifyCommand in the destination workspace and
	// returns its output; the orchestrator compares it with the snapshot's tree
	// (Snapshot.CheckTree) before anything else happens to the move.
	VerifyDestination(ctx context.Context, s Session, h Host, snap *Snapshot) (string, error)
	// SyncAccount is the W7.3 seam (account sync + credential push, W7.8).
	SyncAccount(ctx context.Context, s Session, h Host) error
	// StartAgent starts the agent in the sandbox. It resumes the same
	// conversation once W7.2 lands (the W7.2 seam), and returns only when the
	// agent is confirmed up.
	StartAgent(ctx context.Context, s Session, h Host) error
	// SwitchRow atomically repoints the session row at the sandbox backend.
	SwitchRow(ctx context.Context, s Session, h Host) error
	// ArchiveLocal archives (restorably) the local worktree.
	ArchiveLocal(ctx context.Context, s Session) error
}

// Options controls a move.
type Options struct {
	Force bool
	// DefaultMoveCmd is the global default script, used when the host has none.
	DefaultMoveCmd string
	// TempDir holds the snapshot bundle.
	TempDir string
}

// Plan is what a move would do; --dry-run prints it.
type Plan struct {
	Title          string     `json:"title"`
	Branch         string     `json:"branch"`
	Host           string     `json:"host"`
	Dirty          bool       `json:"uncommitted_work"`
	Steps          []string   `json:"steps"`
	MoveScript     string     `json:"move_script,omitempty"`
	ScriptInput    ScriptSpec `json:"script_input"`
	ScriptEnvNames []string   `json:"script_env"`
}

// Describe builds the plan without touching anything but a read-only status
// probe of the worktree.
func Describe(ctx context.Context, s Session, h Host, opt Options) (Plan, error) {
	cmd := firstNonEmpty(h.MoveCmd, opt.DefaultMoveCmd)
	if err := CheckShallow(ctx, s.Worktree); err != nil {
		return Plan{}, err
	}
	out, err := git(ctx, s.Worktree, nil, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return Plan{}, err
	}
	steps := []string{
		"quiesce: wait for the agent to go idle" + map[bool]string{true: " (skipped: --force)", false: ""}[opt.Force],
		"snapshot: branch history + uncommitted + untracked files into a bundle",
		"provision: sandbox workspace seeded from the bundle on " + h.Name,
	}
	if cmd != "" {
		steps = append(steps, "move script: run on this machine")
	}
	steps = append(steps,
		"verify: the destination's tree equals the snapshot's before the source is released",
		"sync account and credentials (not implemented yet: W7.3/W7.8)",
		"start the agent in the sandbox (conversation resume: not implemented yet: W7.2)",
		"switch the session row to the sandbox backend",
		"archive the local worktree (restorable)",
	)
	return Plan{
		Title: s.Title, Branch: s.Branch, Host: h.Name, Dirty: strings.TrimSpace(out) != "",
		Steps: steps, MoveScript: cmd,
		ScriptInput:    ScriptSpec{SessionID: s.ID, Title: s.Title, Branch: s.Branch, SourceWorktree: s.Worktree, Host: h.Name, DestWorkspace: "<assigned at provision>", Account: s.Account},
		ScriptEnvNames: []string{"AF_SSH", "AF_SSH_DEST", "AF_SSH_FULL"},
	}, nil
}

// Run moves s to h. All-or-nothing: any failure before SwitchRow reaps the
// sandbox and leaves the local session exactly as it was; the local worktree is
// archived only after the row has switched.
func Run(ctx context.Context, st Steps, s Session, h Host, opt Options) (err error) {
	if err := st.Quiesce(ctx, s, opt.Force); err != nil {
		return fmt.Errorf("quiesce: %w", err)
	}
	snap, err := TakeSnapshot(ctx, s.Worktree, s.Branch, s.ID, opt.TempDir)
	if err != nil {
		return err
	}
	defer snap.Cleanup()

	workspace, reap, err := st.Provision(ctx, s, h, snap)
	if err != nil {
		if reap != nil {
			err = errors.Join(err, reap())
		}
		return fmt.Errorf("provision: %w", err)
	}
	switched := false
	defer func() {
		if err != nil && !switched && reap != nil {
			err = errors.Join(err, reap())
		}
	}()

	spec := ScriptSpec{SessionID: s.ID, Title: s.Title, Branch: s.Branch, SourceWorktree: s.Worktree, Host: h.Name, DestWorkspace: workspace, Account: s.Account}
	if err = RunScript(ctx, firstNonEmpty(h.MoveCmd, opt.DefaultMoveCmd), spec, h); err != nil {
		return err
	}
	// The destination must hold exactly what was snapshotted before the agent
	// starts there or the source is released.
	got, verr := st.VerifyDestination(ctx, s, h, snap)
	if verr != nil {
		err = fmt.Errorf("verify destination: %w", verr)
		return err
	}
	if err = snap.CheckTree(got); err != nil {
		return err
	}
	if err = st.SyncAccount(ctx, s, h); err != nil {
		return fmt.Errorf("account sync: %w", err)
	}
	if err = st.StartAgent(ctx, s, h); err != nil {
		return fmt.Errorf("start agent on %s: %w", h.Name, err)
	}
	if err = st.SwitchRow(ctx, s, h); err != nil {
		return fmt.Errorf("switch session row: %w", err)
	}
	switched = true
	if err := st.ArchiveLocal(ctx, s); err != nil {
		// The session already runs on the sandbox; a failed local archive is
		// reported, never rolled back.
		return fmt.Errorf("moved to %s, but archiving the local worktree failed (archive it manually): %w", h.Name, err)
	}
	return nil
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
