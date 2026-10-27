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
}

// ScriptEnv returns the extra environment the script runs with. AF_SSH is the
// exact ssh command af uses for the destination, so `rsync -e "$AF_SSH" …` and
// `$AF_SSH host 'cmd'` reach the same machine af does.
func ScriptEnv(sshCmd string) []string {
	return []string{"AF_SSH=" + sshCmd}
}

// RunScript runs the operator's move script on the daemon box through `sh -c`
// (the command is operator-written global config, like sandbox_ssh). Exit 0
// continues the move; anything else is an error that aborts it. The script's
// output is returned for the error text only; callers must not log it as a
// matter of course since it may name credentials paths.
func RunScript(ctx context.Context, cmdline string, spec ScriptSpec, sshCmd string) error {
	if strings.TrimSpace(cmdline) == "" {
		return nil
	}
	body, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, ScriptTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdline)
	cmd.Stdin = bytes.NewReader(body)
	cmd.Dir = spec.SourceWorktree
	cmd.Env = append(os.Environ(), ScriptEnv(sshCmd)...)
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
