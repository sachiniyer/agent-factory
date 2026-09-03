package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/task"
)

// Routing every `af tasks` verb to the targeted daemon (#3730).
//
// --daemon-url/--token are PERSISTENT root flags, so cobra advertises them under
// all eight task verbs, and until this file NOTHING in the group read them. The
// verbs bound straight to daemon.AddTask/UpdateTask/RemoveTask/RestartTask/
// TriggerTask/ListTasksNoSpawn, which dial the LOCAL unix socket — and the
// mutating four go through callDaemon, which calls EnsureDaemon first. So
//
//	af tasks add --daemon-url http://box:8443 --name nightly --cron '0 3 * * *' …
//
// armed a cron task on the operator's own laptop, printed a success line naming
// a local project path, and on a laptop with no daemon SPAWNED one — a daemon on
// the machine the operator was explicitly steering away from. The `af config`
// group had the same shape for a file write and was routed in #3679/#3704; here
// the mutation is durable scheduled execution, which is why routing rather than
// refusing is the answer (#3730 triage).
//
// Four properties this file exists to hold. Each is a way to get it wrong, and
// three of them are things the LOCAL path deliberately does:
//
//   - A remote target NEVER falls back to the local socket or the local task
//     store. The local read path's disk fallback is right for the local socket
//     (`af tasks list` must work with no daemon running) and a lie for a remote
//     one: it would answer a question about box with rows from this laptop. So
//     every remote failure — including "that daemon is too old to serve the
//     route" — is a refusal.
//   - A remote target NEVER spawns a local daemon. EnsureDaemon is the local
//     path's business, and the only way to reach it from these verbs is the
//     daemon.* indirections in tasks.go, which the remote branches never touch.
//   - project_path is interpreted on the DAEMON's host. A path resolves against
//     a filesystem this client cannot see, so the CLI sends what the operator
//     typed and lets the daemon resolve it (task.AddTaskChecked derives RepoID
//     from it there). Resolving it here — through config.ResolveUserPath and
//     git — would expand ~ against the caller's home and reject a path that
//     exists perfectly well on box.
//   - Nothing about the LOCAL path changes. Every routed helper below tests the
//     target first and hands the unchanged local call back untouched.
//
// The routing decision lives HERE, above both layers, for the reason #3704 gives:
// apiclient imports daemon, so daemon cannot ask which target is configured, and
// apiclient's methods are thin parity twins of the daemon's handlers rather than
// policy. This is the api package's copy of that seam because the task verbs
// live here, not in commands/.
//
// What this does NOT route is the project SCOPE. --repo becomes an identity by
// hashing a path on this machine, so it cannot filter a remote daemon's tasks;
// resolveProjectScope refuses it against a remote target rather than silently
// matching nothing. See api/scope.go.

// remoteTaskClient builds the client for the targeted daemon. Callers reach it
// only inside an apiclient.IsRemoteTarget() branch, so it never constructs a
// local-socket client by accident — a local client here would be the fallback
// this file exists to forbid, wearing the routed path's clothes.
func remoteTaskClient() (*apiclient.Client, error) {
	if !apiclient.IsRemoteTarget() {
		return nil, errors.New("internal: remote task client requested with no remote target")
	}
	return apiclient.NewTargeted()
}

// remoteListTasks reads the targeted daemon's task list. It is the read half of
// every routed verb: `list` filters it, and `get`/`show`/the update pre-check
// select one id out of it, because ListTasks is the only task READ the daemon
// serves — there is no per-id route to call instead.
//
// No disk fallback, unlike listTasks' local path. A remote read that fails has
// nothing local to fall back TO: this machine's tasks.json describes this
// machine's schedule, and answering a question about box with it is the same
// wrong-host lie for a read that #3730 is about for a write.
func remoteListTasks(verb string) ([]task.Task, error) {
	client, err := remoteTaskClient()
	if err != nil {
		return nil, err
	}
	defer client.CloseIdleConnections()

	tasks, err := client.ListTasks(context.Background())
	if err != nil {
		return nil, remoteTaskError(client, verb, "ListTasks", err, remoteNothingRead)
	}
	return tasks, nil
}

