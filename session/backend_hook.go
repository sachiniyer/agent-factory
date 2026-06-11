package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/creack/pty"
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

// HookBackend implements Backend by delegating to user-provided shell scripts.
type HookBackend struct {
	Hooks config.RemoteHooks

	// mu protects the pty fields below.
	mu sync.Mutex
	// ptys maps instance title → running attach PTY for preview capture.
	ptys map[string]*hookPTY
}

// hookPTY holds a persistent attach_cmd process for preview capture.
// Instead of allocating a real PTY (which SSH rejects without a terminal),
// we use a pipe-based approach that captures whatever the attach_cmd writes.
type hookPTY struct {
	cmd      *exec.Cmd
	stdout   *os.File // read end of stdout pipe
	buf      []byte
	mu       sync.Mutex
	closed   bool
	waitOnce sync.Once
	waitDone chan struct{}
	waitErr  error
}

var slugRegexp = regexp.MustCompile(`[^a-z0-9-]`)

// ed2Marker is ED2 (erase entire display). The documented remote preview
// pattern (docs/remote-hooks.md) emits it at the top of every capture-loop
// iteration, so hookPTY ingestion treats it as a snapshot boundary (#810).
var ed2Marker = []byte("\x1b[2J")

// ingest appends a chunk of attach_cmd output to the preview buffer.
//
// ED2 (\x1b[2J) marks the start of a fresh snapshot: the documented preview
// contract is a clear-screen + capture loop, so everything up to and
// including the last ED2 is a stale frame and is dropped instead of
// concatenated (#810). The search runs over the accumulated buffer, not just
// the incoming chunk, so a sequence split across read boundaries is detected
// once its tail arrives — the head bytes are already in buf.
func (hp *hookPTY) ingest(chunk []byte) {
	const maxBuf = 64 * 1024
	hp.mu.Lock()
	defer hp.mu.Unlock()
	hp.buf = append(hp.buf, chunk...)
	if idx := bytes.LastIndex(hp.buf, ed2Marker); idx >= 0 {
		hp.buf = hp.buf[idx+len(ed2Marker):]
	}
	if len(hp.buf) > maxBuf {
		hp.buf = hp.buf[len(hp.buf)-maxBuf:]
	}
}

// Slugify converts a title to a slug-safe string for hook scripts.
// The slug is part of the public remote hook protocol documented in
// docs/remote-hooks.md: launch_cmd, list_cmd, attach_cmd, and delete_cmd all
// receive this value unless the instance was imported with an explicit
// remote_meta.name.
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

// slugify is kept as an unexported alias for existing package-local call sites
// and tests.
func slugify(title string) string { return Slugify(title) }

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

func (b *HookBackend) Type() string { return "remote" }

func (b *HookBackend) Start(i *Instance, firstTimeSetup bool) error {
	if i.Title == "" {
		return fmt.Errorf("instance title cannot be empty")
	}

	if !firstTimeSetup {
		// Restoring from storage. Before marking the instance started,
		// confirm that the remote session reported by list_cmd still
		// exists. Without this check, deleted/expired remote sessions
		// were restored and shown with a green Ready dot in the sidebar
		// even though attaching was a silent no-op (#645).
		//
		// list_cmd is optional at config-validation time (import/sync treat
		// an empty list_cmd as "no remote sessions to enumerate", #738), but
		// restore has no other way to verify liveness. An empty list_cmd here
		// would surface as an opaque exec failure from isAliveWithTimeout.
		// Fail fast with an actionable message that names the missing field
		// instead (#753).
		if strings.TrimSpace(b.Hooks.ListCmd) == "" {
			return fmt.Errorf("cannot restore remote session %q: list_cmd is required for restore (currently empty in config; add a list_cmd to remote_hooks so the agent can verify the remote session is still alive)", i.Title)
		}
		// Distinguish "list_cmd could not be run" from "list_cmd ran and the
		// session is absent" (#841): the first is a local config/network
		// problem, the second a remote deletion or rename. For the absent
		// case, naming what list_cmd DID return makes a hook-script rename
		// self-diagnosing instead of requiring a manual list_cmd run.
		alive, listed, aliveErr := b.isAliveWithTimeout(i, restoreAliveTimeout)
		if aliveErr != nil {
			return fmt.Errorf("cannot verify remote session %q: %w", i.Title, aliveErr)
		}
		if !alive {
			return fmt.Errorf("remote session %q no longer exists in list_cmd output%s", i.Title, formatListedNames(listed))
		}
		if err := b.ensurePTY(i); err != nil {
			return fmt.Errorf("failed to start preview process: %w", err)
		}
		i.mu.Lock()
		i.started = true
		i.mu.Unlock()
		return nil
	}

	// Launch a new remote session.
	slug := Slugify(i.Title)
	args := []string{"--name", slug, "--json"}
	cmd := exec.Command(b.Hooks.LaunchCmd, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("launch_cmd failed: %s: %w", string(out), err)
	}

	// launch_cmd exited 0, so the remote session may now exist on the remote
	// host even though we have not yet parsed its metadata. From here on, any
	// failure must trigger best-effort cleanup via delete_cmd, otherwise the
	// remote session is orphaned: Start returns an error, the caller's Kill
	// sees remoteMeta == nil and skips delete_cmd, and the session leaks
	// permanently (#739). delete_cmd is invoked with the same slug launch_cmd
	// received, which is the only identifier we have when the JSON is
	// unparseable.

	// The script writes progress to stderr and JSON to stdout.
	// With CombinedOutput we get both mixed together. Try to find
	// the first complete top-level JSON value in the output.
	jsonStr := extractJSON(string(out))
	if jsonStr == "" {
		b.cleanupOrphanedLaunch(slug, i.Title)
		return fmt.Errorf("launch_cmd returned no JSON in output: %s", string(out))
	}

	var meta map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &meta); err != nil {
		b.cleanupOrphanedLaunch(slug, i.Title)
		return fmt.Errorf("launch_cmd returned invalid JSON: %s: %w", jsonStr, err)
	}

	i.mu.Lock()
	i.remoteMeta = meta
	if name, ok := meta["name"].(string); ok && i.Branch == "" {
		i.Branch = name
	}
	i.started = true
	i.mu.Unlock()

	if err := b.ensurePTY(i); err != nil {
		// launch_cmd succeeded so the remote session itself is alive; we
		// just couldn't spin up the preview process. Log and continue —
		// the user can still attach interactively.
		log.WarningLog.Printf("hook backend: preview process failed for %s: %v", i.Title, err)
	}
	return nil
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

