package tmux

import (
	"os"

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

func (t *TmuxSession) launchEnvironment(program string) (string, []string, error) {
	t.programMu.RLock()
	extra := append([]string(nil), t.envPassthrough...)
	t.programMu.RUnlock()
	agent := DetectAgentFromCommand(program)
	executable, err := sessionEnvExecutable()
	if err != nil {
		return "", nil, err
	}
	wrapped, err := sessionenv.WrapCommand(executable, agent, extra, program)
	if err != nil {
		return "", nil, err
	}
	return wrapped, sessionenv.Filter(os.Environ(), agent, extra), nil
}
