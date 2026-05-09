package tmux

import (
	"fmt"
	cmd2 "github.com/sachiniyer/agent-factory/cmd"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	aflog "github.com/sachiniyer/agent-factory/log"

	"github.com/stretchr/testify/require"
)

// TestMain initializes the logger so tests that exercise paths writing to
// InfoLog/ErrorLog (e.g. Restore's re-spawn fallback) do not nil-deref.
func TestMain(m *testing.M) {
	aflog.Initialize(false)
	defer aflog.Close()
	os.Exit(m.Run())
}

type MockPtyFactory struct {
	t *testing.T

	// Array of commands and the corresponding file handles representing PTYs.
	cmds  []*exec.Cmd
	files []*os.File
}

func (pt *MockPtyFactory) Start(cmd *exec.Cmd) (*os.File, error) {
	filePath := filepath.Join(pt.t.TempDir(), fmt.Sprintf("pty-%s-%d", pt.t.Name(), rand.Int31()))
	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_RDWR, 0644)
	if err == nil {
		pt.cmds = append(pt.cmds, cmd)
		pt.files = append(pt.files, f)
	}
	return f, err
}

func (pt *MockPtyFactory) Close() {}

func NewMockPtyFactory(t *testing.T) *MockPtyFactory {
	return &MockPtyFactory{
		t: t,
	}
}

func TestSanitizeName(t *testing.T) {
	// Without repo path (legacy naming)
	session := NewTmuxSession("asdf", "program")
	require.Equal(t, TmuxPrefix+"asdf", session.sanitizedName)

	session = NewTmuxSession("a sd f . . asdf", "program")
	require.Equal(t, TmuxPrefix+"asdf__asdf", session.sanitizedName)

	// With repo path (repo-scoped naming)
	session = NewTmuxSessionForRepo("asdf", "/home/user/repo", "program")
	hash := repoHash("/home/user/repo")
	require.Equal(t, TmuxPrefix+hash+"_asdf", session.sanitizedName)

	// Same title, different repo → different tmux name
	session2 := NewTmuxSessionForRepo("asdf", "/home/user/other-repo", "program")
	require.NotEqual(t, session.sanitizedName, session2.sanitizedName)

	// FromSanitizedName preserves exact name
	session3 := NewTmuxSessionFromSanitizedName("af_custom_name", "program")
	require.Equal(t, "af_custom_name", session3.sanitizedName)
}

// errPtyFactory is a PtyFactory that fails Start(). Used to verify Restore
// surfaces non-missing-session errors from the PTY layer.
type errPtyFactory struct {
	err error
}

func (e errPtyFactory) Start(_ *exec.Cmd) (*os.File, error) { return nil, e.err }
func (e errPtyFactory) Close()                              {}

// TestRestoreAttachesWhenSessionExists covers issue #386 case (a): when the
// tmux session is alive, Restore must attach to it (not re-spawn) regardless
// of whether a workDir was supplied.
func TestRestoreAttachesWhenSessionExists(t *testing.T) {
	ptyFactory := NewMockPtyFactory(t)

	cmdExec := cmd_test.MockCmdExec{
		// All cmdExec.Run calls succeed → has-session reports the session exists.
		RunFunc:    func(cmd *exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return []byte("output"), nil },
	}

	session := newTmuxSession(toTmuxName("existing", ""), "claude", ptyFactory, cmdExec)

	require.NoError(t, session.Restore("/some/work/dir"))

	require.Equal(t, 1, len(ptyFactory.cmds), "expected exactly one PTY command (attach-session)")
	require.Equal(t, "tmux attach-session -t af_existing", cmd2.ToString(ptyFactory.cmds[0]))
}