func (b *HookBackend) Kill(i *Instance) error {
	slug := hookNameForInstance(i)

	// Snapshot whether a remote session was ever allocated before clearing
	// state. If Start never ran successfully there is nothing for delete_cmd
	// to clean up — invoking it would surface as a confusing failure against
	// a slug the user-provided script has never heard of. Mirrors
	// LocalBackend.Kill's tmuxSession/gitWorktree guards.
	//
	// Mark the instance as stopped BEFORE any resource cleanup so that the
	// instance is in a consistent state even if delete_cmd fails. Otherwise
	// the PTY could be closed while started=true, leaving the session
	// appearing running but unusable (empty preview, broken attach).
	i.mu.Lock()
	hadRemote := i.remoteMeta != nil
	i.started = false
	i.remoteMeta = nil
	i.mu.Unlock()

	b.closePTY(i.Title)

	if !hadRemote {
		log.WarningLog.Printf("kill %q: skipping delete_cmd, no remote session allocated", i.Title)
		return nil
	}

	out, err := b.runDeleteCmd(slug)
	if err != nil {
		return fmt.Errorf("delete_cmd failed: %s: %w", string(out), err)
	}

	return nil
}

// runDeleteCmd invokes delete_cmd for the given hook name and returns its
// combined output and error. Shared by Kill (which surfaces the error to the
// user) and the orphan-cleanup path in Start (which logs it best-effort, #739)
// so both stay in sync on how delete_cmd is invoked.
func (b *HookBackend) runDeleteCmd(name string) ([]byte, error) {
	return exec.Command(b.Hooks.DeleteCmd, "--name", name, "--json").CombinedOutput()
}

// cleanupOrphanedLaunch best-effort deletes a remote session that launch_cmd
// created but whose metadata Start failed to parse (#739). It never retries
// and never returns an error: the parse failure is the error the user sees,
// and if delete_cmd also fails the user can clean up manually with delete_cmd.
// slug is the --name launch_cmd was invoked with — the only identifier we have
// once the JSON payload is unusable.
func (b *HookBackend) cleanupOrphanedLaunch(slug, title string) {
	out, err := b.runDeleteCmd(slug)
	if err != nil {
		log.WarningLog.Printf(
			"hook backend: failed to clean up orphaned remote session %q (slug %q) after launch_cmd JSON parse failure; clean up manually via delete_cmd: %s: %v",
			title, slug, strings.TrimSpace(string(out)), err)
		return
	}
	log.WarningLog.Printf(
		"hook backend: cleaned up orphaned remote session %q (slug %q) after launch_cmd JSON parse failure",
		title, slug)
}

