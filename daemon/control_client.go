package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/rpc"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sockpath"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

var ensureDaemonMu sync.Mutex

// daemonStartingErrText is the wire-visible text of the warm-up error. net/rpc
// flattens server-side errors into plain strings, so clients cannot errors.Is
// against a sentinel value; IsDaemonStartingErr matches this text instead.
const daemonStartingErrText = "agent-factory daemon is starting (restoring sessions); retry shortly"

// daemonUpgradeProbationErrText is the stable portion of the wire-visible
// probation refusal. The transaction ID follows it for diagnosis, so clients
// match the prefix rather than one complete dynamic string.
const daemonUpgradeProbationErrText = "agent-factory daemon is validating an upgrade"

// errDaemonStarting is returned by state-dependent RPC handlers in the window
// between the control-socket bind and the completion of the instance restore
// (#829). The socket now binds before the restore so this window is observable
// without admitting state-dependent work against a partial instance map.
func errDaemonStarting() error {
	return errors.New(daemonStartingErrText)
}

func errDaemonUpgradeProbation(transactionID string) error {
	return fmt.Errorf("%s (transaction %s); retry shortly", daemonUpgradeProbationErrText, transactionID)
}

// daemonQuiescingErrText is the stable wire prefix for a mutation refused because
// the daemon is quiescing to hand off to a validated upgrade candidate: it has
// stopped admitting new work and is about to exit so the candidate can bind the
// socket. Retryable — the client should reach the new daemon that takes over.
const daemonQuiescingErrText = "agent-factory daemon is handing off to an upgrade"

func errDaemonQuiescing() error {
	return errors.New(daemonQuiescingErrText + "; retry shortly")
}

// IsDaemonQuiescingErr reports whether a mutation was refused because the daemon is
// quiescing for an upgrade hand-off. net/rpc flattens the server error to text, so
// this matches the stable wire prefix like the other admission classifiers.
func IsDaemonQuiescingErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), daemonQuiescingErrText)
}

// IsDaemonStartingErr reports whether an RPC client error means the daemon is
// up but still restoring instances. Callers should treat it as retryable: the
// daemon is alive (EnsureDaemon's ping succeeds, so it must NOT spawn another)
// and the same request succeeds once the restore finishes.
func IsDaemonStartingErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), daemonStartingErrText)
}

// IsDaemonUpgradeProbationErr reports whether a daemon mutation was refused
// because a candidate is restored but its previous-binary supervisor has not
// released admission yet. Like the warm-up error, net/rpc flattens the server
// error to text, so this classifier matches the stable wire prefix.
func IsDaemonUpgradeProbationErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), daemonUpgradeProbationErrText)
}

// IsDaemonAdmissionRetryable reports whether err is a lifecycle admission
// refusal expected to clear without restarting the daemon. Both the net/rpc
// and HTTP/TUI transports use this predicate so a new admission phase cannot
// become retryable on one transport while failing immediately on the other.
func IsDaemonAdmissionRetryable(err error) bool {
	return IsDaemonStartingErr(err) || IsDaemonUpgradeProbationErr(err) || IsDaemonQuiescingErr(err)
}

// DaemonSocketPath returns the Unix socket path used by the local control
// plane.
//
// The length check happens HERE, where the path is resolved, rather than at
// net.Listen: every client and the daemon itself route through this function,
// so one check covers dialling and binding, and it fires before a listener has
// half-started. An over-long path otherwise fails inside the kernel as a bare
// "bind: invalid argument" that names neither the path, the limit, nor
// AGENT_FACTORY_HOME — the knob that fixes it (#1940).
func DaemonSocketPath() (string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, daemonSocketFileName)
	if err := sockpath.Check("daemon control socket", path); err != nil {
		return "", err
	}
	return path, nil
}

// EnsureDaemon starts the daemon if the control socket is not already serving.
func EnsureDaemon() error {
	return ensureDaemonWithLauncherUntil(launchDaemonProcessFn, time.Time{})
}

var launchDaemonProcessAtFn = launchDaemonProcessAt

