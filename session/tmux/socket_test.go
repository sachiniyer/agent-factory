package tmux

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
)

// TestSocketPath_ReturnsTrimmedTmuxReportedPath pins that SocketPath returns
// exactly what tmux's #{socket_path} format prints, trimmed, and that it queries
// display-message for THIS session (exact target). This is the authoritative
// socket the config-agent attach pins with `-S` (#2019).
func TestSocketPath_ReturnsTrimmedTmuxReportedPath(t *testing.T) {
	var gotArgs []string
	mock := cmd_test.MockCmdExec{
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			gotArgs = c.Args
			return []byte("/tmp/tmux-1000/default\n"), nil
		},
	}
	session := NewTmuxSessionFromSanitizedNameWithDeps("af_af-config-1", "bash", NewMockPtyFactory(t), mock)

	path, err := session.SocketPath(context.Background())
	if err != nil {
		t.Fatalf("SocketPath: %v", err)
	}
	if path != "/tmp/tmux-1000/default" {
		t.Fatalf("socket path should be tmux's #{socket_path}, trimmed; got %q", path)
	}

	joined := strings.Join(gotArgs, " ")
	for _, want := range []string{"display-message", "#{socket_path}", "af_af-config-1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the socket query is missing %q; got: %s", want, joined)
		}
	}
}

// TestSocketPath_EmptyIsAnError pins that an empty socket_path is surfaced rather
// than returned as a usable path: attaching with `-S ""` would be worse than the
// default-socket fallback, which an empty return triggers on the caller side.
func TestSocketPath_EmptyIsAnError(t *testing.T) {
	mock := cmd_test.MockCmdExec{
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return []byte("   \n"), nil },
	}
	session := NewTmuxSessionFromSanitizedNameWithDeps("af_af-config-1", "bash", NewMockPtyFactory(t), mock)

	if _, err := session.SocketPath(context.Background()); err == nil {
		t.Fatal("an empty socket_path must be an error, not a usable path")
	}
}

// TestSocketPath_TimeoutStaysReachable pins that a wedged display-message surfaces
// as ErrTmuxTimeout, so the daemon can tell "server is slow, unknown" from a real
// answer — the same discipline every other bounded tmux query keeps (#1917).
func TestSocketPath_TimeoutStaysReachable(t *testing.T) {
	shortTmuxTimeout(t, 100*time.Millisecond)
	mock := cmd_test.MockCmdExec{
		OutputFunc: func(*exec.Cmd) ([]byte, error) {
			time.Sleep(2 * time.Second)
			return nil, errors.New("wedged tmux never answered")
		},
	}
	session := NewTmuxSessionFromSanitizedNameWithDeps("af_af-config-1", "bash", NewMockPtyFactory(t), mock)

	_, err := session.SocketPath(context.Background())
	if err == nil || !errors.Is(err, ErrTmuxTimeout) {
		t.Fatalf("a wedged socket_path query must surface ErrTmuxTimeout, got: %v", err)
	}
}
