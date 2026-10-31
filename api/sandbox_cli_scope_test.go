package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// W3.5 x W9.4 (#5891, #5892): every verb the sandbox scope ADMITS must work from the
// in-sandbox CLI with a sandbox credential — and everything the CLI touches on the
// way (its preflights included) must be a route that credential is admitted to.
//
// The server here fronts the REAL scope table (daemon.SandboxAllowedPath): a route
// the table denies is answered 401 exactly as the gate does, and a route it admits
// answers a canned success. So a verb passes only if it succeeds AND every request
// it made was admitted. (The handler-side owner constraints are pinned by the
// daemon package's own scope tests, which run the real handlers.)

type scopeServer struct {
	mu      sync.Mutex
	seen    []string
	refused []string
	bodies  map[string][]byte
	// operator: the credential is an operator token — every route admitted, and a
	// Snapshot that names no caller.
	operator bool
}

func newScopeServer(t *testing.T) (*scopeServer, string) {
	t.Helper()
	s := &scopeServer{bodies: map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.seen = append(s.seen, r.URL.Path)
		s.bodies[r.URL.Path] = body
		s.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer sandbox-secret" {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		if s.operator && r.URL.Path == "/v1/health" {
			_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.PingResponse{OK: true, HostPick: true}))
			return
		}
		if s.operator && r.URL.Path == "/v1/Snapshot" {
			_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.SnapshotResponse{}))
			return
		}
		if !s.operator && !daemon.SandboxAllowedPath(r.URL.Path) {
			// What the gate does for a path outside the scope.
			s.mu.Lock()
			s.refused = append(s.refused, r.URL.Path)
			s.mu.Unlock()
			w.WriteHeader(http.StatusUnauthorized)
			_ = apiproto.WriteEnvelope(w, apiproto.Failure("unauthorized"))
			return
		}
		me := session.InstanceData{ID: "me-id", Title: "me"}
		peer := session.InstanceData{ID: "peer-id", Title: "peer"}
		var data any
		switch r.URL.Path {
		case "/v1/Snapshot":
			data = daemon.SnapshotResponse{
				Instances: []session.InstanceData{me, peer}, Caller: "me-id",
				Capabilities: &daemon.SnapshotCapabilities{HostPick: true, DeliverHostPick: true, TaskHost: true},
			}
		case "/v1/SendPrompt":
			data = daemon.SendPromptResponse{OK: true, Status: session.PromptDeliveryStatus("")}
		case "/v1/ArchiveSession":
			data = daemon.ArchiveSessionResponse{OK: true, ArchivedPath: "/h/arch"}
		case "/v1/CreateTab":
			data = daemon.CreateTabResponse{ID: "tab-1", Name: "shell-1"}
		case "/v1/AddTask":
			data = daemon.AddTaskResponse{OK: true}
		case "/v1/ListTasks":
			data = daemon.ListTasksResponse{Tasks: []task.Task{{ID: "t-1", Name: "n", Prompt: "p", CronExpr: "0 * * * *", TargetSession: "peer"}}}
		case "/v1/RemoveTask":
			data = daemon.RemoveTaskResponse{OK: true}
		case "/v1/CreateSession":
			data = daemon.CreateSessionResponse{Instance: session.InstanceData{ID: "new-id", Title: "new", Host: "box1"}}
		default:
			data = map[string]any{"ok": true}
		}
		_ = apiproto.WriteEnvelope(w, apiproto.Success(data))
	}))
	t.Cleanup(srv.Close)
	return s, srv.URL
}

func (s *scopeServer) reset() { s.mu.Lock(); s.seen, s.refused = nil, nil; s.mu.Unlock() }

// runWithFlags runs a real command's RunE with its flags set, restoring them after.
func runWithFlags(t *testing.T, cmd *cobra.Command, flags map[string]string, args ...string) error {
	t.Helper()
	for name, val := range flags {
		f := cmd.Flags().Lookup(name)
		require.NotNilf(t, f, "flag --%s on %s", name, cmd.Name())
		require.NoError(t, f.Value.Set(val))
	}
	t.Cleanup(func() {
		for name := range flags {
			f := cmd.Flags().Lookup(name)
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		}
	})
	var err error
	captureJSON(t, func() error { err = cmd.RunE(cmd, args); return nil })
	return err
}