// EnsureDaemonFromPath starts the daemon from execPath if the control socket is
// not already serving. It is used by post-upgrade restart paths after the
// current process's executable may have been replaced on disk: asking the
// still-running old process for os.Executable can resolve to a deleted inode,
// while execPath is the freshly written binary path the new daemon must run.
func EnsureDaemonFromPath(execPath string) error {
	return ensureDaemonWithPolicyUntil(func() error {
		return launchDaemonProcessAtFn(execPath)
	}, false, time.Time{})
}

func ensureDaemonWithLauncher(launch func() error) error {
	return ensureDaemonWithLauncherUntil(launch, time.Time{})
}

func ensureDaemonWithLauncherUntil(launch func() error, deadline time.Time) error {
	return ensureDaemonWithPolicyUntil(launch, true, deadline)
}

func ensureDaemonWithPolicyUntil(launch func() error, preferUnit bool, deadline time.Time) error {
	if !lockEnsureDaemonUntil(deadline) {
		return daemonAdmissionDeadlineError()
	}
	defer ensureDaemonMu.Unlock()

	if err := pingDaemonUntil(deadline); err == nil {
		return nil
	}
	if admissionDeadlineExpired(deadline) {
		return daemonAdmissionDeadlineError()
	}
	// #2212 R1: before spawning a daemon, defer to an in-progress upgrade rather
	// than racing its recovery actor with a rival daemon. A client defers to BOTH
	// a forward upgrade and a rollback restoring the previous daemon — in the
	// latter it must not stop and replace the very daemon the actor is validating.
	// Fail-open — only a provably live upgrade stops the spawn, as a typed
	// retryable error; a stale, corrupt, or absent journal proceeds. The gate is
	// bounded, so a bad journal can never wedge this launch path (which fronts
	// every af invocation).
	if homeDir, ok := configHomeDir(); ok {
		switch decision, gateErr := checkUpgradeGateUntil(homeDir, false, deadline); decision {
		case upgradeGateInProgress, upgradeGateRestoringPrevious:
			return gateErr
		}
	}
	if admissionDeadlineExpired(deadline) {
		return daemonAdmissionDeadlineError()
	}
	if preferUnit {
		configDir, configErr := config.GetConfigDir()
		if configErr != nil {
			log.WarningLog.Printf("could not resolve AF home while choosing daemon supervisor; using ad-hoc launch: %v", configErr)
		} else {
			owner, ownerErr := ResolveSupervisionOwner(configDir)
			switch {
			case ownerErr != nil:
				log.WarningLog.Printf("could not determine daemon supervision owner; using ad-hoc launch: %v", ownerErr)
			case owner == OwnerUnit:
				return ensureDaemonThroughUnitUntil(launch, deadline)
			}
		}
	}
	return ensureDaemonAdHocUntil(launch, deadline)
}

func ensureDaemonThroughUnitUntil(launch func() error, deadline time.Time) error {
	// The ONLY condition under which an ad-hoc launch remains legitimate on a
	// unit-claimed home: the service manager the installed unit belongs to
	// provably cannot exist in this environment — no systemd/launchd as init,
	// or a platform with no autostart support. There is no supervision to
	// escape, and the fallback keeps af usable on systemd-less boxes and
	// containers (#2373). Every other outcome below — a refused, hung,
	// bus-unreachable, or UNINVOKABLE start — instead returns an error: a
	// manager that COULD run the unit makes an ad-hoc spawn an unsupervised
	// escapee that outlives the transient failure, greets the unit's next
	// start with an already-served socket (whose ExecStart exits 0 by
	// design), and leaves the daemon permanently outside Restart=on-failure
	// protection (#4470).
	presence, probeErr := probeUnitSupervisor(autostartGOOS)
	switch presence {
	case supervisorAbsent:
		if admissionDeadlineExpired(deadline) {
			return daemonAdmissionDeadlineError()
		}
		log.WarningLog.Printf("installed daemon service cannot run in this environment; falling back to an ad-hoc daemon: %v", probeErr)
		if err := ensureDaemonAdHocUntil(launch, deadline); err != nil {
			return fmt.Errorf("installed daemon service unavailable: %v; ad-hoc fallback failed: %w", probeErr, err)
		}
		// The ad-hoc fallback brought up a reachable daemon. EnsureDaemon's
		// contract is "daemon reachable when I return nil", and every caller
		// (callDaemon, withDaemonHTTP, attach) hard-returns on a non-nil
		// result and skips the RPC — so a non-nil supervision-degradation
		// return after a working fallback broke the first client action on
		// hosts without a systemd user bus, self-healing only on the second
		// call (#2373). The degradation is still surfaced where the user
		// looks: the warning above, and af doctor / af daemon status carry a
		// supervision-owner row. Report success.
		return nil
	case supervisorUnreachable:
		// The manager provably runs this system — only this process's ability
		// to invoke it failed (e.g. a PATH that omits the binary). `af daemon
		// adopt` would hit the same wall, so the remedy is an environment
		// that can reach the manager.
		return unreachableSupervisorRefusal(autostartGOOS, probeErr)
	}

	unitDeadline := admissionBoundedDeadline(deadline, ensureUnitStartTimeout)
	if startErr := runEnsureUnitStartCommand(unitDeadline); startErr != nil {
		return unitStartRefusal(autostartGOOS, startErr)
	}
	// The manager accepted the start — but "accepted" is not "serving":
	// after an on-failure kill the unit holds ExecStart for RestartSec, so a
	// serving socket can legitimately be several seconds out. Wait on the
	// whole readiness budget, not the bounded start share: an ad-hoc spawn
	// in this window races the pending ExecStart to the socket, wins it as
	// an unsupervised process, and is exactly the escape that left the unit
	// inactive while an impostor served the home for hours (#4470).
	if err := waitForUnitDaemonReady(deadline); err != nil {
		return unitReadinessRefusal(autostartGOOS, err)
	}
	return nil
}

