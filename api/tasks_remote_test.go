package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/task"

	"github.com/stretchr/testify/require"
)

// Every `af tasks` verb against a remote daemon ROUTES there (#3730).
//
// All eight used to ignore --daemon-url/AF_DAEMON_URL completely: they bound
// straight to daemon.AddTask/…/ListTasksNoSpawn, which dial the LOCAL unix
// socket, and the mutating four reach that socket through callDaemon, which
// calls EnsureDaemon and STARTS a local daemon when none is serving. So a
// remote-targeted `af tasks add` armed a cron task on the caller's own laptop,
// printed a success line naming a local project path, and could bring up a
// daemon on the machine the operator was steering away from.
//
// Every case below therefore asserts three things at once, because each has an
// appealing wrong answer:
//
//	routes        the request reaches the TARGETED daemon carrying what the
//	              operator typed, and its answer — not this machine's — is what
//	              reaches stdout.
//	local store   a seeded local task is neither read nor written. Seeded rather
//	              than absent so "untouched" has teeth: an empty home would only
//	              catch a local CREATE, not a local edit or delete.
//	no daemon     no local daemon was started. Two independent oracles: the
//	              daemon.* indirections (the only route from these verbs to
//	              callDaemon/EnsureDaemon) are replaced with stubs that fail the
//	              test if entered, AND the AF home is checked for the sockets a
//	              started daemon binds.

// stubTaskDaemon is an HTTP server answering the daemon's six task routes
// through the REAL apiproto envelope writer and the REAL 404 catch-all shape, so
// a case here is a round trip against the wire contract rather than a mock
// agreeing with itself. It records what it was sent.
type stubTaskDaemon struct {
	server *httptest.Server

	mu         sync.Mutex
	listHits   int
	healthHits int
	adds       []daemon.AddTaskRequest
	updates    []daemon.UpdateTaskRequest
	removes    []daemon.RemoveTaskRequest
	restarts   []daemon.RestartTaskRequest
	triggers   []daemon.TriggerTaskRequest

	// tasks is what this "daemon host" reports holding. Deliberately bound to a
	// project path that could not exist in the caller's temp home, so a row
	// reaching stdout proves which store it came from.
	tasks []task.Task
	// version is what GET /v1/health reports. Empty models a daemon predating
	// version reporting (#1044), which still answers Ping.
	version string
	// unserved names routes this daemon does not have, modelling an older build.
	unserved map[string]bool
}

// The remote daemon's own project path and one task on it. Neither string can
// be produced by this test's local home, so either appearing in output is proof
// the answer came from the remote.
const (
	stubDaemonProjectPath = "/srv/boxoperator/projects/alpha"
	stubDaemonTaskID      = "remotetask1"
	stubDaemonTaskName    = "nightly-on-box"
)

func newStubTaskDaemon(t *testing.T, version string, unserved ...string) *stubTaskDaemon {
	t.Helper()
	d := &stubTaskDaemon{
		version:  version,
		unserved: map[string]bool{},
		tasks: []task.Task{{
			ID:          stubDaemonTaskID,
			Name:        stubDaemonTaskName,
			Prompt:      "sweep the box",
			CronExpr:    "0 3 * * *",
			ProjectPath: stubDaemonProjectPath,
			Program:     "claude",
			Enabled:     true,
			CreatedAt:   time.Date(2026, time.August, 1, 3, 0, 0, 0, time.UTC),
		}},
	}
	for _, route := range unserved {
		d.unserved[route] = true
	}
	d.server = httptest.NewServer(http.HandlerFunc(d.serve))
	t.Cleanup(d.server.Close)
	return d
}

func (d *stubTaskDaemon) url() string { return d.server.URL }

