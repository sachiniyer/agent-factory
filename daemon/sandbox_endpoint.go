package daemon

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sockpath"
	"github.com/sachiniyer/agent-factory/log"
)

// The dedicated sandbox callback endpoint (#5571 W3.4, PR A).
//
// A sandbox's in-sandbox `af` reaches the daemon over a forward carried by the
// supervised ssh connection. That forward must NOT terminate on the client
// listener: ssh delivers it from a loopback peer, and a loopback-bound client
// listener (the default) exempts loopback peers from the token check
// (httpauth.go authRequired), which would hand the sandbox operator authority.
//
// It terminates here instead: one host-local unix socket, serving the same route
// mux behind a gate that admits ONLY per-session sandbox credentials. Neither
// the operator's token nor the loopback exemption exists on this listener — the
// gate's operator credential is empty, which ConstantTimeEqual refuses for every
// presented value — so the scope table (sandboxAllowed) and the owner
// constraints (sandbox_owner.go) are the only authority a peer can obtain.
// Because it does not read network.listen_addr or require_token, a default-
// config daemon can offer it.
//
// The socket lives under AF_HOME with mode 0600, so other host users cannot
// reach it either; the credential, not the path, identifies the session.

const sandboxEndpointSocketFileName = "sandbox-callback.sock"

// SandboxEndpointSocketPath is where the daemon's sandbox callback endpoint
// listens. Length-checked like the daemon's other sockets (#1940).
func SandboxEndpointSocketPath() (string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, sandboxEndpointSocketFileName)
	if err := sockpath.Check("sandbox callback socket", path); err != nil {
		return "", err
	}
	return path, nil
}

// sandboxEndpoint is the running endpoint.
type sandboxEndpoint struct {
	srv  *http.Server
	ln   net.Listener
	path string
	once sync.Once
}

// sandboxOnlyGate is the endpoint's auth gate: auth is always required, no
// operator credential exists, and loopback is not exempt.
func sandboxOnlyGate(reg *sandboxTokenRegistry) *authGate {
	return &authGate{
		expectedToken:  func() (string, error) { return "", nil },
		tokenDisabled:  false,
		loopbackExempt: false,
		sandboxTokens:  reg,
	}
}

// startSandboxEndpoint binds the endpoint at path and serves mux behind the
// sandbox-only gate. A stale socket file from a previous daemon is replaced.
func startSandboxEndpoint(mux http.Handler, reg *sandboxTokenRegistry, path string) (*sandboxEndpoint, error) {
	if reg == nil {
		return nil, fmt.Errorf("sandbox callback endpoint needs a credential registry")
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("bind sandbox callback socket %q: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	e := &sandboxEndpoint{
		srv: &http.Server{
			// The marker is how a route that must never answer on the main
			// listener (GitCredential, #5571 W7.8) tells the two apart: the
			// main listener admits sandbox credentials to the scope too.
			Handler:           markSandboxEndpoint(withAuth(mux, sandboxOnlyGate(reg), nil)),
			ReadHeaderTimeout: httpReadHeaderTimeout,
		},
		ln:   ln,
		path: path,
	}
	go func() {
		if err := e.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.WarningLog.Printf("daemon sandbox callback endpoint stopped: %v", err)
		}
	}()
	return e, nil
}

// close stops the endpoint and unlinks its socket. Safe to call repeatedly.
func (e *sandboxEndpoint) close() error {
	if e == nil {
		return nil
	}
	var err error
	e.once.Do(func() {
		err = e.srv.Close()
		_ = os.Remove(e.path)
	})
	return err
}