func TestSandboxCredentialDrivesEveryAdmittedVerbThroughTheRealCLI(t *testing.T) {
	srv, url := newScopeServer(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir()) // a local registry/tasks.json must never be read
	oldURL, oldTok, oldRepo := apiclient.FlagDaemonURL, apiclient.FlagDaemonToken, repoFlag
	apiclient.FlagDaemonURL, apiclient.FlagDaemonToken = url, "sandbox-secret"
	t.Cleanup(func() { apiclient.FlagDaemonURL, apiclient.FlagDaemonToken, repoFlag = oldURL, oldTok, oldRepo })
	repoFlag = "" // a sandbox does not know the daemon host's layout and never names a repo
	oldSelf := sessionsArchiveSelf
	t.Cleanup(func() { sessionsArchiveSelf = oldSelf })

	verbs := []struct {
		name  string
		run   func(t *testing.T) error
		wants []string // routes the verb must have reached
	}{
		{"whoami", func(t *testing.T) error { return runWithFlags(t, sessionsWhoamiCmd, nil) }, []string{"/v1/Snapshot"}},
		{"send-prompt", func(t *testing.T) error { return runWithFlags(t, sessionsSendPromptCmd, nil, "peer", "hello") }, []string{"/v1/SendPrompt"}},
		{"archive --self", func(t *testing.T) error {
			sessionsArchiveSelf = true
			t.Cleanup(func() { sessionsArchiveSelf = false })
			return runWithFlags(t, sessionsArchiveCmd, nil)
		}, []string{"/v1/Snapshot", "/v1/ArchiveSession"}},
		{"tab-create", func(t *testing.T) error {
			return runWithFlags(t, sessionsTabCreateCmd, map[string]string{"kind": "shell"}, "me")
		}, []string{"/v1/CreateTab"}},
		{"attention --self", func(t *testing.T) error {
			return runWithFlags(t, newSessionsAttentionCmd(), map[string]string{"self": "true", "reason": "pick A or B"})
		}, []string{"/v1/Snapshot", "/v1/SetAttention"}},
		{"tasks add", func(t *testing.T) error {
			return runWithFlags(t, tasksAddCmd, map[string]string{"name": "n", "prompt": "p", "cron": "0 * * * *", "target-session": "peer"})
		}, []string{"/v1/AddTask"}},
		{"tasks get", func(t *testing.T) error { return runWithFlags(t, tasksGetCmd, nil, "t-1") }, []string{"/v1/ListTasks"}},
		{"tasks remove", func(t *testing.T) error { return runWithFlags(t, tasksRemoveCmd, nil, "t-1") }, []string{"/v1/ListTasks", "/v1/RemoveTask"}},
		{"sessions create --host", func(t *testing.T) error {
			return runWithFlags(t, sessionsCreateCmd, map[string]string{"name": "new", "host": "box1", "prompt": "go"})
		}, []string{"/v1/CreateSession"}},
	}
	for _, v := range verbs {
		t.Run(v.name, func(t *testing.T) {
			srv.reset()
			require.NoError(t, v.run(t), "an admitted verb must succeed from the in-sandbox CLI")
			srv.mu.Lock()
			defer srv.mu.Unlock()
			require.Empty(t, srv.refused, "the CLI touched routes the sandbox credential is NOT admitted to: %v", srv.refused)
			for _, want := range v.wants {
				require.Containsf(t, srv.seen, want, "%s never reached %s (saw %v)", v.name, want, srv.seen)
			}
			sort.Strings(srv.seen)
		})
	}
}

// Controls: the relaxations are for a credential the DAEMON says is bound to a
// session. An operator token keeps both rules — --self needs a bound caller, and a
// remote create still has to name its project.
func TestOperatorTokenKeepsTheRemoteSelfAndRepoRules(t *testing.T) {
	srv, url := newScopeServer(t)
	srv.operator = true
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	oldURL, oldTok, oldRepo := apiclient.FlagDaemonURL, apiclient.FlagDaemonToken, repoFlag
	apiclient.FlagDaemonURL, apiclient.FlagDaemonToken = url, "sandbox-secret"
	t.Cleanup(func() { apiclient.FlagDaemonURL, apiclient.FlagDaemonToken, repoFlag = oldURL, oldTok, oldRepo })
	repoFlag = ""
	oldSelf := sessionsArchiveSelf
	t.Cleanup(func() { sessionsArchiveSelf = oldSelf })

	sessionsArchiveSelf = true
	err := runWithFlags(t, sessionsArchiveCmd, nil)
	require.ErrorContains(t, err, "needs a sandbox credential that names the calling session")
	require.NotContains(t, srv.seen, "/v1/ArchiveSession", "nothing was archived on a guess")

	err = runWithFlags(t, sessionsCreateCmd, map[string]string{"name": "new", "host": "box1", "prompt": "go"})
	require.ErrorContains(t, err, "--repo is required")
	err = runWithFlags(t, tasksAddCmd, map[string]string{"name": "n", "prompt": "p", "cron": "0 * * * *", "target-session": "peer"})
	require.ErrorContains(t, err, "--repo is required")
}
