package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// restoreAliveTimeout bounds how long we wait for list_cmd to report on
// whether a persisted remote session still exists. list_cmd is user-supplied
// and may block on network/SSH; the restore path runs at TUI startup for
// every persisted instance, so an unbounded wait would stall the TUI.
//
// The restore path is intentionally aggressive: a session that was alive a
// moment ago must respond promptly, so 2s is enough to clear out stale
// entries without dragging out startup.
const restoreAliveTimeout = 2 * time.Second

// runtimeAliveTimeout bounds steady-state IsAlive checks issued from
// background ticks (every 3-5s). list_cmd may SSH to remote hosts where
// transient latency is routine, so this is intentionally more generous than
// restoreAliveTimeout; the goal is to avoid freezing the TUI on a hanging
// list_cmd (#666), not to fail fast on slow networks.
const runtimeAliveTimeout = 5 * time.Second

var slugRegexp = regexp.MustCompile(`[^a-z0-9-]`)

// Slugify converts a title to a slug-safe string for hook scripts.
// The slug is part of the public remote hook protocol documented in
// docs/remote-hooks.md: launch_cmd, list_cmd, attach_cmd, delete_cmd, and
// terminal_cmd all receive this value unless the instance was imported with
// an explicit remote_meta.name.
func Slugify(title string) string {
	s := strings.ToLower(title)
	s = strings.ReplaceAll(s, " ", "-")
	s = slugRegexp.ReplaceAllString(s, "")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "session"
	}
	return s
}

// RemoteHookName returns the hook-protocol name for a title and persisted
// remote metadata. Imported remote sessions can carry their authoritative
// list_cmd name in remote_meta.name; TUI-created sessions derive it from the
// title.
func RemoteHookName(title string, meta map[string]interface{}) string {
	if name, ok := meta["name"].(string); ok && name != "" {
		return name
	}
	return Slugify(title)
}

// FindSlugCollision returns the title of the first existing remote instance
// whose hook name collides with candidate, or "" if none do.
func FindSlugCollision(candidate string, existing []*Instance) string {
	if candidate == "" {
		return ""
	}
	want := Slugify(candidate)
	for _, inst := range existing {
		if inst == nil || inst.Title == candidate {
			continue
		}
		inst.mu.RLock()
		name := RemoteHookName(inst.Title, inst.remoteMeta)
		inst.mu.RUnlock()
		if name == want {
			return inst.Title
		}
	}
	return ""
}

func hookNameForInstance(i *Instance) string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return RemoteHookName(i.Title, i.remoteMeta)
}

// extractJSON finds the first complete top-level JSON value (object or array)
// in output, ignoring text outside JSON delimiters. It handles pretty-printed
// / multi-line JSON and stderr interleaving around (but not inside) the JSON
// payload. Returns empty string if no valid JSON value is found.
func extractJSON(output string) string {
	for i := 0; i < len(output); i++ {
		if output[i] != '{' && output[i] != '[' {
			continue
		}

		var depth int
		inString := false
		escape := false

		for j := i; j < len(output); j++ {
			c := output[j]

			if escape {
				escape = false
				continue
			}
			if c == '\\' && inString {
				escape = true
				continue
			}
			if c == '"' {
				inString = !inString
				continue
			}

			if !inString {
				if c == '{' || c == '[' {
					depth++
				}
				if c == '}' || c == ']' {
					depth--
					if depth == 0 {
						candidate := output[i : j+1]
						var test interface{}
						if json.Unmarshal([]byte(candidate), &test) == nil {
							return candidate
						}
						break
					}
				}
			}
		}
	}
	return ""
}

// isAliveWithTimeout asks list_cmd whether the remote session backing i is
// currently running. The three outcomes are distinct (#841):
//   - err != nil: list_cmd could not be run or its output was unparseable —
//     this says nothing about whether the remote session exists, and callers
//     that surface errors must NOT report it as "no longer exists".
//   - alive false, err nil: list_cmd ran fine and the session is absent.
//     listed carries the names list_cmd did report (nil for an empty list) so
//     callers can make a rename mismatch self-diagnosing.
//   - alive true: the session is listed with status "running".
//
// A non-zero timeout bounds the wait; zero falls through to an unbounded
// exec, which no production caller should use because IsAlive runs on the
// TUI event loop and a hanging list_cmd would freeze the UI (#666). Callers
// must pass either restoreAliveTimeout or runtimeAliveTimeout.
func (b *HookBackend) isAliveWithTimeout(i *Instance, timeout time.Duration) (alive bool, listed []string, err error) {
	out, runErr := runListCmd(b.Hooks.ListCmd, timeout)
	// exec.ErrWaitDelay is non-fatal here (#676). runListCmd sets
	// cmd.WaitDelay, so CombinedOutput returns ErrWaitDelay when the list_cmd
	// script itself exited (per docs/remote-hooks.md, with code 0 on success)
	// but a backgrounded child still holds the stdout/stderr pipes open. In
	// that case the script's output is already complete on stdout; fall
	// through to extractJSON + json.Unmarshal, which validate it. A genuinely
	// broken list_cmd produces no parseable JSON and still errors below.
	if runErr != nil && !errors.Is(runErr, exec.ErrWaitDelay) {
		return false, nil, fmt.Errorf("list_cmd failed: %s: %w", strings.TrimSpace(string(out)), runErr)
	}
	// Mirror launch_cmd: list_cmd may write progress to stderr and JSON to
	// stdout, so recover the JSON object from the combined output.
	jsonStr := extractJSON(string(out))
	if jsonStr == "" {
		return false, nil, fmt.Errorf("list_cmd returned no JSON in output: %s", strings.TrimSpace(string(out)))
	}
	var sessions []map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &sessions); err != nil {
		return false, nil, fmt.Errorf("list_cmd returned invalid JSON: %s: %w", jsonStr, err)
	}
	slug := hookNameForInstance(i)
	for _, s := range sessions {
		name, _ := s["name"].(string)
		status, _ := s["status"].(string)
		if name != "" {
			listed = append(listed, name)
		}
		if name == slug && status == "running" {
			alive = true
		}
	}
	return alive, listed, nil
}