// remoteTaskByID selects one of the targeted daemon's tasks. The not-found
// wording matches the local path's (task.GetTask / getTaskByID) so the only
// thing that differs between transports is WHICH store was searched.
func remoteTaskByID(verb, id string) (*task.Task, error) {
	tasks, err := remoteListTasks(verb)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		if tasks[i].ID == id {
			return &tasks[i], nil
		}
	}
	return nil, fmt.Errorf("task with id %q not found", id)
}

// The routed mutations. Each is the shape of its daemon.* counterpart, so the
// call sites in tasks.go read the same over both transports and keep their
// existing committed-mutation handling: apiclient.IsMutationCommitted classifies
// the daemon's shared committed code over HTTP exactly as it does over the
// control socket, so a reload failure after a durable remote write is still
// reported as "saved, do not retry" rather than as a plain failure.

func routedAddTask(t task.Task, actor task.Actor) error {
	if !apiclient.IsRemoteTarget() {
		return daemonAddTask(t, actor)
	}
	client, err := remoteTaskClient()
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	if err := client.AddTask(t, actor); err != nil {
		return remoteTaskError(client, "af tasks add", "AddTask", err, remoteNothingWritten)
	}
	return nil
}

func routedUpdateTask(id string, patch task.TaskUpdate, expect task.ProjectExpectation, actor task.Actor) (task.Task, error) {
	if !apiclient.IsRemoteTarget() {
		return daemonUpdateTask(id, patch, expect, actor)
	}
	client, err := remoteTaskClient()
	if err != nil {
		return task.Task{}, err
	}
	defer client.CloseIdleConnections()
	updated, err := client.UpdateTask(id, patch, expect, actor)
	if err != nil {
		return task.Task{}, remoteTaskError(client, "af tasks update", "UpdateTask", err, remoteNothingWritten)
	}
	return updated, nil
}

func routedRemoveTask(id string, expect task.ProjectExpectation) error {
	if !apiclient.IsRemoteTarget() {
		return daemonRemoveTask(id, expect)
	}
	client, err := remoteTaskClient()
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	if err := client.RemoveTask(id, expect); err != nil {
		return remoteTaskError(client, "af tasks remove", "RemoveTask", err, remoteNothingWritten)
	}
	return nil
}

func routedRestartTask(id string, expect task.ProjectExpectation) error {
	if !apiclient.IsRemoteTarget() {
		return daemonRestartTask(id, expect)
	}
	client, err := remoteTaskClient()
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	if err := client.RestartTask(id, expect); err != nil {
		return remoteTaskError(client, "af tasks restart", "RestartTask", err, remoteNothingWritten)
	}
	return nil
}

func routedTriggerTask(id string, expect task.ProjectExpectation) error {
	if !apiclient.IsRemoteTarget() {
		return daemonTriggerTask(id, expect)
	}
	client, err := remoteTaskClient()
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	if err := client.TriggerTask(id, expect); err != nil {
		return remoteTaskError(client, "af tasks trigger", "TriggerTask", err, remoteNothingFired)
	}
	return nil
}

// The "and here is what did NOT happen" clause of the route refusal below. It is
// spelled per verb class because the reassurance a reader needs differs: a
// mutation's reader wants to know their schedule was not changed on either host,
// a read's reader wants to know the rows they did not get were not quietly
// replaced by local ones.
const (
	remoteNothingWritten = "Nothing was written — af never falls back to this machine's task store for a remote target."
	remoteNothingFired   = "Nothing was fired — af never falls back to this machine's task store for a remote target."
	remoteNothingRead    = "No tasks were read — af never falls back to this machine's task store for a remote target."
)