func ensureDaemonAdHocUntil(launch func() error, deadline time.Time) error {
	if admissionDeadlineExpired(deadline) {
		return daemonAdmissionDeadlineError()
	}
	// A previous daemon version may have a PID file but no control socket. Stop
	// it before launching the control-plane daemon so we do not run duplicate
	// scheduler and session-monitor loops. StopDaemon is also how an
	// alive-but-unreachable daemon
	// (its control socket removed/corrupted) is reclaimed: it SIGTERMs the
	// holder, which releases the per-home lock on exit, so the launch below
	// acquires a free lock. That reclaim-then-respawn is what keeps auto-start
	// within the singleton invariant — the freshly spawned daemon binds only
	// once the previous one is gone. A spurious spawn that races a still-live
	// holder can never become a second daemon: the child fails fast on the
	// exclusive startup lock (see RunDaemon / acquireHomeLock).
	if _, err := stopDaemonUntil(deadline); err != nil {
		log.WarningLog.Printf("failed to stop stale daemon before launch: %v", err)
	}
	if admissionDeadlineExpired(deadline) {
		return daemonAdmissionDeadlineError()
	}

	// No auth-posture pre-flight here any more (#2168 Phase 0). This used to load
	// the config and return the #2090 refusal before spawning, because a spawned
	// daemon's stderr is discarded (startDaemonChild) and the refusal would
	// otherwise reach the user only as the 5s "did not become ready" timeout
	// below. There is nothing left to pre-flight: a tokenless network bind starts
	// and serves, so this path can no longer predict a startup failure — and
	// keeping the check would turn the very config the owner chose to allow into
	// an `af` that refuses to run at all.
	//
	// The exposure is still reported, on surfaces the user is actually looking
	// at: `af config set` warns at write time, the daemon warns once when the
	// listener binds (startHTTPServer), and `af doctor` / `af daemon status`
	// carry a row for it.

	if err := launch(); err != nil {
		return err
	}
	if admissionDeadlineExpired(deadline) {
		return daemonAdmissionDeadlineError()
	}

	err := waitForDaemonReady(admissionBoundedDeadline(deadline, daemonReadyTimeout))
	if err != nil && admissionDeadlineExpired(deadline) {
		// Same deadline-identity rule as the unit path: a readiness wait that
		// consumed the whole admission window must still read as a deadline to
		// callDaemon's lifecycle-fallback guard (Codex on #4475).
		return fmt.Errorf("%w: %w", err, context.DeadlineExceeded)
	}
	return err
}

