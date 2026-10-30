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
	if r, ok := ParseStatusLine(data, env.Now); ok {
		_ = write(AccountForConfigDir(env.Home, env.Getenv("CLAUDE_CONFIG_DIR")), r)
	}
	command := userStatusLine(data, env)
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

// userStatusLine finds the status line command the user's own Claude settings
// define, in Claude's precedence order: the project's local and shared
// settings, then the user settings in the config dir.
func userStatusLine(data []byte, env TapEnv) string {
	var in struct {
		CWD       string `json:"cwd"`
		Workspace struct {
			ProjectDir string `json:"project_dir"`
			CurrentDir string `json:"current_dir"`
		} `json:"workspace"`
	}
	_ = json.Unmarshal(data, &in)
	var files []string
	project := in.Workspace.ProjectDir
	if project == "" {
		project = in.Workspace.CurrentDir
	}
	if project == "" {
		project = in.CWD
	}
	if project != "" {
		files = append(files,
			filepath.Join(project, ".claude", "settings.local.json"),
			filepath.Join(project, ".claude", "settings.json"))
	}
	configDir := strings.TrimSpace(env.Getenv("CLAUDE_CONFIG_DIR"))
	if configDir == "" && env.UserHome != "" {
		configDir = filepath.Join(env.UserHome, ".claude")
	}
	if configDir != "" {
		files = append(files, filepath.Join(configDir, "settings.json"))
	}
	for _, f := range files {
		sl, ok := ReadStatusLine(f)
		if !ok {
			continue
		}
		cmd := strings.TrimSpace(sl.Command)
		if (sl.Type != "" && sl.Type != "command") || cmd == "" || strings.Contains(cmd, TapCommandArgs) {
			return ""
		}
		return cmd
	}
	return ""
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
