package planusage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// TapCommandArgs are the words after the af executable in the status line
// command af injects.
const TapCommandArgs = "internal plan-tap"

// tapUserTimeout bounds the user's own status line command.
var tapUserTimeout = 5 * time.Second

const maxStatusLineInput = 1 << 20

// TapEnv is what the tap reads from its process.
type TapEnv struct {
	Home      string
	Getenv    func(string) string
	Now       time.Time
	UserHome  string
	WriteFunc func(account string, r Reading) error
	// Record says whether to store the plan reading. It is the opt-in
	// ([backlog] plan_usage) re-read by the tap itself, so a Claude session that
	// was launched with the tap and outlives the key being turned off stops
	// recording; the user's own status line still renders.
	Record bool
}

// Tap is af's status line command: it stores the plan usage in stdin, then
// runs the user's own status line, if any, with the same input and copies its
// output. It never fails: the status line must keep rendering.
func Tap(stdin io.Reader, stdout io.Writer, env TapEnv) {
	defer func() { _ = recover() }()
	data, _ := io.ReadAll(io.LimitReader(stdin, maxStatusLineInput+1))
	if len(data) > maxStatusLineInput {
		return
	}
	write := env.WriteFunc
	if write == nil {
		write = WriteReading
	}
	if r, ok := ParseStatusLine(data, env.Now); ok && env.Record {
		_ = write(AccountForConfigDir(env.Home, env.Getenv("CLAUDE_CONFIG_DIR")), r)
	}
	command := userStatusLine(env)
	if command == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), tapUserTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Stdin = bytes.NewReader(data)
	cmd.Stdout = stdout
	cmd.WaitDelay = time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	_ = cmd.Run()
}

// userStatusLine finds the status line command in the USER's own Claude
// settings: the settings.json in the account's Claude config dir. It never reads
// a project's .claude/settings*.json. Claude Code runs a project's status line
// only after the user accepted that workspace's trust dialog; the tap cannot see
// that decision, so running a repository-chosen command here would make af run a
// program the user's agent would not have run (af must not be that confused
// deputy). A project status line is therefore not rendered while the tap is
// active; see docs/configuration.md (Backlog plan usage, [backlog]
// plan_usage).
func userStatusLine(env TapEnv) string {
	configDir := strings.TrimSpace(env.Getenv("CLAUDE_CONFIG_DIR"))
	if configDir == "" && env.UserHome != "" {
		configDir = filepath.Join(env.UserHome, ".claude")
	}
	if configDir == "" {
		return ""
	}
	sl, ok := ReadStatusLine(filepath.Join(configDir, "settings.json"))
	if !ok {
		return ""
	}
	cmd := strings.TrimSpace(sl.Command)
	if (sl.Type != "" && sl.Type != "command") || cmd == "" || strings.Contains(cmd, TapCommandArgs) {
		return ""
	}
	return cmd
}

// StatusLine is the statusLine object of a Claude Code settings file.
type StatusLine struct {
	Type            string          `json:"type"`
	Command         string          `json:"command"`
	RefreshInterval json.RawMessage `json:"refreshInterval,omitempty"`
	Padding         json.RawMessage `json:"padding,omitempty"`
}

// ReadStatusLine reads the statusLine of a Claude Code settings file; false
// when the file is missing, unreadable or has none.
func ReadStatusLine(path string) (StatusLine, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return StatusLine{}, false
	}
	var s struct {
		StatusLine *StatusLine `json:"statusLine"`
	}
	if json.Unmarshal(raw, &s) != nil || s.StatusLine == nil {
		return StatusLine{}, false
	}
	return *s.StatusLine, true
}