func waitForDaemonReady(deadline time.Time) error {
	var lastErr error
	for time.Now().Before(deadline) {
		if err := pingDaemonUntil(deadline); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if !waitUntilAdmissionDeadline(deadline, 50*time.Millisecond) {
			break
		}
	}
	if lastErr == nil {
		lastErr = errors.New("readiness deadline elapsed before the daemon could be probed")
	}
	return fmt.Errorf("daemon did not become ready: %w", lastErr)
}

func pingDaemon() error {
	_, err := pingDaemonResponse()
	return err
}

func pingDaemonUntil(deadline time.Time) error {
	var resp PingResponse
	return callDaemonNoEnsureBefore("Ping", PingRequest{}, &resp, deadline, true)
}

// pingDaemonResponse pings the daemon and returns its full reply, so callers
// that need the reported version (`af doctor`'s skew check) read it from the
// same probe that establishes liveness. Never ensures a daemon: doctor is
// read-only and must not spawn the thing it is diagnosing.
func pingDaemonResponse() (PingResponse, error) {
	var resp PingResponse
	err := callDaemonNoEnsure("Ping", PingRequest{}, &resp)
	return resp, err
}

func callDaemonNoEnsure(method string, req any, resp any) error {
	return callDaemonNoEnsureUntil(method, req, resp, time.Time{})
}

type daemonCallAttempt struct {
	err            error
	requestStarted bool
}

// callDaemonNoEnsureUntil bounds only the retry dial. Once a daemon accepts the
// RPC, handler execution keeps its historical method-specific lifetime: session
// creation and other legitimate operations may outlive the admission window.
func callDaemonNoEnsureUntil(method string, req any, resp any, deadline time.Time) error {
	return callDaemonNoEnsureAttemptBefore(method, req, resp, deadline, false).err
}

func callDaemonNoEnsureBefore(method string, req any, resp any, deadline time.Time, boundRPC bool) error {
	return callDaemonNoEnsureAttemptBefore(method, req, resp, deadline, boundRPC).err
}

// callDaemonNoEnsureAttemptUntil retains whether net/rpc started the request.
// A failed dial proves that the handler never ran and is therefore the only
// transport failure callDaemon may replay. Once Client.Call starts, a lost
// response is ambiguous: the handler may already have committed its mutation.
func callDaemonNoEnsureAttemptUntil(method string, req any, resp any, deadline time.Time) daemonCallAttempt {
	return callDaemonNoEnsureAttemptBefore(method, req, resp, deadline, false)
}

func callDaemonNoEnsureAttemptBefore(method string, req any, resp any, deadline time.Time, boundRPC bool) daemonCallAttempt {
	socketPath, err := DaemonSocketPath()
	if err != nil {
		return daemonCallAttempt{err: err}
	}
	dialTimeout := daemonDialTimeout
	if !deadline.IsZero() {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return daemonCallAttempt{err: daemonAdmissionDeadlineError()}
		}
		if remaining < dialTimeout {
			dialTimeout = remaining
		}
	}
	conn, err := net.DialTimeout("unix", socketPath, dialTimeout)
	if err != nil {
		return daemonCallAttempt{err: err}
	}
	if boundRPC && !deadline.IsZero() {
		if err := conn.SetDeadline(deadline); err != nil {
			_ = conn.Close()
			return daemonCallAttempt{err: err}
		}
	}
	client := rpc.NewClient(conn)
	defer client.Close()
	if err := client.Call(controlServiceName+"."+method, req, resp); err != nil {
		// An erroring handler sends NO response body, so the envelope below
		// never runs for handlers that answer with an error -- the task
		// mutations do. Recognise an older daemon's committed marker here so a
		// new CLI does not read "failed" and retry a durable task change into a
		// duplicate. Skew-only; see committedFromLegacyRPCError.
		if committed := committedFromLegacyRPCError(err); committed != nil {
			return daemonCallAttempt{err: committed, requestStarted: true}
		}
		return daemonCallAttempt{err: err, requestStarted: true}
	}
	// A committed mutation answers OK and reports itself in the response
	// envelope: net/rpc reduces a concrete error to rpc.ServerError and keeps
	// only its string, so the marker cannot ride in the error. Read once,
	// generically, for any response embedding MutationOutcome — which replaced
	// reconstructing it from per-method message prefixes (#3036).
	if carrier, ok := resp.(interface {
		CommittedOutcome() (bool, string)
	}); ok {
		if committed, warning := carrier.CommittedOutcome(); committed {
			return daemonCallAttempt{
				err:            &rpcMutationCommittedError{err: errors.New(warning)},
				requestStarted: true,
			}
		}
	}
	return daemonCallAttempt{requestStarted: true}
}