func (d *stubTaskDaemon) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if d.unserved[r.URL.Path] {
		d.notFound(w, r)
		return
	}
	decode := func(into any) {
		_ = json.NewDecoder(r.Body).Decode(into)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	switch r.URL.Path {
	case "/v1/health":
		d.healthHits++
		_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.PingResponse{OK: true, Version: d.version}))
	case "/v1/ListTasks":
		d.listHits++
		_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.ListTasksResponse{Tasks: d.tasks}))
	case "/v1/AddTask":
		var req daemon.AddTaskRequest
		decode(&req)
		d.adds = append(d.adds, req)
		_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.AddTaskResponse{OK: true}))
	case "/v1/UpdateTask":
		var req daemon.UpdateTaskRequest
		decode(&req)
		d.updates = append(d.updates, req)
		// Echo the daemon's own merged record, project path and all, so a test can
		// tell the remote answer from a locally reconstructed one.
		merged := d.tasks[0]
		if req.Update.Name != nil {
			merged.Name = *req.Update.Name
		}
		_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.UpdateTaskResponse{Task: merged}))
	case "/v1/RemoveTask":
		var req daemon.RemoveTaskRequest
		decode(&req)
		d.removes = append(d.removes, req)
		_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.RemoveTaskResponse{OK: true}))
	case "/v1/RestartTask":
		var req daemon.RestartTaskRequest
		decode(&req)
		d.restarts = append(d.restarts, req)
		_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.RestartTaskResponse{OK: true}))
	case "/v1/TriggerTask":
		var req daemon.TriggerTaskRequest
		decode(&req)
		d.triggers = append(d.triggers, req)
		_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.TriggerTaskResponse{OK: true}))
	default:
		d.notFoundLocked(w, r)
	}
}

func (d *stubTaskDaemon) notFound(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.notFoundLocked(w, r)
}

// notFoundLocked is byte-for-byte the daemon's own catch-all
// (daemon/httpserver.go): a 404 carrying the envelope, not a bare Go 404 page.
// The client's route-not-served detection keys off that STATUS, so a stub
// answering 500 here would pass a test the real daemon fails.
func (d *stubTaskDaemon) notFoundLocked(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	_ = apiproto.WriteEnvelope(w, apiproto.Failure(fmt.Sprintf("unknown route %q", r.URL.Path)))
}

func (d *stubTaskDaemon) snapshot() stubTaskDaemon {
	d.mu.Lock()
	defer d.mu.Unlock()
	return stubTaskDaemon{
		listHits:   d.listHits,
		healthHits: d.healthHits,
		adds:       append([]daemon.AddTaskRequest(nil), d.adds...),
		updates:    append([]daemon.UpdateTaskRequest(nil), d.updates...),
		removes:    append([]daemon.RemoveTaskRequest(nil), d.removes...),
		restarts:   append([]daemon.RestartTaskRequest(nil), d.restarts...),
		triggers:   append([]daemon.TriggerTaskRequest(nil), d.triggers...),
	}
}

// remoteTargetNames drives every routed case over BOTH spellings an operator
// names a daemon by. They resolve through one apiclient seam, but a routing
// branch written against the flag alone would still pass one and fail the other.
var remoteTargetNames = []string{"flag", "env"}

// useRemoteTarget points this invocation at url through the named spelling, and
// pins the OTHER one empty so an ambient AF_DAEMON_URL on the developer's box
// cannot decide the case.
func useRemoteTarget(t *testing.T, spelling, url string) {
	t.Helper()
	prev := apiclient.FlagDaemonURL
	t.Cleanup(func() { apiclient.FlagDaemonURL = prev })
	switch spelling {
	case "env":
		apiclient.FlagDaemonURL = ""
		t.Setenv("AF_DAEMON_URL", url)
	default:
		apiclient.FlagDaemonURL = url
		t.Setenv("AF_DAEMON_URL", "")
	}
}

