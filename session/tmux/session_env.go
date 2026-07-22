package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

var sessionEnvExecutable = os.Executable

// SetEnvPassthrough replaces the exact-name extension to this session's
// default-deny environment. Call it before Start or Restore; sibling tabs copy
// the normalized list from their agent session.
func (t *TmuxSession) SetEnvPassthrough(names []string) error {
	normalized, err := sessionenv.NormalizeExtraNames(names)
	if err != nil {
		return err
	}
	t.programMu.Lock()
	t.envPassthrough = normalized
	t.programMu.Unlock()
	return nil
}

func (t *TmuxSession) launchEnvironment(program string) (string, []string, []string, error) {
	t.programMu.RLock()
	extra := append([]string(nil), t.envPassthrough...)
	t.programMu.RUnlock()
	agent := sessionenv.AgentForCommand(program)
	executable, err := sessionEnvExecutable()
	if err != nil {
		return "", nil, nil, err
	}
	wrapped, err := sessionenv.WrapCommand(executable, agent, extra, program)
	if err != nil {
		return "", nil, nil, err
	}
	source := os.Environ()
	return wrapped,
		sessionenv.FilterForCommand(source, agent, program, extra),
		sessionenv.ImportNamesForCommand(source, agent, program, extra), nil
}

// importClientEnvironmentArgs makes an existing tmux server copy the approved
// client variables into only the new session. update-environment is temporarily
// expanded and restored in the same tmux command queue, so values stay in
// cmd.Env and never enter argv or the server's persistent global environment.
func (t *TmuxSession) importClientEnvironmentArgs(newSessionArgs, names []string) ([]string, error) {
	ctx, cancel := tmuxTimeoutContext()
	previousRaw, err := t.outputTmuxBounded(ctx, "show-options", "-gv", "update-environment")
	timedOut := ctx.Err() != nil
	cancel()
	if err != nil {
		if timedOut {
			return nil, fmt.Errorf("%w: read existing tmux update-environment option", ErrTmuxTimeout)
		}
		if tmuxServerAbsent(err) {
			// No server is the ordinary first-session case. A fresh server snapshots
			// the already-filtered client environment, so no import override is needed.
			return newSessionArgs, nil
		}
		return nil, fmt.Errorf("read existing tmux update-environment option: %w", err)
	}

	previous := strings.Fields(string(previousRaw))
	combinedSet := make(map[string]struct{}, len(previous)+len(names))
	for _, name := range previous {
		combinedSet[name] = struct{}{}
	}
	for _, name := range names {
		combinedSet[name] = struct{}{}
	}
	combined := make([]string, 0, len(combinedSet))
	for name := range combinedSet {
		combined = append(combined, name)
	}
	sort.Strings(combined)

	args := []string{"set-option", "-g", "update-environment", strings.Join(combined, " "), ";"}
	args = append(args, newSessionArgs...)
	args = append(args, ";", "set-option", "-g", "update-environment", strings.Join(previous, " "))
	return args, nil
}

func tmuxServerAbsent(err error) bool {
	message := err.Error()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		message += " " + string(exitErr.Stderr)
	}
	message = strings.ToLower(message)
	for _, fragment := range []string{
		"no server running",
		"failed to connect to server",
		"error connecting to ",
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}
