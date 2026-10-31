package daemon

import (
	"context"
	"errors"

	"github.com/sachiniyer/agent-factory/internal/pushcred"
	"github.com/sachiniyer/agent-factory/session"
)

// Push credentials on listed hosts (#5571 W7.8): the daemon half.
//
// A session on a listed host that the operator opted in can `git push` its origin
// and run `gh` as the operator. The GitHub token is NEVER stored on the host:
// the session's git credential helper (and its gh shim) asks this route each time
// it needs one, over the session's own scoped af callback (W3.4's -R endpoint,
// W3.5's scope, re-derived across restarts by W3.6). So there is no copy to
// refresh, write back, fan out or chase at teardown: every way a session ends
// revokes its callback, and this route answers only a live row.
//
// What it answers, and for whom, each rule enforced here and pinned by a test:
//
//   - ONLY the session's own callback credential, and only over the dedicated
//     sandbox endpoint. The main web listener also admits sandbox credentials to
//     the scope, so a leaked af-callback.env would otherwise fetch the operator's
//     GitHub token over the network; and the operator's own token gets nothing, so
//     this adds no "give me the daemon's GitHub token" call to the operator API.
//   - ONLY a live, off-box row whose recorded placement the operator licensed: a
//     listed host with push_credentials = true, or the free-form sandbox.ssh
//     target with sandbox.push_credentials = true. Read live, so turning either
//     off stops the next answer. A repository's own [ssh] target never qualifies.
//   - ONLY the session's own origin: the request's host and owner/repo must equal
//     the DAEMON checkout's https GitHub origin. Nothing the host reports is
//     trusted; it is only compared.
//   - NEVER while the daemon runs an automatic preserve-push for the row, so
//     W7.8 changes no af-initiated push (the owner's answer to the plan's Q4). An
//     explicit `archive --push` and the agent's own pushes are answered.
//
// The token itself comes from the daemon box (internal/pushcred): its own
// GH_TOKEN-family variables, else `gh auth token`, with gh located on absolute
// PATH entries only. It is held in memory for at most a minute and never logged.

// GitCredentialRequest is the git credential-protocol request the in-sandbox
// helper relays: the three fields git sends with credential.useHttpPath.
type GitCredentialRequest struct {
	Protocol string `json:"protocol"`
	Host     string `json:"host"`
	Path     string `json:"path"`
}

// GitCredentialResponse is the credential git receives: GitHub's fixed token
// username and the daemon box's token for the session's origin.
type GitCredentialResponse struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

const pushCredentialUsername = "x-access-token"

// The refusals a sandbox is shown. Constant texts (sandboxRefusal's rule): none
// reflects what the caller sent, and each names what the operator can change.
const (
	refusePushOverMainListener = "push credentials are answered only over the session's own af callback socket"
	refusePushNotLive          = "push credentials are answered only for a live off-box session"
	refusePushNotLicensed      = "push credentials are not enabled for this session's host: the operator sets push_credentials = true on the listed host (or sandbox.push_credentials = true for the sandbox.ssh target)"
	refusePushNoOrigin         = "push credentials cover an https GitHub origin, and this session's origin is not one"
	refusePushOtherRepo        = "push credentials cover only this session's own origin repository"
	refusePushAutoPreserve     = "push credentials are withheld while af runs an automatic preserve-push for this session"
	refusePushNoLogin          = "the daemon box has no GitHub login for this origin's host: run gh auth login there, or set GH_TOKEN for the daemon"
	refusePushSandboxCreated   = "this session was started by another session's af, and such sessions never receive push credentials"
	refusePushWrongForward     = "push credentials are answered only through the session's own ssh forward"
)

// errGitCredentialOperator refuses the operator's own authority. Not a sandbox
// refusal: the operator is not a sandbox, and its error is not scrubbed.
var errGitCredentialOperator = errors.New("GitCredential answers only a sandbox session's own callback credential")

func (s *pushCredentialState) token(ctx context.Context, o pushcred.Origin) (string, error) {
	if s.tokenFor != nil {
		return s.tokenFor(ctx, o)
	}
	return s.source.Token(ctx, o)
}

// instancePushPlacement and instancePushOrigin read the two session facts the
// licence needs. Package vars, like instanceBackendType, so a test can place a
// session without provisioning one.
var (
	instancePushPlacement = func(i *session.Instance) (session.PushPlacement, bool) { return i.PushCredentialPlacement() }
	instancePushOrigin    = func(i *session.Instance) (pushcred.Origin, bool) { return i.PushCredentialOrigin() }
)

// gitCredential is the /v1/GitCredential handler.
func (s *controlServer) gitCredential(ctx context.Context, req GitCredentialRequest, resp *GitCredentialResponse) error {
	owner, isSandbox := sandboxOwner(ctx)
	if !isSandbox {
		return errGitCredentialOperator
	}
	if !viaSandboxEndpoint(ctx) {
		return sandboxRefusal(refusePushOverMainListener)
	}
	if err := s.requireManagerReady(); err != nil {
		return err
	}
	ask := pushAsk{owner: owner, grant: admittedSandboxCredential(ctx), carrier: carrierPGID(ctx), req: req}
	token, err := s.manager.pushCredentialFor(ctx, ask)
	if err != nil {
		return err
	}
	resp.Username = pushCredentialUsername
	resp.Password = token
	return nil
}