// localTaskGuard replaces the six daemon.* indirections with stubs that fail the
// test if entered. They are the ONLY route from a task verb to
// daemon.callDaemon — and therefore to EnsureDaemon, which starts a local daemon
// — so a case that finishes without tripping one has proven both properties the
// issue turns on: no local socket call, and no locally spawned daemon.
func localTaskGuard(t *testing.T) {
	t.Helper()
	trip := func(name string) {
		t.Errorf("the LOCAL control-socket path (%s) was entered for a remote target; "+
			"that path calls EnsureDaemon and would start a daemon on this machine", name)
	}
	origAdd, origUpdate := daemonAddTask, daemonUpdateTask
	origRemove, origRestart := daemonRemoveTask, daemonRestartTask
	origTrigger, origList := daemonTriggerTask, daemonListTasksNoSpawn

	daemonAddTask = func(task.Task, task.Actor) error { trip("daemon.AddTask"); return nil }
	daemonUpdateTask = func(string, task.TaskUpdate, task.ProjectExpectation, task.Actor) (task.Task, error) {
		trip("daemon.UpdateTask")
		return task.Task{}, nil
	}
	daemonRemoveTask = func(string, task.ProjectExpectation) error { trip("daemon.RemoveTask"); return nil }
	daemonRestartTask = func(string, task.ProjectExpectation) error { trip("daemon.RestartTask"); return nil }
	daemonTriggerTask = func(string, task.ProjectExpectation) error { trip("daemon.TriggerTask"); return nil }
	daemonListTasksNoSpawn = func() ([]task.Task, error) { trip("daemon.ListTasksNoSpawn"); return nil, nil }

	t.Cleanup(func() {
		daemonAddTask, daemonUpdateTask = origAdd, origUpdate
		daemonRemoveTask, daemonRestartTask = origRemove, origRestart
		daemonTriggerTask, daemonListTasksNoSpawn = origTrigger, origList
	})
}

// The local task this machine holds. Its id and name are distinctive so a read
// that answered from disk is caught by CONTENT rather than only by count.
const (
	localTaskID   = "localtask1"
	localTaskName = "nightly-on-laptop"
)

// seedLocalTask writes one task to this machine's store and returns the home it
// lives in. Every routed case seeds it: an untouched EMPTY store only proves a
// local write did not CREATE anything, while a seeded one also catches a local
// edit, a local delete, and a read that answered from disk.
func seedLocalTask(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	require.NoError(t, task.AddTask(task.Task{
		ID:          localTaskID,
		Name:        localTaskName,
		Prompt:      "sweep the laptop",
		CronExpr:    "0 4 * * *",
		ProjectPath: filepath.Join(home, "local-project"),
		Program:     "claude",
		Enabled:     true,
	}))
	return home
}

// requireLocalStoreUntouched asserts this machine's task store still holds
// exactly the seeded task, unedited. This is the assertion #3730 is about: the
// pre-fix code wrote HERE while reporting success about another host.
func requireLocalStoreUntouched(t *testing.T, home string) {
	t.Helper()
	local, err := task.LoadTasks()
	require.NoError(t, err, "the local task store must still be readable")
	require.Len(t, local, 1, "the local store must still hold exactly the seeded task")
	require.Equal(t, localTaskID, local[0].ID)
	require.Equal(t, localTaskName, local[0].Name, "the seeded local task must not have been edited")
	require.Equal(t, "0 4 * * *", local[0].CronExpr)
	require.True(t, local[0].Enabled)
	requireNoLocalDaemon(t, home)
}

// requireNoLocalDaemon is the second, independent oracle for "no daemon was
// started here": a daemon that came up binds both of its sockets in the AF home,
// so their absence is evidence that does not depend on the indirection stubs
// above being the only path.
func requireNoLocalDaemon(t *testing.T, home string) {
	t.Helper()
	for _, name := range []string{"daemon.sock", "daemon-http.sock"} {
		if _, err := os.Stat(filepath.Join(home, name)); !os.IsNotExist(err) {
			t.Errorf("a local daemon was started for a remote target: %s exists in %s (stat err: %v)", name, home, err)
		}
	}
}

// setupRemoteCase wires one routed case: a seeded local store, a stub daemon,
// the target spelling under test, and the local-path guard. It returns the home
// and the stub.
func setupRemoteCase(t *testing.T, spelling string, unserved ...string) (string, *stubTaskDaemon) {
	t.Helper()
	home := seedLocalTask(t)
	stub := newStubTaskDaemon(t, "1.9.0", unserved...)
	useRemoteTarget(t, spelling, stub.url())
	localTaskGuard(t)
	resetAddFlags(t)
	resetUpdateFlags(t)
	repoFlag = ""
	return home, stub
}