func (b *HookBackend) Preview(i *Instance) (string, error) {
	hp := b.getPTY(i.Title)
	if hp == nil {
		return "", nil
	}
	hp.mu.Lock()
	raw := string(hp.buf)
	hp.mu.Unlock()
	// Never return the raw stream: attach_cmd output is a PTY stream whose
	// control sequences would be executed by the real terminal when the
	// preview pane is flushed, corrupting the whole TUI (#810). Sanitizing
	// happens outside the lock so the ingestion goroutine is never blocked
	// on it.
	return sanitizeHookPreview(raw), nil
}

func (b *HookBackend) PreviewFullHistory(i *Instance) (string, error) {
	// Same as Preview for remote — we capture everything from the process.
	return b.Preview(i)
}

func (b *HookBackend) Attach(i *Instance) (chan struct{}, error) {
	i.mu.RLock()
	s := i.started
	i.mu.RUnlock()

	if !s {
		return nil, fmt.Errorf("cannot attach instance that has not been started")
	}

	// Stop the background preview process so it doesn't compete, but reap it
	// in the background: Attach runs on the bubbletea event loop, and waiting
	// out the 2s grace period here froze the whole TUI whenever the preview
	// process didn't exit promptly (#817). The dying preview only writes into
	// its own buffer via its own pipe (see stopPreview), so it cannot
	// interleave output with the interactive attach_cmd started below.
	if hp := b.stopPreview(i.Title); hp != nil {
		go hp.reap()
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			// Restart preview process after detach. Failure is non-fatal —
			// the user can still re-attach interactively; we just lose the
			// background preview snapshot.
			if err := b.ensurePTY(i); err != nil {
				log.WarningLog.Printf("hook backend: preview process failed to restart for %s: %v", i.Title, err)
			}
		}()

		slug := hookNameForInstance(i)
		cmd := exec.Command(b.Hooks.AttachCmd, slug)
		if err := runHookAttachWithDetach(cmd, os.Stdin, os.Stdout, os.Stderr); err != nil {
			log.ErrorLog.Printf("attach_cmd error: %v", err)
		}
	}()
	return done, nil
}

func (b *HookBackend) HasUpdated(_ *Instance) (updated bool, hasPrompt bool) {
	return false, false
}

func (b *HookBackend) SendPrompt(_ *Instance, _ string) error {
	return fmt.Errorf("SendPrompt not supported for remote sessions")
}

func (b *HookBackend) SendPromptCommand(_ *Instance, _ string) error {
	return fmt.Errorf("SendPromptCommand not supported for remote sessions")
}

func (b *HookBackend) SendKeys(_ *Instance, _ string) error {
	return fmt.Errorf("SendKeys not supported for remote sessions")
}

func (b *HookBackend) SetPreviewSize(_ *Instance, _, _ int) error {
	// No-op: remote session size is controlled by the remote host.
	return nil
}

func (b *HookBackend) IsAlive(i *Instance) bool {
	alive, _, _ := b.isAliveWithTimeout(i, runtimeAliveTimeout)
	return alive
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

func (b *HookBackend) CheckAndHandleTrustPrompt(_ *Instance) bool {
	return false
}

func (b *HookBackend) TapEnter(_ *Instance) {}

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

func runHookAttachWithDetach(cmd *exec.Cmd, stdin io.Reader, stdout, stderr io.Writer) error {
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return err
	}
	defer ptmx.Close()

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()

	copyDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(stdout, ptmx)
		close(copyDone)
	}()

	detached := make(chan struct{})
	var detachOnce sync.Once
	detach := func() {
		detachOnce.Do(func() {
			close(detached)
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = ptmx.Close()
		})
	}

	go func() {
		buf := make([]byte, 32)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				if n == 1 && buf[0] == tmux.DetachKeyByte {
					detach()
					return
				}
				if _, writeErr := ptmx.Write(buf[:n]); writeErr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	err = <-waitDone
	_ = ptmx.Close()
	<-copyDone

	select {
	case <-detached:
		return nil
	default:
	}
	if err != nil {
		fmt.Fprintf(stderr, "remote attach exited: %v\n", err)
		return err
	}
	return nil
}

// --- Process management for preview capture ---

