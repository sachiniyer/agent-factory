package move

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ScriptTimeout bounds the operator's move script. A script that hangs must not
// wedge a move forever; hitting it aborts the move like any non-zero exit.
const ScriptTimeout = 10 * time.Minute

// ScriptSpec is the JSON document the move script receives on stdin. Field
// names are a public contract (docs/backends.md); add fields, never rename.
type ScriptSpec struct {
	SessionID string `json:"session_id"`
	Title     string `json:"title"`
	Branch    string `json:"branch"`
	// SourceWorktree is the local worktree path on the daemon box.
	SourceWorktree string `json:"source_worktree"`
	// Host is the destination host name.
	Host string `json:"host"`
	// DestWorkspace is the directory on the destination holding the seeded
	// workspace (the session's worktree root there).
	DestWorkspace string `json:"dest_workspace"`
	// Account is the session's account name; empty when unscoped.
	Account string `json:"account,omitempty"`
	// SSH, SSHDest and SSHFull mirror AF_SSH, AF_SSH_DEST and AF_SSH_FULL.
	SSH     string `json:"ssh,omitempty"`
	SSHDest string `json:"ssh_dest,omitempty"`
	SSHFull string `json:"ssh_full"`
}

// ScriptEnv returns the extra environment the script runs with:
//
//   - AF_SSH      the ssh command WITHOUT the destination (options only), so
//     `rsync -e "$AF_SSH" f "$AF_SSH_DEST:path"` and `$AF_SSH "$AF_SSH_DEST" cmd`
//     reach the same machine af does.
//   - AF_SSH_DEST the destination (user@host or an ssh alias).
//   - AF_SSH_FULL the complete command including the destination. Always set.
//
// A free-form sandbox.ssh command already embeds the host and af cannot split it
// reliably, so for those AF_SSH and AF_SSH_DEST are empty and only AF_SSH_FULL
// is set: use `$AF_SSH_FULL 'cmd'` and, in place of rsync -e, pipe tar:
// `tar -C "$src" -c . | $AF_SSH_FULL 'tar -C dir -x'`.
func ScriptEnv(h Host) []string {
	return []string{"AF_SSH=" + h.SSH, "AF_SSH_DEST=" + h.SSHDest, "AF_SSH_FULL=" + h.fullSSH()}
}

// RunScript runs the operator's move script on the daemon box through `sh -c`
// (the command is operator-written global config, like sandbox_ssh). Exit 0
// continues the move; anything else is an error that aborts it. The script's
// output is returned for the error text only; callers must not log it as a
// matter of course since it may name credentials paths.
func RunScript(ctx context.Context, cmdline string, spec ScriptSpec, h Host) error {
	if strings.TrimSpace(cmdline) == "" {
		return nil
	}
	spec.SSH, spec.SSHDest, spec.SSHFull = h.SSH, h.SSHDest, h.fullSSH()
	body, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, ScriptTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdline)
	cmd.Stdin = bytes.NewReader(body)
	cmd.Dir = spec.SourceWorktree
	cmd.Env = append(os.Environ(), ScriptEnv(h)...)
	cmd.WaitDelay = 5 * time.Second
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		tail := strings.TrimSpace(out.String())
		if len(tail) > 2000 {
			tail = "…" + tail[len(tail)-2000:]
		}
		return fmt.Errorf("move script failed (the session was not moved): %w: %s", err, tail)
	}
	return nil
}