// committedPrefixes are the wire strings a pre-#3036 daemon uses to mark a
// durable-but-incomplete task mutation. This is NOT the deleted per-method
// classifier: it is one shared, method-agnostic check over the SAME shared
// vocabulary, reached only when no response body exists to carry the envelope.
// Delete it once the oldest supported daemon reports through the envelope.
var committedPrefixes = []string{
	taskAddCommittedErrorPrefix,
	taskUpdateCommittedErrorPrefix,
	taskRemoveCommittedErrorPrefix,
}

// committedFromLegacyRPCError recognises an older daemon's committed outcome,
// which arrives as a flattened rpc.ServerError with no structure left (#2512).
func committedFromLegacyRPCError(err error) error {
	if err == nil {
		return nil
	}
	var srv rpc.ServerError
	if !errors.As(err, &srv) {
		return nil
	}
	for _, prefix := range committedPrefixes {
		if strings.HasPrefix(string(srv), prefix) {
			return &rpcMutationCommittedError{err: err}
		}
	}
	return nil
}

type rpcMutationCommittedError struct{ err error }

var _ apiproto.MutationCommittedError = (*rpcMutationCommittedError)(nil)

func (e *rpcMutationCommittedError) Error() string           { return e.err.Error() }
func (e *rpcMutationCommittedError) Unwrap() error           { return e.err }
func (e *rpcMutationCommittedError) MutationCommitted() bool { return true }

// CreateSession asks the daemon to create, start, and persist a session.
func CreateSession(req CreateSessionRequest) (*session.InstanceData, error) {
	var resp CreateSessionResponse
	err := callDaemon("CreateSession", req, &resp)
	// callDaemon classifies the committed outcome generically; keep the payload
	// on that path — a retained failed create (#3233) still has a durable row
	// the CLI may need to report. Only a clean failure has nothing to return.
	if err != nil && !isMutationCommitted(err) {
		return nil, err
	}
	return &resp.Instance, err
}

// ListBackends asks the daemon which runtimes a create against req.RepoPath may
// select, and which one an unspecified create resolves to. It is the read that
// makes CreateSession's Backend field choosable rather than guessable, and it
// answers from the same controlServer.ListBackends the web's picker reaches over
// /v1/ListBackends — one catalog, two transports, so the CLI and the web can
// never describe a repo's backends differently.
//
// Read-only: it provisions nothing and dials nothing. Like every other CLI
// control call it goes to the LOCAL daemon, which is the daemon that would serve
// the create it describes.
func ListBackends(req ListBackendsRequest) (ListBackendsResponse, error) {
	var resp ListBackendsResponse
	if err := callDaemon("ListBackends", req, &resp); err != nil {
		return ListBackendsResponse{}, err
	}
	return resp, nil
}

// CreateTab asks the daemon to spawn, persist, and report a new tab on an
// existing session. Returning the response object keeps the daemon-minted ID
// together with its resolved and tmux names across the gob transport.
func CreateTab(req CreateTabRequest) (CreateTabResponse, error) {
	var resp CreateTabResponse
	err := callDaemon("CreateTab", req, &resp)
	// callDaemon classifies the committed outcome generically; keep the payload
	// on that path — a spawned tab whose rollback could not prove it absent
	// (#3237) still has a minted identity the CLI must report so the survivor
	// can be targeted. Only a clean failure has nothing to return.
	if err != nil && !isMutationCommitted(err) {
		return CreateTabResponse{}, err
	}
	return resp, err
}

// CloseTab asks the daemon to close a non-agent tab on an existing session and
// returns the resolved name of the tab that was closed. It is the close-side
// counterpart of CreateTab.
func CloseTab(req CloseTabRequest) (string, error) {
	var resp CloseTabResponse
	if err := callDaemon("CloseTab", req, &resp); err != nil {
		return "", err
	}
	return resp.Name, nil
}