// TestRestoreRespawnsWhenSessionMissing covers issue #386 case (b): when the
// tmux server has died (e.g. across a machine reboot) the session is gone but
// the worktree on disk is fine. Restore must transparently re-spawn the
// session in workDir using the same program.
func TestRestoreRespawnsWhenSessionMissing(t *testing.T) {
	ptyFactory := NewMockPtyFactory(t)

	// First two has-session calls report missing (the outer Restore check, then
	// the existence check at the top of Start). After tmux new-session runs via
	// the PTY factory, subsequent has-session calls report exists so Start's
	// poll loop and the inner Restore("") call can succeed.
	hasSessionCalls := 0
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "has-session") {
				hasSessionCalls++
				if hasSessionCalls <= 2 {
					return fmt.Errorf("can't find session")
				}
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return []byte("output"), nil },
	}

	workdir := t.TempDir()
	session := newTmuxSession(toTmuxName("missing", ""), "claude", ptyFactory, cmdExec)

	require.NoError(t, session.Restore(workdir))

	require.Equal(t, 2, len(ptyFactory.cmds),
		"expected new-session followed by attach-session via PTY")
	require.Equal(t,
		fmt.Sprintf("tmux new-session -d -s af_missing -c %s claude", workdir),
		cmd2.ToString(ptyFactory.cmds[0]))
	require.Equal(t, "tmux attach-session -t af_missing", cmd2.ToString(ptyFactory.cmds[1]))
}

// TestRestoreSurfacesPtyError covers issue #386 case (c): real failures
// distinct from "session does not exist" — here a PTY allocation failure on
// the attach path — must propagate to the caller unchanged so an operator
// can act on them rather than silently fall back to a re-spawn.
func TestRestoreSurfacesPtyError(t *testing.T) {
	ptyFactory := errPtyFactory{err: fmt.Errorf("pty allocation failed")}

	cmdExec := cmd_test.MockCmdExec{
		// Session exists → Restore takes the attach branch where PTY error fires.
		RunFunc:    func(cmd *exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return []byte("output"), nil },
	}

	session := newTmuxSession(toTmuxName("ptyerr", ""), "claude", ptyFactory, cmdExec)

	err := session.Restore("/some/work/dir")
	require.Error(t, err)
	require.Contains(t, err.Error(), "pty allocation failed")
}

// TestRestoreReturnsErrorWhenSessionMissingAndNoWorkDir guards the contract
// used by Detach() and Start()'s internal Restore("") call: when no workDir
// is provided, a missing session is a real error and must not silently
// re-spawn (which would lose history on Detach or recurse infinitely from
// Start).
func TestRestoreReturnsErrorWhenSessionMissingAndNoWorkDir(t *testing.T) {
	ptyFactory := NewMockPtyFactory(t)

	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "has-session") {
				return fmt.Errorf("can't find session")
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return []byte("output"), nil },
	}

	session := newTmuxSession(toTmuxName("gone", ""), "claude", ptyFactory, cmdExec)

	err := session.Restore("")
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not exist")
	require.Equal(t, 0, len(ptyFactory.cmds), "expected no PTY commands when no workDir is provided")
}

func TestStartTmuxSession(t *testing.T) {
	ptyFactory := NewMockPtyFactory(t)

	created := false
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "has-session") && !created {
				created = true
				return fmt.Errorf("session not found")
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			return []byte("output"), nil
		},
	}

	workdir := t.TempDir()
	session := newTmuxSession(toTmuxName("test-session", ""), "claude", ptyFactory, cmdExec)

	err := session.Start(workdir)
	require.NoError(t, err)
	require.Equal(t, 2, len(ptyFactory.cmds))
	require.Equal(t, fmt.Sprintf("tmux new-session -d -s af_test-session -c %s claude", workdir),
		cmd2.ToString(ptyFactory.cmds[0]))
	require.Equal(t, "tmux attach-session -t af_test-session",
		cmd2.ToString(ptyFactory.cmds[1]))

	require.Equal(t, 2, len(ptyFactory.files))

	// File should be closed.
	_, err = ptyFactory.files[0].Stat()
	require.Error(t, err)
	// File should be open
	_, err = ptyFactory.files[1].Stat()
	require.NoError(t, err)
}