// remoteTaskError passes a remote failure through untouched, except for the one
// failure whose raw form is unactionable: a daemon that does not serve the route
// at all.
//
// `daemon does not serve /v1/RestartTask (it answered 404: …)` names a path the
// operator never typed and says nothing about what changed, what did not, or
// what to do — and the tempting repair, "fall back to the local socket the way
// the local path does", is the whole defect #3730 is about. So: name the verb, the
// daemon and its version, state plainly that nothing happened, and give the two
// ways forward.
//
// The classification is sound rather than a guess: the daemon's rpcHandler
// answers 200/400/405/413/500/503 and never 404, so a 404 on /v1/<Method> can
// only be the mux catch-all — the route is absent from THAT daemon, and nothing
// ran server-side. See apiclient.RouteNotServedError.
//
// RestartTask is the likeliest of the six to hit this: it is the newest task
// route (#2359), so a daemon old enough to be missing any of them is missing it
// first.
func remoteTaskError(client *apiclient.Client, verb, route string, err error, nothing string) error {
	if !apiclient.IsRouteNotServed(err) {
		return err
	}
	return fmt.Errorf(
		"%s cannot reach the tasks of the daemon at %s: that daemon (%s) does not serve the %s route. "+
			"%s Upgrade the daemon, or run %s on the daemon host",
		verb, apiclient.RemoteTargetURL(), client.DaemonVersionPhrase(context.Background()), route,
		nothing, verb)
}

// addProjectBinding resolves `af tasks add`'s project binding for whichever
// daemon this invocation targets.
//
// Locally it is resolveRepo, unchanged: --repo or the current directory's
// repository, resolved through git to the main-worktree root and checked against
// the stray-clone-inside-af's-home refusal (#1891). The *config.RepoContext it
// returns is what the caller reads the repo's default_program from.
//
// Against a REMOTE daemon there is no RepoContext to return — every field of one
// is an answer about this machine's filesystem — so it returns nil beside the
// operator-typed path, and the caller defers the program default to the daemon.
// The #1891 home guard is not lost by that: it only ever fires on a binding
// INHERITED from the cwd (guardProjectBinding's explicit escape hatch), and the
// remote path has no inherited binding to guard — --repo is required there.
func addProjectBinding() (*config.RepoContext, string, error) {
	if apiclient.IsRemoteTarget() {
		// --repo is REQUIRED here rather than inherited. Against a remote target
		// the current directory names a repository on THIS machine, which says
		// nothing about the daemon's projects, so binding a remote task to it would
		// schedule agent runs and worktrees against a path box may not have — or,
		// worse, against a path it has and the operator did not mean. That is rule 3
		// of the shared project contract (a command that BINDS never guesses)
		// applied across the network.
		if strings.TrimSpace(repoFlag) == "" {
			return nil, "", fmt.Errorf(
				"--repo is required against the daemon at %s: a task's project path is resolved on the DAEMON's host, "+
					"and this machine's current directory names a repository the daemon may not have. "+
					"Pass --repo <path on the daemon host>",
				apiclient.RemoteTargetURL())
		}
		// Sent AS TYPED. Only a whitespace-only value is rejected — the same rule
		// --project-path already applies locally, and for the same reason: a
		// directory name may legitimately carry leading or trailing spaces, and the
		// shell already made the operator quote one to get it here. Nothing else is
		// resolved, because every resolution available here runs against the wrong
		// filesystem: config.ResolveUserPath would expand ~ to the CALLER's home,
		// and config.RepoFromPath would reject a path that is a perfectly good git
		// repository on the daemon's host.
		return nil, repoFlag, nil
	}
	repo, err := resolveRepo()
	if err != nil {
		return nil, "", err
	}
	return repo, repo.Root, nil
}

// updateTaskRecord reads the record `af tasks update` needs from the host the
// update will land on — twice: once before the write, for the cross-field
// pre-checks, and once after a COMMITTED-but-unreported write, to recover the
// output.
//
// Locally that is task.GetTask, the disk read the command has always done.
// Against a remote target it is that daemon's own record, because reading this
// machine's store there would pre-check a remote patch against a local task's
// prompt and trigger — and, on the committed path, print a LOCAL task as the
// value a remote write produced. It also keeps the local branch on task.GetTask
// rather than on getTaskByID, so the local error wording is byte-identical to
// what it was before routing.
func updateTaskRecord(id string) (*task.Task, error) {
	if apiclient.IsRemoteTarget() {
		return remoteTaskByID("af tasks update", id)
	}
	return task.GetTask(id)
}