// RenameTab asks the daemon to relabel one tab of an existing session. It
// returns the RESOLVED name — sanitized and collision-suffixed — which is what
// the tab is actually called afterwards, so callers print that rather than the
// name that was requested.
func RenameTab(req RenameTabRequest) (string, error) {
	var resp RenameTabResponse
	if err := callDaemon("RenameTab", req, &resp); err != nil {
		return "", err
	}
	return resp.Name, nil
}

// ReorderTab asks the daemon to move one tab within a session's roster,
// returning the moved tab's name and its final index.
func ReorderTab(req ReorderTabRequest) (string, int, error) {
	var resp ReorderTabResponse
	if err := callDaemon("ReorderTab", req, &resp); err != nil {
		return "", 0, err
	}
	return resp.Name, resp.Index, nil
}

// The TUI's control + read path moved onto the HTTP apiclient in #1592 Phase 2
// PR3, so the net/rpc client wrappers only the TUI called — PauseStatusPoll, ResumeStatusPoll
// (here) and ResumeFromLimit /
// SnapshotWithAlarms (in limit.go / snapshot.go) — are gone.
// The controlServer handlers stay: the gob control socket still SERVES every
// verb for CLI/internal callers; only the TUI-only Go client wrappers were
// removed. The sessions read (SnapshotNoSpawn) moved to apiclient in Phase 2
// PR2; ListTasksNoSpawn below remains the CLI's non-spawning, instances-only
// read on net/rpc.

// ErrDaemonUnavailable signals that a non-spawning daemon read (e.g.
// ListTasksNoSpawn) found no reachable, ready daemon: the control socket is
// absent/refused or the daemon is still restoring instances (#829). It is the
// CLI read path's cue to fall back to reading off disk — never to spawn a
// daemon or to surface a transient RPC error from a read-only command
// (#1029 PR 2).
var ErrDaemonUnavailable = errors.New("daemon not available")

// PreviewSessionSnapshot captures one session tab through the daemon's sole
// Preview handler and returns the WHOLE response, mirroring
// apiclient.PreviewSnapshot on the remote side. Unlike ListTasksNoSpawn,
// previewing a live terminal is an active read: it ensures the daemon is
// running and waits through daemon warm-up, just like the other session control
// calls.
//
// It returns everything because both transports already decode every field and
// only the old narrowing wrappers threw them away — so a caller that needs to
// know a capture was partial (#3169) had no way to ask, on either path. That
// narrowing wrapper (PreviewSession) kept no callers once api/sessions.go moved
// here, and #3734 removed it.
func PreviewSessionSnapshot(req PreviewRequest) (PreviewResponse, error) {
	var resp PreviewResponse
	if err := callDaemon("Preview", req, &resp); err != nil {
		return PreviewResponse{}, err
	}
	return resp, nil
}

// KillSession asks the daemon to kill a session and remove it from storage.
//
// callDaemon classifies the committed outcome generically. On the committed
// path the kill's durable tombstone landed but a post-commit
// teardown/storage/delete follow-up failed, and the tombstoned row is retained
// for the asynchronous reap — that is NOT a clean failure a caller may freely
// retry. Preserve the committed error (do not swallow it as nil) so the CLI's
// apiclient.IsMutationCommitted branch reports it as success-with-warning,
// the way ArchiveSession preserves its committed error for the same outcome
// class (#3252).
func KillSession(req KillSessionRequest) error {
	var resp KillSessionResponse
	err := callDaemon("KillSession", req, &resp)
	if err != nil && !isMutationCommitted(err) {
		return err // clean failure → caller hard-fails
	}
	return err // success (nil) OR committed path → caller classifies via IsMutationCommitted
}

// ArchiveSession asks the daemon to archive a session (#1028) and returns the
// relocated worktree's new path.
func ArchiveSession(req ArchiveSessionRequest) (string, error) {
	var resp ArchiveSessionResponse
	err := callDaemon("ArchiveSession", req, &resp)
	// callDaemon classifies the committed outcome generically. Keep the payload
	// on that path: the archive IS durable, and the CLI still has to report
	// where it landed. Only a clean failure has no path to report.
	if err != nil && !isMutationCommitted(err) {
		return "", err
	}
	return resp.ArchivedPath, err
}

