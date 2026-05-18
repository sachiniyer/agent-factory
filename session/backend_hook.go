package session

import (
	"encoding/json"
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
		// Restoring from storage — just reconnect the preview PTY.
		b.ensurePTY(i)
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

	// The script writes progress to stderr and JSON to stdout.
	// With CombinedOutput we get both mixed together. Try to find
	// the first complete top-level JSON value in the output.
	jsonStr := extractJSON(string(out))
	if jsonStr == "" {
		return fmt.Errorf("launch_cmd returned no JSON in output: %s", string(out))
	}

	var meta map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &meta); err != nil {
		return fmt.Errorf("launch_cmd returned invalid JSON: %s: %w", jsonStr, err)
	}

	i.mu.Lock()
	i.remoteMeta = meta
	if name, ok := meta["name"].(string); ok && i.Branch == "" {
		i.Branch = name
	}
	i.started = true
	i.mu.Unlock()

	b.ensurePTY(i)
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

	args := []string{"--name", slug, "--json"}
	out, err := exec.Command(b.Hooks.DeleteCmd, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("delete_cmd failed: %s: %w", string(out), err)
	}

	return nil
}

func (b *HookBackend) Preview(i *Instance) (string, error) {
	hp := b.getPTY(i.Title)
	if hp == nil {
		return "", nil
	}
	hp.mu.Lock()
	defer hp.mu.Unlock()
	return string(hp.buf), nil
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

	// Stop the background preview process so it doesn't compete.
	b.closePTY(i.Title)

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer b.ensurePTY(i) // restart preview process after detach

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
	out, err := exec.Command(b.Hooks.ListCmd, "--json").CombinedOutput()
	if err != nil {
		return false
	}
	// Mirror launch_cmd: list_cmd may write progress to stderr and JSON to
	// stdout, so recover the JSON object from the combined output.
	jsonStr := extractJSON(string(out))
	if jsonStr == "" {
		return false
	}
	var sessions []map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &sessions); err != nil {
		return false
	}
	slug := hookNameForInstance(i)
	for _, s := range sessions {
		name, _ := s["name"].(string)
		status, _ := s["status"].(string)
		if name == slug && status == "running" {
			return true
		}
	}
	return false
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

	out, err := exec.Command(hooks.ListCmd, "--json").CombinedOutput()
	if err != nil {
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

func (b *HookBackend) ensurePTY(i *Instance) {
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
			return
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
		log.ErrorLog.Printf("failed to create pipe for preview of %s: %v", i.Title, err)
		return
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stdoutW

	if err := cmd.Start(); err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		log.ErrorLog.Printf("failed to start attach_cmd for preview of %s: %v", i.Title, err)
		return
	}
	// Close the write end in the parent so reads get EOF when the child exits.
	_ = stdoutW.Close()

	hp := &hookPTY{cmd: cmd, stdout: stdoutR, waitDone: make(chan struct{})}
	b.ptys[i.Title] = hp

	// Background goroutine reads output into a ring buffer.
	go func() {
		buf := make([]byte, 4096)
		const maxBuf = 64 * 1024
		for {
			n, err := stdoutR.Read(buf)
			if n > 0 {
				hp.mu.Lock()
				hp.buf = append(hp.buf, buf[:n]...)
				if len(hp.buf) > maxBuf {
					hp.buf = hp.buf[len(hp.buf)-maxBuf:]
				}
				hp.mu.Unlock()
			}
			if err != nil {
				break
			}
		}
		// The reader has hit EOF or an error, which means the attach_cmd
		// child has closed its stdout (typically because it exited).
		// If closePTY already marked us closed, it owns the Wait() call
		// (and may need to Kill the process); otherwise the child exited
		// on its own, so we Wait() here to reap it and mark the entry
		// closed so a subsequent ensurePTY call can recreate it.
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
}

func (b *HookBackend) closePTY(title string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	hp, ok := b.ptys[title]
	if !ok {
		return
	}
	hp.mu.Lock()
	hp.closed = true
	hp.mu.Unlock()

	_ = hp.stdout.Close()
	// Give the process a moment to exit, then kill if needed.
	done := hp.waitAsync()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = hp.cmd.Process.Kill()
	}
	delete(b.ptys, title)
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

func (b *HookBackend) getPTY(title string) *hookPTY {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ptys == nil {
		return nil
	}
	return b.ptys[title]
}

// previewFromPTY extracts visible lines from the raw buffer.
func previewFromPTY(raw []byte) string {
	s := string(raw)
	lines := strings.Split(s, "\n")
	return strings.Join(lines, "\n")
}
