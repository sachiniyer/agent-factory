package ui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"

	"github.com/stretchr/testify/require"
)

// writeRemoteHookScript writes an executable shell script and returns its path.
func writeRemoteHookScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0755))
	return path
}

// startedRemoteInstance builds and starts a remote (hook-backed) instance with
// (optionally) a terminal_cmd hook configured, so its Tabs are populated by the
// real HookBackend.Start path (#930 PR 6). It registers cleanup that releases the
// preview process without deleting any remote session.
func startedRemoteInstance(t *testing.T, withTerminal bool) *session.Instance {
	t.Helper()
	dir := t.TempDir()
	hooks := config.RemoteHooks{
		LaunchCmd: writeRemoteHookScript(t, dir, "launch.sh", `echo '{"name": "'"$2"'", "status": "running"}'`),
		ListCmd:   writeRemoteHookScript(t, dir, "list.sh", `echo '[{"name": "remote-tabbar", "status": "running"}]'`),
		AttachCmd: writeRemoteHookScript(t, dir, "attach.sh", `echo "attached $1"; sleep 0.1`),
		DeleteCmd: writeRemoteHookScript(t, dir, "delete.sh", `echo '{"deleted": true}'`),
	}
	if withTerminal {
		hooks.TerminalCmd = writeRemoteHookScript(t, dir, "terminal.sh", `echo "terminal $1"; sleep 0.1`)
	}

	inst, err := session.NewInstance(session.InstanceOptions{Title: "remote-tabbar", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	inst.SetBackend(&session.HookBackend{Hooks: hooks})
	require.True(t, inst.IsRemote())
	require.NoError(t, inst.Start(true))
	t.Cleanup(func() { _ = inst.CloseAttachOnly() })
	return inst
}

// TestTabbedWindowRemoteTabBar verifies a remote instance is tab-driven: the bar
// shows the agent (Preview) tab plus a Terminal tab only when terminal_cmd is
// configured — never the local two-tab default when terminal_cmd is absent.
func TestTabbedWindowRemoteTabBar(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	for _, tc := range []struct {
		name       string
		withTerm   bool
		wantLabels []string
	}{
		{"with terminal_cmd shows agent + terminal", true, []string{"Preview", "Terminal"}},
		{"without terminal_cmd shows only the agent tab", false, []string{"Preview"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := startedRemoteInstance(t, tc.withTerm)
			w := newTestTabbedWindow()
			setWindowInstance(w, inst)
			require.Equal(t, tc.wantLabels, w.tabLabels())

			// Toggle wraps within the real tab count, never a phantom slot.
			w.Toggle()
			if tc.withTerm {
				require.Equal(t, 1, w.GetActiveTab(), "Toggle advances onto the terminal tab")
			} else {
				require.Equal(t, 0, w.GetActiveTab(), "single-tab remote: Toggle stays on the agent tab")
			}
		})
	}
}
