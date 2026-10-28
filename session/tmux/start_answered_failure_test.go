package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/fsprobe"
	"github.com/stretchr/testify/require"
)

type answeredFailurePtyFactory struct {
	t       *testing.T
	waitErr error
}

// TestStartAnsweredCommandFailureDoesNotOverrideObservedSession proves the exit
// status is not itself the cleanup marker. If the post-exit probe sees the
// runtime, the workspace must remain protected even though new-session failed.
func TestStartAnsweredCommandFailureDoesNotOverrideObservedSession(t *testing.T) {
	forceNewSessionEnvMarkers(t, false)
	var probes atomic.Int32
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			for _, arg := range c.Args {
				if arg != "has-session" {
					continue
				}
				if probes.Add(1) >= 3 {
					return nil
				}
				return errors.New("session not found")
			}
			return nil
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return nil, nil },
	}
	ptyFactory := answeredFailurePtyFactory{
		t:       t,
		waitErr: errors.New("new-session returned a late failure"),
	}
	ts := NewTmuxSessionWithDeps("answered-start-live", "sh", ptyFactory, cmdExec)

	err := ts.Start(t.TempDir())
	if err == nil {
		t.Fatal("Start succeeded after its launch command exited non-zero")
	}
	if errors.Is(err, ErrSessionNotStarted) {
		t.Fatalf("Start marked the workspace cleanup-safe even though its post-exit probe saw the session: %v", err)
	}
}

func (f answeredFailurePtyFactory) Start(*exec.Cmd) (*os.File, error) {
	f.t.Fatal("Start called instead of StartTracked")
	return nil, nil
}

func (f answeredFailurePtyFactory) StartTracked(*exec.Cmd) (*os.File, <-chan error, error) {
	ptmx, err := os.CreateTemp(f.t.TempDir(), "answered-start-pty")
	if err != nil {
		return nil, nil, err
	}
	done := make(chan error, 1)
	done <- f.waitErr
	close(done)
	return ptmx, done, nil
}

// TestStartAnsweredCommandFailureDoesNotClaimPreSpawn covers a new-session (or
// its systemd-run wrapper) that exits non-zero while exact probes answer that no
// session exists. The launch process began, so later name absence is not proof
// that a pane never ran or finished flushing into the worktree.
func TestStartAnsweredCommandFailureDoesNotClaimPreSpawn(t *testing.T) {
	forceNewSessionEnvMarkers(t, false)
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return errors.New("session not found") },
		OutputFunc: func(*exec.Cmd) ([]byte, error) {
			return nil, nil
		},
	}
	ptyFactory := answeredFailurePtyFactory{
		t:       t,
		waitErr: errors.New("new-session refused the request"),
	}
	ts := NewTmuxSessionWithDeps("answered-start-failure", "sh", ptyFactory, cmdExec)

	err := ts.Start(t.TempDir())
	if err == nil {
		t.Fatal("Start succeeded after its launch command exited non-zero")
	}
	if errors.Is(err, ErrSessionNotStarted) {
		t.Fatalf("a post-spawn failure was misclassified as proof the process never began: %v", err)
	}
}

// TestStartAnsweredFailureCarriesSocketFSProbeEvidence is the #5295-review
// regression: tmux's own launch failure can be the SERVER dying on its
// socket filesystem — ENOSPC creating tmux-<uid>/default or writing server
// state — and that errno exists only as text in the child's output, which
// the classifier refuses to read. The socket-filesystem probe must join a
// REAL errno onto the returned error so lost-restore classification reads
// environmental instead of charging the structural retry budget.
func TestStartAnsweredFailureCarriesSocketFSProbeEvidence(t *testing.T) {
	forceNewSessionEnvMarkers(t, false)
	old := socketFSProbe
	socketFSProbe = func(string) error {
		return &os.PathError{Op: "write", Path: "/tmp", Err: syscall.ENOSPC}
	}
	defer func() { socketFSProbe = old }()

	cmdExec := cmd_test.MockCmdExec{
		RunFunc:    func(*exec.Cmd) error { return errors.New("session not found") },
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return nil, nil },
	}
	ptyFactory := answeredFailurePtyFactory{
		t:       t,
		waitErr: errors.New("tmux new-session exited with status 1"),
	}
	ts := NewTmuxSessionWithDeps("answered-start-enospc", "sh", ptyFactory, cmdExec)

	err := ts.Start(t.TempDir())
	if err == nil {
		t.Fatal("Start succeeded after its launch command exited non-zero")
	}
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("launch failure on a full socket filesystem must carry ENOSPC in the chain: %v", err)
	}
}