func TestTasksList_RoutesToTheTargetedDaemon(t *testing.T) {
	for _, spelling := range remoteTargetNames {
		t.Run(spelling, func(t *testing.T) {
			home, stub := setupRemoteCase(t, spelling)

			out := captureStdout(t, func() {
				require.NoError(t, tasksListCmd.RunE(tasksListCmd, nil))
			})

			require.Equal(t, 1, stub.snapshot().listHits, "the targeted daemon must serve the list")
			require.Contains(t, out, stubDaemonTaskName, "the remote daemon's task must reach stdout")
			require.Contains(t, out, stubDaemonProjectPath)
			require.NotContains(t, out, localTaskName,
				"this machine's tasks must never answer a question about the remote daemon")
			requireLocalStoreUntouched(t, home)
		})
	}
}

func TestTasksAdd_RoutesToTheTargetedDaemon(t *testing.T) {
	for _, spelling := range remoteTargetNames {
		t.Run(spelling, func(t *testing.T) {
			home, stub := setupRemoteCase(t, spelling)

			// A path on the DAEMON's host, which does not exist here — the point of
			// #3730's project_path decision. A local resolve would reject it.
			repoFlag = stubDaemonProjectPath
			taskAddNameFlag = "armed-on-box"
			taskAddPromptFlag = "run the nightly sweep"
			taskAddCronFlag = "0 3 * * *"

			out := captureStdout(t, func() {
				require.NoError(t, tasksAddCmd.RunE(tasksAddCmd, nil))
			})

			adds := stub.snapshot().adds
			require.Len(t, adds, 1, "the targeted daemon must receive exactly one add")
			require.Equal(t, "armed-on-box", adds[0].Task.Name)
			require.Equal(t, stubDaemonProjectPath, adds[0].Task.ProjectPath,
				"project_path must reach the daemon as the operator typed it, for the daemon to resolve")
			require.Equal(t, string(task.ActorCLI), adds[0].Actor)
			require.Empty(t, adds[0].Task.Program,
				"an absent --program must defer to the DAEMON's default_program, not resolve this machine's")

			// The success line has to name the machine the schedule now lives on.
			// A path alone reads as this laptop's whichever host it is a path on.
			require.Contains(t, out, stubDaemonProjectPath)
			require.Contains(t, out, stub.url(), "the success line must name the daemon the task was armed on")
			requireLocalStoreUntouched(t, home)
		})
	}
}

func TestTasksGet_RoutesToTheTargetedDaemon(t *testing.T) {
	for _, spelling := range remoteTargetNames {
		t.Run(spelling, func(t *testing.T) {
			home, stub := setupRemoteCase(t, spelling)

			out := captureStdout(t, func() {
				require.NoError(t, tasksGetCmd.RunE(tasksGetCmd, []string{stubDaemonTaskID}))
			})

			require.Equal(t, 1, stub.snapshot().listHits)
			require.Contains(t, out, stubDaemonTaskName)
			require.Contains(t, out, stubDaemonProjectPath)
			requireLocalStoreUntouched(t, home)

			// And the local id is NOT reachable through a remote target — the
			// clearest statement that the two stores did not get merged.
			err := tasksGetCmd.RunE(tasksGetCmd, []string{localTaskID})
			require.Error(t, err, "a local task id must not resolve against a remote daemon")
			require.Contains(t, err.Error(), "not found")
		})
	}
}