// pushAsk is one request, as the gate and the endpoint admitted it: the owning
// session, the very credential it presented, and the process group of the
// connection's peer (the forward it arrived through).
type pushAsk struct {
	owner, grant string
	carrier      int
	req          GitCredentialRequest
}

// pushCredentialFor authorizes the request, reads the token, and authorizes
// again: the read can take seconds (a gh run), and a session archived, killed,
// replaced or fenced meanwhile must not be answered with a token its own end was
// meant to stop. The audit line is written before a LAST liveness check, so not
// even a log write sits between the final check and the reply.
func (m *Manager) pushCredentialFor(ctx context.Context, ask pushAsk) (string, error) {
	origin, _, err := m.authorizePushCredential(ask)
	if err != nil {
		return "", err
	}
	token, err := m.pushCreds.token(ctx, origin)
	if errors.Is(err, pushcred.ErrNoLogin) {
		return "", sandboxRefusal(refusePushNoLogin)
	}
	if err != nil {
		return "", err
	}
	again, inst, err := m.authorizePushCredential(ask)
	if err != nil {
		return "", err
	}
	if again != origin {
		// The daemon checkout's origin moved between the two checks: the token
		// read was for a remote this session no longer has.
		return "", sandboxRefusal(refusePushOtherRepo)
	}
	m.info().Printf("push credential: answered session %s for https://%s/%s", ask.owner, origin.Host, origin.Path)
	if err := m.pushStillLive(ask, inst); err != nil {
		return "", err
	}
	return token, nil
}

// authorizePushCredential applies every rule but the token read, and returns the
// origin the request may be answered for and the row it authorized. Its one
// blocking step, reading the daemon checkout's origin (a git run), comes FIRST;
// every rule that can change runs after it, and the liveness of the row and of
// the very credential presented runs LAST, after every lock and read the others
// take.
func (m *Manager) authorizePushCredential(ask pushAsk) (pushcred.Origin, *session.Instance, error) {
	inst, err := m.pushCredentialRow(ask.owner, nil)
	if err != nil {
		return pushcred.Origin{}, nil, err
	}
	origin, originOK := instancePushOrigin(inst)
	if _, err := m.pushCredentialRow(ask.owner, inst); err != nil {
		return pushcred.Origin{}, nil, err
	}
	if inst.CreatedBySandbox() != "" {
		return pushcred.Origin{}, nil, sandboxRefusal(refusePushSandboxCreated)
	}
	backend := instanceBackendType(inst)
	if backend != "ssh" && backend != "sandbox" {
		return pushcred.Origin{}, nil, sandboxRefusal(refusePushNotLicensed)
	}
	placement, ok := instancePushPlacement(inst)
	if !ok || !placement.Licensed(m.Config(), backend) {
		return pushcred.Origin{}, nil, sandboxRefusal(refusePushNotLicensed)
	}
	if m.pushCreds.autoPushFenced(inst.ID, placement.CallbackRef) {
		return pushcred.Origin{}, nil, sandboxRefusal(refusePushAutoPreserve)
	}
	if !carriedByOwnForward(ask.carrier, placement.CallbackRef) {
		return pushcred.Origin{}, nil, sandboxRefusal(refusePushWrongForward)
	}
	if !originOK {
		return pushcred.Origin{}, nil, sandboxRefusal(refusePushNoOrigin)
	}
	if !origin.Matches(ask.req.Protocol, ask.req.Host, ask.req.Path) {
		return pushcred.Origin{}, nil, sandboxRefusal(refusePushOtherRepo)
	}
	if err := m.pushStillLive(ask, inst); err != nil {
		return pushcred.Origin{}, nil, err
	}
	return origin, inst, nil
}

// pushCredentialRow finds the owner's tracked row. want, when set, is the row an
// earlier step found: a different object under the same id is a replacement, and
// is refused rather than trusted.
func (m *Manager) pushCredentialRow(owner string, want *session.Instance) (*session.Instance, error) {
	m.mu.Lock()
	inst, _ := m.trackedSessionByIDLocked(owner)
	m.mu.Unlock()
	if inst == nil || (want != nil && inst != want) {
		return nil, sandboxRefusal(refusePushNotLive)
	}
	return inst, nil
}

// pushStillLive is the last check of every pass: the row is still the one
// authorized, still live, and the credential the request presented is still that
// row's. It reads in-memory state only.
func (m *Manager) pushStillLive(ask pushAsk, inst *session.Instance) error {
	if _, err := m.pushCredentialRow(ask.owner, inst); err != nil {
		return err
	}
	if inst.IsResting() || inst.GetStatus() == session.Deleting || !m.sandboxTokens.stillGrants(ask.owner, ask.grant) {
		return sandboxRefusal(refusePushNotLive)
	}
	return nil
}
