package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pushcred"
	"github.com/sachiniyer/agent-factory/log"
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
)

// errGitCredentialOperator refuses the operator's own authority. Not a sandbox
// refusal: the operator is not a sandbox, and its error is not scrubbed.
var errGitCredentialOperator = errors.New("GitCredential answers only a sandbox session's own callback credential")

// pushCredentialState is the Manager's push-credential state.
type pushCredentialState struct {
	// source reads the daemon box's token.
	source pushcred.Source
	// tokenFor overrides source.Token in tests; nil in production.
	tokenFor func(ctx context.Context, o pushcred.Origin) (string, error)
	// fencePath overrides where the fences persist (tests); "" means
	// <AF home>/push-credential-fences.json.
	fencePath string

	mu     sync.Mutex
	loaded bool
	// loadErr is set when the fence record exists but cannot be read. Every
	// request is then refused (fail closed) until it can: an unreadable record
	// may be hiding a fence.
	loadErr error
	// autoPushing counts, per session id, the automatic preserve-pushes this
	// process is running right now (preserveSandboxBeforeReap).
	autoPushing map[string]int
	// fences maps a session id to the callback credential it held when an
	// automatic preserve-push STARTED, until that push is confirmed to have
	// succeeded. The in-sandbox archive takes no context and runs its git steps
	// with no deadline, so after a timeout, a lost reply or a daemon restart it
	// may still reach its push; no elapsed time proves it will not. A fence
	// therefore lasts as long as that credential does: until a replacement
	// runtime is minted a new one, or the session ends and revokes it.
	//
	// PERSISTED, and written before the push starts: a callback credential
	// survives a daemon restart (W3.6 re-derives the very same one), so a fence
	// held only in memory would lift on a restart while the archive ran on. It
	// keeps a digest of the credential, never the credential.
	fences map[string]pushFence
}

// pushFence is one persisted fence.
type pushFence struct {
	Grant string    `json:"grant"`
	At    time.Time `json:"at"`
}

// pushFenceRetention bounds how long a fence is kept: far beyond any archive,
// and only so a record that names sessions long gone does not grow forever.
const pushFenceRetention = 30 * 24 * time.Hour

const pushFenceFileName = "push-credential-fences.json"

func grantDigest(grant string) string {
	sum := sha256.Sum256([]byte(grant))
	return hex.EncodeToString(sum[:])
}

func (s *pushCredentialState) path() (string, error) {
	if s.fencePath != "" {
		return s.fencePath, nil
	}
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, pushFenceFileName), nil
}

// loadLocked reads the persisted fences once. Caller holds s.mu.
func (s *pushCredentialState) loadLocked() {
	if s.loaded {
		return
	}
	s.loaded = true
	s.fences = map[string]pushFence{}
	path, err := s.path()
	if err != nil {
		s.loadErr = err
		return
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return
	}
	if err == nil {
		err = json.Unmarshal(data, &s.fences)
	}
	if err != nil {
		s.loadErr = fmt.Errorf("reading %s: %w", path, err)
		log.ErrorLog.Printf("push credentials are withheld from every sandbox session until %s can be read (it records which sessions an automatic preserve-push fences): %v", path, err)
		return
	}
	if s.fences == nil { // a record holding JSON null
		s.fences = map[string]pushFence{}
	}
	for id, f := range s.fences {
		if time.Since(f.At) > pushFenceRetention {
			delete(s.fences, id)
		}
	}
}

// persistLocked writes the fences, 0600, atomically. Caller holds s.mu.
func (s *pushCredentialState) persistLocked() error {
	path, err := s.path()
	if err != nil {
		return err
	}
	data, err := json.Marshal(s.fences)
	if err != nil {
		return err
	}
	return config.AtomicWriteFileRefusingLink(path, data, 0o600)
}

// beginAutoPush fences sessionID for an automatic preserve-push, PERSISTED
// before the push may start. The returned end lifts the fence only when called
// with confirmed=true; any other ending leaves it, tied to grant. An error means
// the fence could not be recorded, and the caller must not start the push.
func (s *pushCredentialState) beginAutoPush(sessionID, grant string) (end func(confirmed bool), err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	if s.autoPushing == nil {
		s.autoPushing = map[string]int{}
	}
	if grant != "" {
		prev, had := s.fences[sessionID]
		s.fences[sessionID] = pushFence{Grant: grantDigest(grant), At: time.Now()}
		if err := s.persistLocked(); err != nil {
			if had {
				s.fences[sessionID] = prev
			} else {
				delete(s.fences, sessionID)
			}
			return nil, err
		}
	}
	s.autoPushing[sessionID]++
	var once sync.Once
	return func(confirmed bool) {
		once.Do(func() { s.endAutoPush(sessionID, grant, confirmed) })
	}, nil
}

func (s *pushCredentialState) endAutoPush(sessionID, grant string, confirmed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.autoPushing[sessionID]--; s.autoPushing[sessionID] <= 0 {
		delete(s.autoPushing, sessionID)
	}
	if !confirmed || s.autoPushing[sessionID] > 0 || grant == "" {
		return
	}
	if f, ok := s.fences[sessionID]; ok && f.Grant == grantDigest(grant) {
		delete(s.fences, sessionID)
		if err := s.persistLocked(); err != nil {
			// The fence stays on disk, which only withholds credentials longer:
			// the safe side to fail on.
			log.WarningLog.Printf("session %s: could not lift its push-credential fence after a confirmed automatic push: %v", sessionID, err)
		}
	}
}