func TestTasksShow_RoutesToTheTargetedDaemon(t *testing.T) {
	for _, spelling := range remoteTargetNames {
		t.Run(spelling, func(t *testing.T) {
			home, stub := setupRemoteCase(t, spelling)

			var out bytes.Buffer
			tasksShowCmd.SetOut(&out)
			t.Cleanup(func() { tasksShowCmd.SetOut(nil) })
			require.NoError(t, tasksShowCmd.RunE(tasksShowCmd, []string{stubDaemonTaskID}))

			require.Equal(t, 1, stub.snapshot().listHits)
			rendered := out.String()
			require.Contains(t, rendered, stubDaemonTaskName)
			require.Contains(t, rendered, stubDaemonProjectPath)
			require.Contains(t, rendered, "Daemon", "the page must name the host the record came from")
			require.Contains(t, rendered, stub.url())
			requireLocalStoreUntouched(t, home)
		})
	}
}

func TestTasksRemove_RoutesToTheTargetedDaemon(t *testing.T) {
	for _, spelling := range remoteTargetNames {
		t.Run(spelling, func(t *testing.T) {
			home, stub := setupRemoteCase(t, spelling)

			out := captureStdout(t, func() {
				require.NoError(t, tasksRemoveCmd.RunE(tasksRemoveCmd, []string{stubDaemonTaskID}))
			})

			removes := stub.snapshot().removes
			require.Len(t, removes, 1, "the targeted daemon must receive exactly one remove")
			require.Equal(t, stubDaemonTaskID, removes[0].ID)
			require.Contains(t, out, `"ok"`)
			requireLocalStoreUntouched(t, home)
		})
	}
}

func TestTasksTrigger_RoutesToTheTargetedDaemon(t *testing.T) {
	for _, spelling := range remoteTargetNames {
		t.Run(spelling, func(t *testing.T) {
			home, stub := setupRemoteCase(t, spelling)

			out := captureStdout(t, func() {
				require.NoError(t, tasksRunCmd.RunE(tasksRunCmd, []string{stubDaemonTaskID}))
			})

			triggers := stub.snapshot().triggers
			require.Len(t, triggers, 1, "the targeted daemon must fire the task")
			require.Equal(t, stubDaemonTaskID, triggers[0].ID)
			require.Contains(t, out, `"ok"`)
			requireLocalStoreUntouched(t, home)
		})
	}
}

func TestTasksRestart_RoutesToTheTargetedDaemon(t *testing.T) {
	for _, spelling := range remoteTargetNames {
		t.Run(spelling, func(t *testing.T) {
			home, stub := setupRemoteCase(t, spelling)

			out := captureStdout(t, func() {
				require.NoError(t, tasksRestartCmd.RunE(tasksRestartCmd, []string{stubDaemonTaskID}))
			})

			got := stub.snapshot()
			require.Len(t, got.restarts, 1, "the targeted daemon must restart the watch command")
			require.Equal(t, stubDaemonTaskID, got.restarts[0].ID)
			require.Zero(t, got.healthHits,
				"a successful call must stay one round trip — the version probe is a refusal-path cost only")
			require.Contains(t, out, `"ok"`)
			requireLocalStoreUntouched(t, home)
		})
	}
}

func TestTasksUpdate_RoutesToTheTargetedDaemon(t *testing.T) {
	for _, spelling := range remoteTargetNames {
		t.Run(spelling, func(t *testing.T) {
			home, stub := setupRemoteCase(t, spelling)

			taskUpdateNameFlag = "renamed-on-box"

			out := captureStdout(t, func() {
				require.NoError(t, tasksUpdateCmd.RunE(tasksUpdateCmd, []string{stubDaemonTaskID}))
			})

			updates := stub.snapshot().updates
			require.Len(t, updates, 1, "the targeted daemon must receive exactly one patch")
			require.NotNil(t, updates[0].Update.Name)
			require.Equal(t, "renamed-on-box", *updates[0].Update.Name)
			require.Nil(t, updates[0].Update.Prompt, "only the flags the user passed may ship")
			require.Equal(t, string(task.ActorCLI), updates[0].Actor)

			// The merged record printed is the DAEMON's, which is why the remote
			// project path is in it: a locally reconstructed answer could not have it.
			require.Contains(t, out, "renamed-on-box")
			require.Contains(t, out, stubDaemonProjectPath)
			requireLocalStoreUntouched(t, home)
		})
	}
}