func (b *HookBackend) ensurePTY(i *Instance) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.ptys == nil {
		b.ptys = make(map[string]*hookPTY)
	}
	if existing, ok := b.ptys[i.Title]; ok {
		// If the existing entry is still alive, reuse it. Otherwise drop
		// the stale entry and fall through to create a fresh process so
		// preview can recover after attach_cmd has exited.
		existing.mu.Lock()
		alive := !existing.closed
		existing.mu.Unlock()
		if alive {
			return nil
		}
		delete(b.ptys, i.Title)
	}

	slug := hookNameForInstance(i)
	cmd := exec.Command(b.Hooks.AttachCmd, slug)

	// Use pipes instead of a PTY. The attach_cmd for preview doesn't need
	// a real terminal — we just want to capture whatever it outputs.
	// (SSH-based attach scripts will fail gracefully here since they
	// require a TTY, and that's fine — preview just shows empty until
	// the user attaches interactively.)
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create pipe for preview of %s: %w", i.Title, err)
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stdoutW

	if err := cmd.Start(); err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return fmt.Errorf("start attach_cmd for preview of %s: %w", i.Title, err)
	}
	// Close the write end in the parent so reads get EOF when the child exits.
	_ = stdoutW.Close()

	hp := &hookPTY{cmd: cmd, stdout: stdoutR, waitDone: make(chan struct{})}
	b.ptys[i.Title] = hp

	// Background goroutine reads output into a ring buffer with ED2
	// snapshot-reset semantics (see hookPTY.ingest).
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdoutR.Read(buf)
			if n > 0 {
				hp.ingest(buf[:n])
			}
			if err != nil {
				break
			}
		}
		// The reader has hit EOF or an error, which means the attach_cmd
		// child has closed its stdout (typically because it exited).
		// If stopPreview already marked us closed, the detaching caller
		// owns the Wait() call via reap (and may need to Kill the
		// process); otherwise the child exited on its own, so we Wait()
		// here to reap it and mark the entry closed so a subsequent
		// ensurePTY call can recreate it.
		hp.mu.Lock()
		alreadyClosed := hp.closed
		hp.mu.Unlock()
		if alreadyClosed {
			return
		}
		if err := hp.wait(); err != nil {
			log.ErrorLog.Printf("attach_cmd preview process exited: %v", err)
		}
		hp.mu.Lock()
		hp.closed = true
		hp.mu.Unlock()
	}()
	return nil
}

// stopPreview removes the preview entry for title and signals its process to
// stop, without waiting for it to exit. It returns the detached hookPTY (nil
// if none was registered) so the caller decides where to pay the grace-period
// wait in reap: Kill reaps synchronously so the preview process is gone before
// delete_cmd tears down the remote session it is connected to; Attach reaps in
// a background goroutine because it runs on the bubbletea event loop, where a
// blocking wait freezes the TUI (#817).
//
// The entry is deleted from b.ptys before the process exits. That is safe:
// once detached, the dying process can only write into its own hp.buf via its
// own pipe — Preview no longer finds the entry, and a replacement started by
// ensurePTY gets a fresh pipe and buffer, so output cannot interleave.
// Closing the pipe's read end makes the process's next write fail with EPIPE,
// nudging well-behaved attach scripts to exit during the grace period.
func (b *HookBackend) stopPreview(title string) *hookPTY {
	b.mu.Lock()
	defer b.mu.Unlock()

	hp, ok := b.ptys[title]
	if !ok {
		return nil
	}
	delete(b.ptys, title)

	hp.mu.Lock()
	hp.closed = true
	hp.mu.Unlock()

	_ = hp.stdout.Close()
	return hp
}

// reap gives the detached preview process a moment to exit, then kills it.
// It blocks for up to the 2s grace period, so it must never run on the TUI
// event loop — see stopPreview for which callers reap where.
func (hp *hookPTY) reap() {
	select {
	case <-hp.waitAsync():
	case <-time.After(2 * time.Second):
		_ = hp.cmd.Process.Kill()
	}
}

// closePTY synchronously stops and reaps the preview process for title.
// Used by Kill (delete_cmd must not race a live preview connection) and by
// test cleanup; Attach uses stopPreview + a background reap instead (#817).
func (b *HookBackend) closePTY(title string) {
	if hp := b.stopPreview(title); hp != nil {
		hp.reap()
	}
}

func (hp *hookPTY) waitAsync() <-chan struct{} {
	hp.waitOnce.Do(func() {
		go func() {
			hp.waitErr = hp.cmd.Wait()
			close(hp.waitDone)
		}()
	})
	return hp.waitDone
}

func (hp *hookPTY) wait() error {
	<-hp.waitAsync()
	return hp.waitErr
}

// SetPreviewBufferForTest seeds the raw preview buffer for title, creating
// the entry if needed. Exported in a non-test file (mirroring FakeBackend)
// so ui tests can drive the hook preview path through PreviewPane without
// spawning a real attach_cmd process.
func (b *HookBackend) SetPreviewBufferForTest(title string, data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ptys == nil {
		b.ptys = make(map[string]*hookPTY)
	}
	hp, ok := b.ptys[title]
	if !ok {
		hp = &hookPTY{waitDone: make(chan struct{})}
		b.ptys[title] = hp
	}
	hp.mu.Lock()
	hp.buf = append([]byte(nil), data...)
	hp.mu.Unlock()
}

func (b *HookBackend) getPTY(title string) *hookPTY {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ptys == nil {
		return nil
	}
	return b.ptys[title]
}
