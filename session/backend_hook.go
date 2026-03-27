package session

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
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

// hookPTY holds a persistent attach_cmd PTY for preview capture.
type hookPTY struct {
	cmd    *exec.Cmd
	pty    *os.File
	buf    []byte
	mu     sync.Mutex
	closed bool
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
	args := []string{"--name", i.Title, "--json"}
	if i.Prompt != "" {
		args = append(args, "--prompt", i.Prompt)
	}
	out, err := exec.Command(b.Hooks.LaunchCmd, args...).Output()
	if err != nil {
		return fmt.Errorf("launch_cmd failed: %w", err)
	}

	var meta map[string]interface{}
	if err := json.Unmarshal(out, &meta); err != nil {
		return fmt.Errorf("launch_cmd returned invalid JSON: %w", err)
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

func (b *HookBackend) Kill(i *Instance) error {
	b.closePTY(i.Title)

	args := []string{"--name", i.Title, "--json"}
	out, err := exec.Command(b.Hooks.DeleteCmd, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("delete_cmd failed: %s: %w", string(out), err)
	}

	i.mu.Lock()
	i.started = false
	i.remoteMeta = nil
	i.mu.Unlock()

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
	// Same as Preview for remote — we capture everything from the PTY.
	return b.Preview(i)
}

func (b *HookBackend) Attach(i *Instance) (chan struct{}, error) {
	if !i.started {
		return nil, fmt.Errorf("cannot attach instance that has not been started")
	}

	// Stop the background preview PTY so it doesn't compete for the terminal.
	b.closePTY(i.Title)

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer b.ensurePTY(i) // restart preview PTY after detach

		cmd := exec.Command(b.Hooks.AttachCmd, i.Title)
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

func (b *HookBackend) SetPreviewSize(_ *Instance, _, _ int) error {
	// No-op: remote PTY size is controlled by the remote host.
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
	for _, s := range sessions {
		name, _ := s["name"].(string)
		status, _ := s["status"].(string)
		if name == i.Title && status == "running" {
			return true
		}
	}
	return false
}

func (b *HookBackend) CheckAndHandleTrustPrompt(_ *Instance) bool {
	return false
}

func (b *HookBackend) TapEnter(_ *Instance) {}

// --- PTY management for preview capture ---

func (b *HookBackend) ensurePTY(i *Instance) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.ptys == nil {
		b.ptys = make(map[string]*hookPTY)
	}
	if _, ok := b.ptys[i.Title]; ok {
		return
	}

	cmd := exec.Command(b.Hooks.AttachCmd, i.Title)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		log.ErrorLog.Printf("failed to start attach_cmd PTY for %s: %v", i.Title, err)
		return
	}

	hp := &hookPTY{cmd: cmd, pty: ptmx}
	b.ptys[i.Title] = hp

	// Background goroutine reads PTY output into a ring buffer.
	go func() {
		buf := make([]byte, 4096)
		const maxBuf = 64 * 1024
		for {
			n, err := ptmx.Read(buf)
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

	_ = hp.pty.Close()
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

// previewFromPTY extracts visible lines from the raw PTY buffer.
// For now it does a simple conversion — a more robust approach would
// use a terminal emulator to interpret escape sequences.
func previewFromPTY(raw []byte) string {
	s := string(raw)
	// Split on newlines and take the last screenful.
	lines := strings.Split(s, "\n")
	return strings.Join(lines, "\n")
}