// TestTasksUpdate_SendsTheProjectPathAsTyped pins #3730's project_path decision
// on the one verb that can REBIND a task. The path is interpreted on the
// daemon's host, so the CLI must not expand ~ against the caller's home and must
// not run the local git probe that would reject a perfectly good remote repo.
func TestTasksUpdate_SendsTheProjectPathAsTyped(t *testing.T) {
	home, stub := setupRemoteCase(t, "flag")

	const typed = "~/projects/beta-on-box"
	taskUpdateProjectPathFlag = typed
	require.NoError(t, tasksUpdateCmd.Flags().Set("project-path", typed))

	captureStdout(t, func() {
		require.NoError(t, tasksUpdateCmd.RunE(tasksUpdateCmd, []string{stubDaemonTaskID}))
	})

	updates := stub.snapshot().updates
	require.Len(t, updates, 1)
	require.NotNil(t, updates[0].Update.ProjectPath)
	require.Equal(t, typed, *updates[0].Update.ProjectPath,
		"the daemon resolves the path on ITS filesystem, so it must arrive unexpanded")
	requireLocalStoreUntouched(t, home)
}

// TestTasksAdd_RefusesAnInheritedBindingAgainstARemoteDaemon pins the other half
// of that decision. Without --repo, `tasks add` binds to the CURRENT DIRECTORY's
// repository — a path on this machine that says nothing about the daemon's
// projects — so against a remote target it must refuse rather than guess, the
// same way rule 3 refuses to guess a binding locally.
func TestTasksAdd_RefusesAnInheritedBindingAgainstARemoteDaemon(t *testing.T) {
	home, stub := setupRemoteCase(t, "flag")

	taskAddNameFlag = "unbound"
	taskAddPromptFlag = "p"
	taskAddCronFlag = "0 3 * * *"

	err := tasksAddCmd.RunE(tasksAddCmd, nil)
	require.Error(t, err, "a remote add with no --repo must be refused, not bound to the local cwd")
	require.Contains(t, err.Error(), "--repo is required")
	require.Contains(t, err.Error(), stub.url(), "the refusal must name the daemon it is about")
	require.Empty(t, stub.snapshot().adds, "nothing may reach the daemon")
	requireLocalStoreUntouched(t, home)
}

// TestTasksScope_RefusesRepoAgainstARemoteDaemon pins the scope half. --repo
// becomes an identity by hashing a path on THIS machine, so filtering the
// daemon's tasks by it matches nothing whenever the two hosts hold the project
// at different paths — an empty list for a project that has tasks, which is the
// same silent wrong-host answer #3730 is about.
func TestTasksScope_RefusesRepoAgainstARemoteDaemon(t *testing.T) {
	verbs := map[string]func() error{
		"list":    func() error { return tasksListCmd.RunE(tasksListCmd, nil) },
		"get":     func() error { return tasksGetCmd.RunE(tasksGetCmd, []string{stubDaemonTaskID}) },
		"show":    func() error { return tasksShowCmd.RunE(tasksShowCmd, []string{stubDaemonTaskID}) },
		"remove":  func() error { return tasksRemoveCmd.RunE(tasksRemoveCmd, []string{stubDaemonTaskID}) },
		"trigger": func() error { return tasksRunCmd.RunE(tasksRunCmd, []string{stubDaemonTaskID}) },
		"restart": func() error { return tasksRestartCmd.RunE(tasksRestartCmd, []string{stubDaemonTaskID}) },
		"update":  func() error { return tasksUpdateCmd.RunE(tasksUpdateCmd, []string{stubDaemonTaskID}) },
	}
	for name, run := range verbs {
		t.Run(name, func(t *testing.T) {
			home, stub := setupRemoteCase(t, "flag")
			repoFlag = stubDaemonProjectPath

			err := run()
			require.Error(t, err, "--repo must be refused against a remote daemon, not silently mis-applied")
			require.Contains(t, err.Error(), "--repo cannot scope tasks")
			require.Contains(t, err.Error(), stub.url())
			got := stub.snapshot()
			require.Empty(t, got.adds)
			require.Empty(t, got.updates)
			require.Empty(t, got.removes)
			require.Empty(t, got.restarts)
			require.Empty(t, got.triggers)
			requireLocalStoreUntouched(t, home)
		})
	}
}