// formatListedNames renders the names list_cmd reported, for appending to the
// "no longer exists in list_cmd output" restore error so a hook-script rename
// is self-diagnosing (#841). An empty list yields "" to preserve the original
// message for genuinely-empty list_cmd output; longer lists are capped so a
// busy remote host cannot bloat the error.
func formatListedNames(listed []string) string {
	if len(listed) == 0 {
		return ""
	}
	const maxNames = 5
	if len(listed) > maxNames {
		return fmt.Sprintf(" (listed: %s, and %d more)", strings.Join(listed[:maxNames], ", "), len(listed)-maxNames)
	}
	return fmt.Sprintf(" (listed: %s)", strings.Join(listed, ", "))
}

// runListCmd executes the user-supplied list_cmd and returns its combined
// output. A non-zero timeout bounds the wait via context + WaitDelay; zero
// falls through to an unbounded exec, which no production caller should use.
// Every list_cmd invocation runs on a path where an unbounded wait freezes
// the UI, so each caller MUST pass a non-zero timeout:
//   - IsAlive → runtimeAliveTimeout (steady-state TUI event loop, #666)
//   - Start restore → restoreAliveTimeout (TUI startup, #645)
//   - ListRemoteHookInstanceData → restoreAliveTimeout (startup import; the
//     TUI blocks on this over RPC with no client-side deadline, so a hanging
//     list_cmd would stall startup indefinitely, #692)
func runListCmd(listCmd string, timeout time.Duration) ([]byte, error) {
	if timeout <= 0 {
		return exec.Command(listCmd, "--json").CombinedOutput()
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, listCmd, "--json")
	// WaitDelay bounds how long CombinedOutput keeps reading from the
	// command's stdout/stderr after the context is cancelled. Without it,
	// a list_cmd script that spawned a long-running child (e.g. `sleep
	// 30`) would keep the read-side pipe open past the kill signal sent
	// to the script itself, defeating the timeout (#645).
	cmd.WaitDelay = 500 * time.Millisecond
	return cmd.CombinedOutput()
}

// ListRemoteHookInstanceData converts running sessions reported by list_cmd
// into persistable remote InstanceData records for the current repo.
func ListRemoteHookInstanceData(repoPath string, hooks config.RemoteHooks, now time.Time) ([]InstanceData, error) {
	if hooks.ListCmd == "" {
		return nil, nil
	}

	// Bound the wait with restoreAliveTimeout (2s). This runs at TUI startup
	// inside the daemon handler that the TUI blocks on over RPC, and the RPC
	// client sets no call deadline, so an unbounded list_cmd would hang
	// startup indefinitely (#692). Fast-fail is appropriate for a startup
	// gate; the caller logs the error and proceeds with persisted sessions.
	out, err := runListCmd(hooks.ListCmd, restoreAliveTimeout)
	// exec.ErrWaitDelay is non-fatal here, mirroring isAliveWithTimeout (#676):
	// runListCmd sets cmd.WaitDelay, so CombinedOutput returns ErrWaitDelay
	// when the list_cmd script exited 0 with complete output but a backgrounded
	// child still holds the stdout/stderr pipes open. Fall through to
	// extractJSON + json.Unmarshal, which validate the payload; a genuinely
	// broken or timed-out list_cmd produces no parseable JSON and still errors.
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return nil, fmt.Errorf("list_cmd failed: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// Mirror launch_cmd: list_cmd may write progress to stderr and JSON to
	// stdout, so recover the JSON array from the combined output.
	jsonStr := extractJSON(string(out))
	if jsonStr == "" {
		return nil, fmt.Errorf("list_cmd returned no JSON in output: %s", strings.TrimSpace(string(out)))
	}

	var listed []map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &listed); err != nil {
		return nil, fmt.Errorf("list_cmd returned invalid JSON: %s: %w", jsonStr, err)
	}

	imported := make([]InstanceData, 0, len(listed))
	for _, meta := range listed {
		name, _ := meta["name"].(string)
		if name == "" {
			continue
		}
		status, _ := meta["status"].(string)
		if status != "running" {
			continue
		}

		title := name
		if displayTitle, _ := meta["title"].(string); displayTitle != "" {
			title = displayTitle
		}

		imported = append(imported, InstanceData{
			Title:       title,
			Path:        repoPath,
			Branch:      name,
			Status:      Running,
			CreatedAt:   now,
			UpdatedAt:   now,
			BackendType: "remote",
			RemoteMeta:  meta,
		})
	}
	return imported, nil
}