// TestStartPrespawnProbeNoAnswerCarriesSentinel is the #5295-review
// regression: when the shared tmux server is wedged, Start's pre-spawn
// has-session gate cannot get an answer — an environmental stall that must
// reach the classifier as the bounded non-answer class
// (ErrTmuxServerNoAnswer + context.DeadlineExceeded), not bare
// ErrTmuxTimeout, which the structural budget charges.
func TestStartPrespawnProbeNoAnswerCarriesSentinel(t *testing.T) {
	forceNewSessionEnvMarkers(t, false)
	shortTmuxTimeout(t, 50*time.Millisecond)
	cmdExec := cmd_test.MockCmdExec{
		// has-session never answers inside the bounded ctx — the wedged
		// server case. A slow error return after the deadline trips is what
		// probeSessionContext reads as !known.
		RunFunc: func(*exec.Cmd) error {
			time.Sleep(300 * time.Millisecond)
			return errors.New("tmux never answered")
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return nil, nil },
	}
	ts := NewTmuxSessionWithDeps("prespawn-noanswer", "sh", answeredFailurePtyFactory{t: t}, cmdExec)

	err := ts.Start(t.TempDir())
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrTmuxServerNoAnswer),
		"the pre-spawn no-answer must carry its dedicated sentinel: %v", err)
	require.True(t, errors.Is(err, context.DeadlineExceeded),
		"the sentinel must pair with the deadline the classifier requires: %v", err)
	require.True(t, errors.Is(err, ErrTmuxTimeout),
		"existing ErrTmuxTimeout consumers must keep matching: %v", err)
}

// TestTmuxSocketFilesystemDir — the probe answers the filesystem the socket
// write actually lands on: an EXISTING tmux-<uid> dir (possibly its own
// mount), or the TMUX_TMPDIR base where tmux creates it, or the $TMUX
// socket's own directory when the env names one (#5295 review).
func TestTmuxSocketFilesystemDir(t *testing.T) {
	t.Setenv("TMUX", "")
	base := t.TempDir()
	t.Setenv("TMUX_TMPDIR", base)

	// No uid dir yet → the base is where tmux will create it.
	dir, err := tmuxSocketFilesystemDir()
	require.NoError(t, err)
	require.Equal(t, base, dir)

	// An existing uid dir is the socket write's real filesystem — it may be
	// a bind mount or its own full fs the base knows nothing about.
	uidDir := filepath.Join(base, fmt.Sprintf("tmux-%d", os.Getuid()))
	require.NoError(t, os.Mkdir(uidDir, 0o700))
	dir, err = tmuxSocketFilesystemDir()
	require.NoError(t, err)
	require.Equal(t, uidDir, dir)

	// $TMUX names a live socket — its own directory is the answer.
	sock := filepath.Join(t.TempDir(), "sockdir", "default")
	t.Setenv("TMUX", sock+",123,0")
	dir, err = tmuxSocketFilesystemDir()
	require.NoError(t, err)
	require.Equal(t, filepath.Dir(sock), dir)
}

// TestTmuxSocketFilesystemDir_StatFailure is the #5295-review regression:
// a uid-dir lookup that FAILS (a wedged bind/NFS mount returning the
// bounded non-answer, or a real errno) must not fall back silently to the
// parent — the parent can be a different filesystem entirely. The uid dir
// stays the probe target so the write probe's own verdict lands for the
// filesystem tmux actually uses, and the stat error is handed back as
// evidence for the joined launch error.
func TestTmuxSocketFilesystemDir_StatFailure(t *testing.T) {
	t.Setenv("TMUX", "")
	base := t.TempDir()
	t.Setenv("TMUX_TMPDIR", base)
	uidDir := filepath.Join(base, fmt.Sprintf("tmux-%d", os.Getuid()))

	statErr := fmt.Errorf("stat probe of %s did not answer within its bound: %w: %w",
		uidDir, fsprobe.ErrProbeTimedOut, context.DeadlineExceeded)
	old := statSocketDir
	statSocketDir = func(string) (os.FileInfo, error) { return nil, statErr }
	defer func() { statSocketDir = old }()

	dir, err := tmuxSocketFilesystemDir()
	require.ErrorIs(t, err, fsprobe.ErrProbeTimedOut,
		"the bounded stat non-answer must reach the caller as evidence")
	require.ErrorIs(t, err, context.DeadlineExceeded,
		"the sentinel must keep its deadline pair")
	require.Equal(t, uidDir, dir,
		"a failed uid-dir stat keeps the uid dir as the probe target")

	// A plain failure (EACCES on an unsearchable mount) is still evidence,
	// still the uid dir — anything but the normal ENOENT stays honest.
	statSocketDir = func(string) (os.FileInfo, error) {
		return nil, &os.PathError{Op: "stat", Path: uidDir, Err: syscall.EACCES}
	}
	dir, err = tmuxSocketFilesystemDir()
	require.ErrorIs(t, err, syscall.EACCES)
	require.Equal(t, uidDir, dir)
}

// TestStartAnsweredFailure_FreeSocketFSStaysStructural — the same launch
// failure while the socket filesystem probes healthy keeps its structural
// classification: no errno is invented, and the classifier sees exactly the
// error it had.
func TestStartAnsweredFailure_FreeSocketFSStaysStructural(t *testing.T) {
	forceNewSessionEnvMarkers(t, false)
	old := socketFSProbe
	socketFSProbe = func(string) error { return nil }
	defer func() { socketFSProbe = old }()

	cmdExec := cmd_test.MockCmdExec{
		RunFunc:    func(*exec.Cmd) error { return errors.New("session not found") },
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return nil, nil },
	}
	ptyFactory := answeredFailurePtyFactory{
		t:       t,
		waitErr: errors.New("tmux new-session refused the request"),
	}
	ts := NewTmuxSessionWithDeps("answered-start-free", "sh", ptyFactory, cmdExec)

	err := ts.Start(t.TempDir())
	if err == nil {
		t.Fatal("Start succeeded after its launch command exited non-zero")
	}
	if errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("a healthy socket filesystem must not invent ENOSPC: %v", err)
	}
}