// TestTasksRestart_RefusesADaemonThatDoesNotServeTheRoute is the skew case, and
// it is the one where the tempting repair is worst: "fall back to the local
// socket, like the local path does" would restart a watch command on the
// caller's own machine for a request aimed at another. RestartTask is the newest
// of the six task routes (#2359), so it is the one a daemon old enough to be
// missing any of them is missing first.
func TestTasksRestart_RefusesADaemonThatDoesNotServeTheRoute(t *testing.T) {
	home, stub := setupRemoteCase(t, "flag", "/v1/RestartTask")

	err := tasksRestartCmd.RunE(tasksRestartCmd, []string{stubDaemonTaskID})
	require.Error(t, err, "a daemon that does not serve the route must be refused, never written around")
	msg := err.Error()
	require.Contains(t, msg, "does not serve the RestartTask route")
	require.Contains(t, msg, "version 1.9.0", "the refusal must name the daemon's version")
	require.Contains(t, msg, stub.url())
	require.Contains(t, msg, "Nothing was written")
	require.Contains(t, msg, "never falls back to this machine's task store")
	require.Equal(t, 1, stub.snapshot().healthHits,
		"the version in the refusal must be READ from the daemon, not assumed")
	requireLocalStoreUntouched(t, home)
}

// TestTasksList_RefusesADaemonPredatingVersionReporting covers the third answer
// DescribeVersion gives. A daemon that responds but reports no version is older
// than #1044, which is positive evidence about the missing route rather than an
// "unknown" — so the refusal says so instead of collapsing it.
func TestTasksList_RefusesADaemonPredatingVersionReporting(t *testing.T) {
	home := seedLocalTask(t)
	stub := newStubTaskDaemon(t, "", "/v1/ListTasks")
	useRemoteTarget(t, "flag", stub.url())
	localTaskGuard(t)
	resetAddFlags(t)
	repoFlag = ""

	err := tasksListCmd.RunE(tasksListCmd, nil)
	require.Error(t, err, "a read must refuse rather than answer from this machine's store")
	require.Contains(t, err.Error(), "predates version reporting")
	require.Contains(t, err.Error(), "No tasks were read")
	require.NotContains(t, err.Error(), localTaskName)
	requireLocalStoreUntouched(t, home)
}

// TestTasksVerbs_LocalPathIsUnchanged is the control. With no target set, every
// verb must still take the local control-socket indirections — the same calls,
// in the same order, that the pre-#3730 code made — so the routing branch is
// provably inert when nothing asked for it.
func TestTasksVerbs_LocalPathIsUnchanged(t *testing.T) {
	useTempConfig(t)
	resetAddFlags(t)
	resetUpdateFlags(t)
	apiclient.FlagDaemonURL = ""
	t.Setenv("AF_DAEMON_URL", "")
	calls := stubDaemon(t)
	repo := setupAddRepo(t)

	taskAddNameFlag = "local-nightly"
	taskAddPromptFlag = "sweep"
	taskAddCronFlag = "0 3 * * *"
	taskAddProgramFlag = "claude"
	captureStdout(t, func() {
		require.NoError(t, tasksAddCmd.RunE(tasksAddCmd, nil))
	})
	require.Equal(t, 1, calls.writes, "the local add must still dispatch over the control socket")

	stored, err := task.LoadTasks()
	require.NoError(t, err)
	require.Len(t, stored, 1, "the local add must still write this machine's store")
	require.Contains(t, stored[0].ProjectPath, filepath.Base(repo))

	require.NoError(t, tasksRunCmd.RunE(tasksRunCmd, []string{stored[0].ID}))
	require.Equal(t, []string{stored[0].ID}, calls.triggered)
	require.NoError(t, tasksRestartCmd.RunE(tasksRestartCmd, []string{stored[0].ID}))
	require.Equal(t, []string{stored[0].ID}, calls.restarted)

	out := captureStdout(t, func() {
		require.NoError(t, tasksListCmd.RunE(tasksListCmd, nil))
	})
	require.Contains(t, out, "local-nightly")
	require.False(t, strings.Contains(out, "daemon_url"), "the local output shape must be unchanged")
}

