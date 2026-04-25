package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
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
	cmd    *exec.Cmd
	stdout *os.File // read end of stdout pipe
	buf    []byte
	mu     sync.Mutex
	closed bool
}

var slugRegexp = regexp.MustCompile(`[^a-z0-9-]`)

// slugify converts a title to a slug-safe string for hook scripts.
// A short hash of the original title is appended to prevent collisions
// when different titles (e.g. "my_app" vs "myapp") reduce to the same slug.
func slugify(title string) string {
	s := strings.ToLower(title)
	s = strings.ReplaceAll(s, " ", "-")
	s = slugRegexp.ReplaceAllString(s, "")
	// Trim leading/trailing hyphens
	s = strings.Trim(s, "-")
	if s == "" {
		s = "session"
	}

	// Append a short hash of the original title to guarantee uniqueness.
	h := sha256.Sum256([]byte(title))
	return s + "-" + hex.EncodeToString(h[:])[:8]
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
	slug := slugify(i.Title)
	args := []string{"--name", slug, "--json"}
	cmd := exec.Command(b.Hooks.LaunchCmd, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("launch_cmd failed: %s: %w", string(out), err)
	}

	// The script writes progress to stderr and JSON to stdout.
	// With CombinedOutput we get both mixed together. Try to find
	// the JSON object in the output (last line starting with '{').
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

// extractJSON finds the last JSON object in mixed output (stderr + stdout).
func extractJSON(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	// Search from the end for a line that looks like JSON
	for idx := len(lines) - 1; idx >= 0; idx-- {
		line := strings.TrimSpace(lines[idx])
		if strings.HasPrefix(line, "{") {
			return line
		}
	}
	return ""
}

func (b *HookBackend) Kill(i *Instance) error {
	// Mark the instance as stopped BEFORE any resource cleanup so that the
	// instance is in a consistent state even if delete_cmd fails. Otherwise
	// the PTY could be closed while started=true, leaving the session
	// appearing running but unusable (empty preview, broken attach).
	// This mirrors LocalBackend.Kill's behavior.
	i.mu.Lock()
	i.started = false
	i.remoteMeta = nil
	i.mu.Unlock()

	b.closePTY(i.Title)

	slug := slugify(i.Title)
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

		slug := slugify(i.Title)
		cmd := exec.Command(b.Hooks.AttachCmd, slug)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
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
	out, err := exec.Command(b.Hooks.ListCmd, "--json").Output()
	if err != nil {
		return false
	}
	var sessions []map[string]interface{}
	if err := json.Unmarshal(out, &sessions); err != nil {
		return false
	}
	slug := slugify(i.Title)
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

	slug := slugify(i.Title)
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

	hp := &hookPTY{cmd: cmd, stdout: stdoutR}
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
		if err := hp.cmd.Wait(); err != nil {
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
	done := make(chan error, 1)
	go func() { done <- hp.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = hp.cmd.Process.Kill()
	}
	delete(b.ptys, title)
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