// RestoreSession asks the daemon to restore an archived, Lost, or Dead session.
func RestoreSession(req RestoreSessionRequest) (string, error) {
	var resp RestoreSessionResponse
	err := callDaemon("RestoreSession", req, &resp)
	if err != nil && !isMutationCommitted(err) {
		return "", err
	}
	return resp.WorktreePath, err
}

// DeleteProject asks the daemon to delete a project (#1735): archive its live
// sessions (restorable), tear down any in-place ones, and drop its root_agents
// opt-in. Returns how many sessions were archived and how many were torn down.
func DeleteProject(req DeleteProjectRequest) (DeleteProjectResponse, error) {
	var resp DeleteProjectResponse
	err := callDaemon("DeleteProject", req, &resp)
	// Keep the payload on the committed path, as ArchiveSession does: the
	// deletion IS durable, and api/projects.go prints the archived/killed counts
	// and deregistration state from exactly this response.
	if err != nil && !isMutationCommitted(err) {
		return DeleteProjectResponse{}, err
	}
	return resp, err
}

// RegisterProject asks the daemon to register a git checkout as a durable,
// sessionless project (#2456). The daemon resolves req.Path on its OWN
// filesystem (expand ~, git root, validate) and persists to the #2355 registry;
// registering a known checkout is an idempotent success. Returns the resolved
// durable identity.
func RegisterProject(req RegisterProjectRequest) (config.Project, error) {
	var resp RegisterProjectResponse
	if err := callDaemon("RegisterProject", req, &resp); err != nil {
		return config.Project{}, err
	}
	return resp.Project, nil
}

// SendPromptWithStatus asks the daemon to send a prompt and returns the
// delivery observation made by the runtime's existing bounded submit path.
func SendPromptWithStatus(req SendPromptRequest) (session.PromptDeliveryStatus, error) {
	var resp SendPromptResponse
	if err := callDaemon("SendPrompt", req, &resp); err != nil {
		return session.PromptCouldNotConfirm, err
	}
	if !resp.Status.Valid() {
		// A pre-upgrade daemon has no Status field. Its nil RPC result proves
		// only that submission returned, so never manufacture "delivered".
		return session.PromptCouldNotConfirm, nil
	}
	return resp.Status, nil
}

// DeliverPrompt asks the daemon to deliver a prompt to a target session,
// auto-creating it when missing. It returns the recorded status ("started",
// "sent", or a task-only park). Unlike a bare CreateSession-then-SendPrompt, the
// whole create-or-send decision runs under the daemon's per-target lock, so
// concurrent deliveries to the same shared target never drop a prompt (#865).
func DeliverPrompt(req DeliverPromptRequest) (string, error) {
	status, _, err := DeliverPromptWithStatus(req)
	return status, err
}

// DeliverPromptWithStatus returns both the task lifecycle status and the
// delivery observation. Older daemons omit DeliveryStatus, which is honest
// could-not-confirm rather than an inferred success.
func DeliverPromptWithStatus(req DeliverPromptRequest) (string, session.PromptDeliveryStatus, error) {
	result, err := deliverPromptForTaskRPC(req)
	return result.status, result.deliveryStatus, err
}

// ListTasksNoSpawn returns the daemon's authoritative task list WITHOUT
// starting a daemon (#1029 PR 3). It dials the existing control socket only if
// it is already serving and returns ErrDaemonUnavailable otherwise, so a
// read-only CLI command (tasks list/get) falls back to reading tasks.json off
// disk rather than ever launching a daemon. Task reads do not depend on the
// instance restore, so there is no warm-up starting-error window to wait out
// here.
func ListTasksNoSpawn() ([]task.Task, error) {
	var resp ListTasksResponse
	if err := callDaemonNoEnsure("ListTasks", ListTasksRequest{}, &resp); err != nil {
		return nil, ErrDaemonUnavailable
	}
	return resp.Tasks, nil
}

// AddTask asks the daemon to append a task and re-arm its schedule set. Like
// every callDaemon path it ensures the daemon is running first, so adding a task
// also brings the scheduler up — a task is not schedulable without a running
// daemon.
// actor names the calling surface for the task's audit trail (#3623); pass
// task.ActorUnknown from a caller that has no surface to name.
func AddTask(t task.Task, actor task.Actor) error {
	var resp AddTaskResponse
	return callDaemon("AddTask", AddTaskRequest{Task: t, Actor: string(actor)}, &resp)
}