// TestTasksShow_RendersTheDaemonsOwnScheduleVerdict pins the other half of
// routing `show`: WHO derives the schedule health.
//
// The local rendering re-derives it from the record and a clock, normalizing to
// now.Location() so it agrees with the scheduler that will fire the task. For a
// REMOTE record that scheduler is on another host in another zone, so
// re-deriving here answers in this terminal's — #3627's finding, live again the
// moment `show` follows the target.
//
// The oracle is differential rather than incidental: the record below carries a
// verdict a client-side derivation CANNOT reach. It says overdue with seven
// missed fires while LastRunAt is now, which any local derivation reads as
// perfectly on schedule. So "overdue · missed 7" can only have come off the
// record, and "on schedule" can only have come from re-deriving.
func TestTasksShow_RendersTheDaemonsOwnScheduleVerdict(t *testing.T) {
	home, stub := setupRemoteCase(t, "flag")

	justRan := time.Now()
	stub.tasks[0].LastRunAt = &justRan
	stub.tasks[0].Overdue = true
	stub.tasks[0].MissedOccurrences = 7

	var out bytes.Buffer
	tasksShowCmd.SetOut(&out)
	t.Cleanup(func() { tasksShowCmd.SetOut(nil) })
	require.NoError(t, tasksShowCmd.RunE(tasksShowCmd, []string{stubDaemonTaskID}))

	rendered := out.String()
	require.Contains(t, rendered, "overdue · missed 7",
		"the verdict must be the daemon's own, read off the record it sent")
	require.NotContains(t, rendered, "on schedule",
		"re-deriving here evaluates the cron in THIS terminal's timezone, not the scheduler's")
	requireLocalStoreUntouched(t, home)
}

// TestTasksShow_RendersARecordedCappedCountAndUnschedulableReason covers the two
// remaining shapes the record carries, both of which have a wrong answer that
// looks fine.
//
// A capped count is a FLOOR, and rendering it as an exact number hands the
// reader a figure that is quietly wrong. An unschedulable verdict must not
// degrade to "on schedule" — including one whose reason this build does not
// recognize, since a newer daemon finding something this client cannot name is
// still a finding.
func TestTasksShow_RendersARecordedCappedCountAndUnschedulableReason(t *testing.T) {
	cases := []struct {
		name  string
		apply func(*task.Task)
		want  string
	}{
		{
			name: "capped count is a floor",
			apply: func(tk *task.Task) {
				tk.Overdue = true
				tk.MissedOccurrences = 10000
				tk.MissedOccurrencesCapped = true
			},
			want: "overdue · missed 10000 or more",
		},
		{
			name: "recorded unschedulable reason",
			apply: func(tk *task.Task) {
				tk.Unschedulable = true
				tk.UnschedulableReason = task.ReasonInvalidExpression
			},
			want: "cron expression is invalid",
		},
		{
			name: "a reason this build does not know is still a finding",
			apply: func(tk *task.Task) {
				tk.Unschedulable = true
				tk.UnschedulableReason = "some-future-reason"
			},
			want: "the daemon reports this task cannot be scheduled: some-future-reason",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home, stub := setupRemoteCase(t, "flag")
			tc.apply(&stub.tasks[0])

			var out bytes.Buffer
			tasksShowCmd.SetOut(&out)
			t.Cleanup(func() { tasksShowCmd.SetOut(nil) })
			require.NoError(t, tasksShowCmd.RunE(tasksShowCmd, []string{stubDaemonTaskID}))

			require.Contains(t, out.String(), tc.want)
			require.NotContains(t, out.String(), "on schedule")
			requireLocalStoreUntouched(t, home)
		})
	}
}
