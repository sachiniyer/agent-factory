package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/stretchr/testify/require"
)

// paneReuseVerbs are the tmux commands that put a program, or keys, into a
// pane that already exists: respawn re-execs a program in the pane it names,
// and send-keys or paste-buffer types into whatever runs there. Abbreviations
// tmux accepts are listed too.
var paneReuseVerbs = map[string]bool{
	"respawn-pane": true, "respawnp": true,
	"respawn-window": true, "respawnw": true,
	"send-keys": true, "send": true,
	"paste-buffer": true, "pasteb": true,
}

// launchCommandRecorder records the argv of every tmux command a mock executor
// is asked to run.
type launchCommandRecorder struct {
	mu    sync.Mutex
	argvs [][]string
}

func (r *launchCommandRecorder) record(c *exec.Cmd) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.argvs = append(r.argvs, append([]string(nil), c.Args...))
}

func (r *launchCommandRecorder) executor(sessionLive bool) cmd_test.MockCmdExec {
	created := false
	return cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			r.record(c)
			if strings.Contains(c.String(), "has-session") && !sessionLive && !created {
				created = true
				return fmt.Errorf("session not found")
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			r.record(c)
			if len(c.Args) >= 2 && c.Args[1] == "show-options" {
				return nil, fmt.Errorf("no server running")
			}
			return []byte("output"), nil
		},
	}
}

func (r *launchCommandRecorder) all(ptys *MockPtyFactory) [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	all := append([][]string(nil), r.argvs...)
	for _, c := range ptys.cmds {
		all = append(all, c.Args)
	}
	return all
}

func requireNoPaneReuse(t *testing.T, argvs [][]string) {
	t.Helper()
	for _, argv := range argvs {
		for _, arg := range argv {
			require.Falsef(t, paneReuseVerbs[arg],
				"a launch must not reuse a pane: %q runs %s", strings.Join(argv, " "), arg)
		}
	}
}

// TestIssue7464ClaudeLaunchesOnlyIntoAPaneItCreates pins the premise #7464's
// canonical composer read rests on for relaunch frames: every Claude launch
// paints on a pane that holds no earlier process's rows.
//
// #7464 reads a whole composer drawn last as its canonical ❯ prompt, so a live
// dialog painted ABOVE an earlier Claude's !, # or > composer reads as master
// already reads it above that Claude's ❯ composer: clear (#7575 review R1,
// 546,920 frames of #7497's relaunch set). That is safe only because af never
// launches Claude where an earlier process's rows remain. Start is the one seam
// every spawn passes through (create, restore, respawn, tab), and it proves the
// name absent, then runs the program as new-session's own command in a pane
// that new-session creates.
//
// If this goes red, a launch reuses a pane, and an earlier Claude's composer
// can sit under a live dialog. Before relaxing it, re-measure #7497's relaunch
// set and decide how the trust gate reads a dialog above a composer it did not
// draw.
func TestIssue7464ClaudeLaunchesOnlyIntoAPaneItCreates(t *testing.T) {
	forceNewSessionEnvMarkers(t, false)
	forceSessionEnvExecutable(t, "/test/af")
	forceAgentOnSearchPath(t, "claude")
	program := wrappedProgramForTest(t, "/test/af", "claude")

	t.Run("a free name is launched as new-session -d's own command", func(t *testing.T) {
		rec := &launchCommandRecorder{}
		ptys := NewMockPtyFactory(t)
		session := newTmuxSession(toTmuxName("fresh-pane", ""), "claude", ptys, rec.executor(false))
		require.NoError(t, session.Start(t.TempDir()))

		all := rec.all(ptys)
		requireNoPaneReuse(t, all)
		var carriers [][]string
		for _, argv := range all {
			for _, arg := range argv {
				if arg == program {
					carriers = append(carriers, argv)
					break
				}
			}
		}
		require.Len(t, carriers, 1, "the program must reach tmux in exactly one command")
		argv := carriers[0]
		verb := -1
		for i, arg := range argv {
			if arg == "new-session" {
				verb = i
				break
			}
		}
		require.GreaterOrEqualf(t, verb, 0, "the program reached tmux outside new-session: %q", strings.Join(argv, " "))
		segment := argv[verb:]
		for i, arg := range segment {
			if arg == ";" {
				segment = segment[:i]
				break
			}
		}
		require.Equalf(t, "-d", segment[1], "new-session must create a detached session: %q", strings.Join(segment, " "))
		require.Equalf(t, program, segment[len(segment)-1],
			"the program must be new-session's own command, run in the pane it creates: %q", strings.Join(segment, " "))
		require.NotContains(t, segment, "-t", "new-session -t joins an existing session's windows")
	})

	t.Run("a live name is refused, never reused", func(t *testing.T) {
		rec := &launchCommandRecorder{}
		ptys := NewMockPtyFactory(t)
		session := newTmuxSession(toTmuxName("live-pane", ""), "claude", ptys, rec.executor(true))
		err := session.Start(t.TempDir())
		require.True(t, errors.Is(err, ErrSessionNameTaken), "a live name must refuse the launch, got: %v", err)

		all := rec.all(ptys)
		requireNoPaneReuse(t, all)
		for _, argv := range all {
			require.NotContainsf(t, argv, program, "a refused launch ran the program: %q", strings.Join(argv, " "))
			require.NotContainsf(t, argv, "new-session", "a refused launch spawned: %q", strings.Join(argv, " "))
		}
	})

	t.Run("no production code respawns a pane", func(t *testing.T) {
		root := filepath.Join("..", "..")
		_, err := os.Stat(filepath.Join(root, "go.mod"))
		require.NoError(t, err, "the module root must be two levels above session/tmux")
		// A Go string literal naming respawn-pane or respawn-window, or any
		// prefix or alias tmux accepts for them.
		respawn := regexp.MustCompile(`"respawn-?[pw][a-z-]*"`)
		var found []string
		err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				name := d.Name()
				if path != root && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" || name == "testdata") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for n, line := range strings.Split(string(src), "\n") {
				if respawn.MatchString(line) {
					found = append(found, fmt.Sprintf("%s:%d: %s", path, n+1, strings.TrimSpace(line)))
				}
			}
			return nil
		})
		require.NoError(t, err)
		require.Emptyf(t, found, "a respawn re-execs a program in a pane that still shows the earlier "+
			"process's rows (#7464):\n%s", strings.Join(found, "\n"))
	})
}