// UpdateTask asks the daemon to apply a field-level patch to the task with the
// given id and re-arm its schedule, returning the merged record (#1700). Only
// the patch's non-nil fields are written, so a single-field edit never clobbers
// a concurrent edit another client made to a different field.
// actor names the calling surface for the task's audit trail (#3623).
func UpdateTask(id string, update task.TaskUpdate, expect task.ProjectExpectation, actor task.Actor) (task.Task, error) {
	var resp UpdateTaskResponse
	if err := callDaemon("UpdateTask", UpdateTaskRequest{ID: id, Update: update, Expect: expect, Actor: string(actor)}, &resp); err != nil {
		return task.Task{}, err
	}
	return resp.Task, nil
}

// RemoveTask asks the daemon to delete a task and re-arm its schedule.
func RemoveTask(id string, expect task.ProjectExpectation) error {
	var resp RemoveTaskResponse
	return callDaemon("RemoveTask", RemoveTaskRequest{ID: id, Expect: expect}, &resp)
}

// RestartTask asks the daemon to stop and replace one enabled watch command,
// waiting until the old process tree is gone and the replacement has started.
func RestartTask(id string, expect task.ProjectExpectation) error {
	var resp RestartTaskResponse
	return callDaemon("RestartTask", RestartTaskRequest{ID: id, Expect: expect}, &resp)
}

// TriggerTask asks the daemon to fire a task now through the shared RunTask
// firing path (the same entrypoint the in-daemon scheduler uses). Replaces the
// old in-process daemon.RunTask CLI call so CLI, TUI, and scheduler triggers all
// converge on one daemon-owned firing path (#1169-class fix).
func TriggerTask(id string, expect task.ProjectExpectation) error {
	var resp TriggerTaskResponse
	return callDaemon("TriggerTask", TriggerTaskRequest{ID: id, Expect: expect}, &resp)
}

// ListAccounts reads the registered accounts and their logged-in state from the
// daemon that owns them.
func ListAccounts(req ListAccountsRequest) (ListAccountsResponse, error) {
	var resp ListAccountsResponse
	if err := callDaemon("ListAccounts", req, &resp); err != nil {
		return ListAccountsResponse{}, err
	}
	return resp, nil
}

// RegisterAccount creates an account's credential directory through the daemon,
// without logging in.
func RegisterAccount(req RegisterAccountRequest) (RegisterAccountResponse, error) {
	var resp RegisterAccountResponse
	if err := callDaemon("RegisterAccount", req, &resp); err != nil {
		return RegisterAccountResponse{}, err
	}
	return resp, nil
}

// AccountLogin asks the daemon to open an agent's own login flow in a bare tmux
// session scoped to one account, registering the account if it does not exist.
//
// callDaemon carries the warm-up retry and EnsureDaemon, so running the verb on
// a box whose daemon is not up yet starts it and waits rather than failing —
// which matters here because a login is often the very first thing a new install
// does.
func AccountLogin(req AccountLoginRequest) (AccountLoginResponse, error) {
	var resp AccountLoginResponse
	if err := callDaemon("AccountLogin", req, &resp); err != nil {
		return AccountLoginResponse{}, err
	}
	return resp, nil
}

// SpawnConfigAgent asks the daemon to start a config agent in a bare tmux session
// and returns the session name AND the absolute socket path to attach to.
// callDaemon carries the warm-up retry, so pressing the hotkey while the daemon
// is still starting waits rather than failing. The socket path may be empty (the
// daemon could not resolve it); the attach then falls back to the default socket.
func SpawnConfigAgent(req SpawnConfigAgentRequest) (string, string, error) {
	var resp SpawnConfigAgentResponse
	if err := callDaemon("SpawnConfigAgent", req, &resp); err != nil {
		return "", "", err
	}
	return resp.SessionName, resp.SocketPath, nil
}

// ReapConfigAgent tears down a config-agent session once the caller is done with
// it. The daemon's own shutdown reap is the backstop if this never arrives.
func ReapConfigAgent(sessionName string) error {
	var resp ReapConfigAgentResponse
	return callDaemon("ReapConfigAgent", ReapConfigAgentRequest{SessionName: sessionName}, &resp)
}