// autoPushFenced reports whether a request from sessionID, admitted with grant,
// is inside an automatic preserve-push's fence. An unreadable record fences
// everything.
func (s *pushCredentialState) autoPushFenced(sessionID, grant string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	if s.loadErr != nil || s.autoPushing[sessionID] > 0 {
		return true
	}
	f, ok := s.fences[sessionID]
	return ok && grant != "" && f.Grant == grantDigest(grant)
}

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

// sandboxEndpointKey marks a request that arrived on the dedicated sandbox
// callback endpoint (W3.4). Unexported zero-size type: nothing outside this
// package can forge it into a context.
type sandboxEndpointKey struct{}

// markSandboxEndpoint wraps the dedicated endpoint's handler so every request it
// serves carries the marker. Nothing else sets it.
func markSandboxEndpoint(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sandboxEndpointKey{}, true)))
	})
}

func viaSandboxEndpoint(ctx context.Context) bool {
	v, _ := ctx.Value(sandboxEndpointKey{}).(bool)
	return v
}

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
	token, err := s.manager.pushCredentialFor(ctx, owner, admittedSandboxCredential(ctx), req)
	if err != nil {
		return err
	}
	resp.Username = pushCredentialUsername
	resp.Password = token
	return nil
}

// pushCredentialFor authorizes owner's request, reads the token, and authorizes
// again: the read can take seconds (a gh run), and a session archived, killed,
// replaced or fenced meanwhile must not be answered with a token its own end was
// meant to stop. grant is the credential the request was admitted with.
func (m *Manager) pushCredentialFor(ctx context.Context, owner, grant string, req GitCredentialRequest) (string, error) {
	origin, err := m.authorizePushCredential(owner, grant, req)
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
	again, err := m.authorizePushCredential(owner, grant, req)
	if err != nil {
		return "", err
	}
	if again != origin {
		// The daemon checkout's origin moved between the two checks: the token
		// read was for a remote this session no longer has.
		return "", sandboxRefusal(refusePushOtherRepo)
	}
	m.info().Printf("push credential: answered session %s for https://%s/%s", owner, origin.Host, origin.Path)
	return token, nil
}

// authorizePushCredential applies every rule but the token read and returns the
// origin the request may be answered for. Its one blocking step, reading the
// daemon checkout's origin (a git run), sits BETWEEN two passes of the state
// checks, so the last thing it does before answering is a check that blocks on
// nothing: no revocation, archive, licence change or fence can land in a gap
// between its check and the reply.
func (m *Manager) authorizePushCredential(owner, grant string, req GitCredentialRequest) (pushcred.Origin, error) {
	inst, err := m.pushCredentialState(owner, grant, nil)
	if err != nil {
		return pushcred.Origin{}, err
	}
	origin, originOK := instancePushOrigin(inst)
	if _, err := m.pushCredentialState(owner, grant, inst); err != nil {
		return pushcred.Origin{}, err
	}
	if !originOK {
		return pushcred.Origin{}, sandboxRefusal(refusePushNoOrigin)
	}
	if !origin.Matches(req.Protocol, req.Host, req.Path) {
		return pushcred.Origin{}, sandboxRefusal(refusePushOtherRepo)
	}
	return origin, nil
}

// pushCredentialState is the non-blocking half of the authorization: the row is
// the caller's own, off-box and live; the credential the request was admitted
// with is still that row's; no automatic preserve-push fences it; and the
// operator's live config licenses its placement. want, when set, is the row an
// earlier pass found: a different object under the same id is a replacement, and
// is refused rather than trusted.
func (m *Manager) pushCredentialState(owner, grant string, want *session.Instance) (*session.Instance, error) {
	m.mu.Lock()
	inst, _ := m.trackedSessionByIDLocked(owner)
	m.mu.Unlock()
	if inst == nil || (want != nil && inst != want) {
		return nil, sandboxRefusal(refusePushNotLive)
	}
	backend := instanceBackendType(inst)
	if backend != "ssh" && backend != "sandbox" {
		return nil, sandboxRefusal(refusePushNotLicensed)
	}
	if inst.IsResting() || inst.GetStatus() == session.Deleting || !m.sandboxTokens.stillGrants(owner, grant) {
		return nil, sandboxRefusal(refusePushNotLive)
	}
	if m.pushCreds.autoPushFenced(inst.ID, grant) {
		return nil, sandboxRefusal(refusePushAutoPreserve)
	}
	if !m.pushCredentialLicensed(inst, backend) {
		return nil, sandboxRefusal(refusePushNotLicensed)
	}
	return inst, nil
}

// pushCredentialLicensed reports whether the operator's live global config
// licenses push credentials for where inst runs. The recorded placement must
// agree with the row's backend: a listed host is an ssh placement, the free-form
// target a sandbox one.
func (m *Manager) pushCredentialLicensed(inst *session.Instance, backend string) bool {
	placement, ok := instancePushPlacement(inst)
	if !ok {
		return false
	}
	cfg := m.Config()
	if cfg == nil {
		return false
	}
	switch {
	case placement.ListedHost != "" && backend == "ssh":
		host, listed := cfg.Hosts[placement.ListedHost]
		return listed && host.PushCredentials
	case placement.FreeFormSandbox && backend == "sandbox":
		return cfg.SandboxPushCredentials
	}
	return false
}
