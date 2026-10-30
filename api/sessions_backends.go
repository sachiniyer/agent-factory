package api

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
)

// listBackendsViaDaemon is the SAME catalog the web's backend picker renders
// (web/src/backends.ts over POST /v1/ListBackends). Both transports dispatch the
// one daemon method — daemon/httproutes.go wires the HTTP route to
// controlServer.ListBackends and the gob control socket exposes that same method
// — so the CLI cannot report a different catalog than the web, and a backend
// added server-side reaches both with no client change. Held in a var so tests
// can answer without a live daemon, like the other *ViaDaemon seams.
var listBackendsViaDaemon = routedListBackends

var sessionsBackendsCmd = &cobra.Command{
	Use:   "backends",
	Short: "List the runtimes this project can create sessions on",
	Long: `Report which backends 'af sessions create --backend' accepts for this
project, whether each one is usable as the project is configured right now, and
which backend a create with no --backend resolves to.

--backend names the enum, but knowing that "docker" is spelled correctly is not
the same as knowing this project can use it. The daemon checks each backend's
preconditions against the project's config and answers one of three things:

  available    every precondition that can be checked was checked and passed
  unavailable  a precondition FAILED — a create would fail; "reason" says what to fix
  unknown      the preconditions could NOT be evaluated (e.g. the project's
               config would not parse) — neither yes nor no is honest, so
               "reason" says what stopped the check

"unknown" is deliberately not folded into either of the other two: reporting an
unchecked backend as available is a promise nobody verified.

"default" is the backend a create with no --backend resolves to here. It is
EMPTY when the project's 'backend' config key names something unrecognized —
such a create fails rather than quietly running local, and "default_reason"
names the offending value.

The reasons are the same text a create prints when it refuses, because both come
from the same precondition checks.

An available backend may also carry "callback_unavailable_reason": a create on
it succeeds but the session gets no af callback, because the daemon cannot mint
a credential while its dedicated sandbox callback endpoint is not running. It is
advisory, not a precondition — the session still works, it just cannot call back
into af.

Example:
  af sessions backends
  af sessions backends --repo ~/src/myproject`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		// resolveRepo is the resolver `sessions create` itself uses, so the
		// catalog describes the project a create run here would bind to —
		// including its "--repo is required" and af-home refusals. A separate
		// resolver could answer about a different project than the create it is
		// meant to predict.
		repoPath, err := backendsRepoPath()
		if err != nil {
			return jsonError(err)
		}

		resp, err := listBackendsViaDaemon(daemon.ListBackendsRequest{RepoPath: repoPath})
		if err != nil {
			return jsonError(err)
		}
		return jsonOut(resp)
	},
}

// backendsRepoPath is the project path the catalog is computed for. Locally it
// is resolveRepo's root. Against a remote target the path is sent AS TYPED —
// the daemon resolves it on its own filesystem, and resolving it here would
// expand ~ against the caller's home and reject a path only the daemon has.
// --repo is required there: this machine's cwd names no project the daemon has.
func backendsRepoPath() (string, error) {
	if apiclient.IsRemoteTarget() {
		if strings.TrimSpace(repoFlag) == "" {
			return "", fmt.Errorf("--repo is required against the daemon at %s: the catalog is computed for a project path on the DAEMON's host. "+
				"Pass --repo <path on the daemon host>", apiclient.RemoteTargetURL())
		}
		return repoFlag, nil
	}
	repo, err := resolveRepo()
	if err != nil {
		return "", err
	}
	return repo.Root, nil
}
