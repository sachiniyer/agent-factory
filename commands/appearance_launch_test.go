package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/rpc"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

type LaunchPaletteRequest struct{}
type LaunchPaletteResponse struct{ Changed bool }
type launchConfigControl struct {
	mu           sync.Mutex
	calls        []string
	listener     string
	requireToken bool
}

func (s *launchConfigControl) ApplyTheme(_ LaunchPaletteRequest, _ *LaunchPaletteResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "ApplyTheme")
	return nil
}
func (s *launchConfigControl) ApplyConfig(_ daemon.ApplyConfigRequest, _ *daemon.ApplyConfigResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "ApplyConfig")
	s.listener = "0.0.0.0:9999"
	s.requireToken = false
	return nil
}

func TestPaletteRetirementRuntimeLaunch(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	leaveAmbientRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("auto_update = false\nappearance = 'light'\n[network]\nlisten_addr = '0.0.0.0:9999'\nrequire_token = false\n"), 0600))
	socket, err := daemon.DaemonSocketPath()
	require.NoError(t, err)
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	stub := &launchConfigControl{listener: "127.0.0.1:8443", requireToken: true}
	server := rpc.NewServer()
	require.NoError(t, server.RegisterName("Control", stub))
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go server.ServeConn(conn)
		}
	}()
	oldRun, oldTasks := runLaunchApp, launchEnsureDaemonForTasks
	t.Cleanup(func() { runLaunchApp = oldRun; launchEnsureDaemonForTasks = oldTasks })
	mounted := false
	runLaunchApp = func(context.Context, string, *config.RepoContext) error { mounted = true; return nil }
	launchEnsureDaemonForTasks = func() {}
	require.NoError(t, rootCmd.RunE(&cobra.Command{}, nil))
	require.True(t, mounted)
	stub.mu.Lock()
	defer stub.mu.Unlock()
	require.Empty(t, stub.calls, "opening the TUI must not request any live configuration apply")
	require.Equal(t, "127.0.0.1:8443", stub.listener)
	require.True(t, stub.requireToken)
}

func TestPaletteRetirementCLIRead(t *testing.T) {
	tempAFHome(t)
	leaveAmbientRepo(t)
	path := filepath.Join(os.Getenv("AGENT_FACTORY_HOME"), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("theme = 'dark'\n"), 0600))
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	require.NoError(t, configGetCmd.RunE(cmd, []string{"appearance"}))
	require.Equal(t, "dark\n", out.String())
	out.Reset()
	prev := configJSONFlag
	configJSONFlag = true
	t.Cleanup(func() { configJSONFlag = prev })
	require.NoError(t, configListCmd.RunE(cmd, nil))
	var env struct {
		Data []configEntry `json:"data"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &env))
	found := false
	for _, e := range env.Data {
		require.NotEqual(t, "theme", e.Key)
		if e.Key == "appearance" {
			found = true
			require.Equal(t, "dark", e.Value)
		}
	}
	require.True(t, found)
	configJSONFlag = false
	for _, key := range []string{"theme", "theme.accent"} {
		out.Reset()
		require.ErrorContains(t, configGetCmd.RunE(cmd, []string{key}), "retired")
	}
}

func TestPaletteRetirementRejectsOlderRemoteBeforeWrite(t *testing.T) {
	newConfigHome(t)
	stub := newStubDaemon(t, "1.9.0")
	t.Setenv("AF_DAEMON_URL", stub.url())
	for _, verb := range []string{"set", "unset"} {
		for _, key := range []string{"theme", "theme.accent"} {
			args := []string{verb, key}
			if verb == "set" {
				args = append(args, "nord")
			}
			_, _, err := runConfigCLI(t, args...)
			if err == nil || !strings.Contains(err.Error(), "retired") || !strings.Contains(err.Error(), "appearance") {
				t.Errorf("%s %s: want retirement error, got %v", verb, key, err)
			}
		}
	}
	require.Empty(t, stub.sets())
	require.Empty(t, stub.unsets())
}
