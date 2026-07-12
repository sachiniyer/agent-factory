package daemon

import (
	"crypto/tls"
	"fmt"
	"net/http"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// The optional TLS TCP listener for the daemon's HTTP/WS surface (#1592 Phase 3
// PR3, §1.1). It serves the SAME mux the unix socket serves, but wrapped in a
// token-enforcing gate: the unix socket is trusted transport (0600 perms are
// the auth, #1029) and passes a nil gate, while this listener requires a valid
// bearer token on every request and applies the CORS allow-list.
//
// It is OFF BY DEFAULT — bound only when config.ListenAddr is non-empty. When
// empty (the default) startHTTPServer never calls in here, so behavior is
// byte-identical to the pure-unix daemon that shipped before Phase 3.

// tlsMinVersion is the floor for the TCP listener. TLS 1.2 is the modern
// baseline (1.0/1.1 are deprecated); the bearer token must never ride a
// downgraded or plaintext connection.
const tlsMinVersion = tls.VersionTLS12

// tcpListenerInfo is the enable-banner payload startHTTPServer logs once when
// the TCP listener binds. The token is included deliberately (§1.3): the daemon
// log is the operator's channel to the freshly generated credential — a
// documented log-file-readability consideration, gated behind the explicit
// listen_addr opt-in.
type tcpListenerInfo struct {
	Addr        string // the resolved bound address (host:port, port filled in for :0)
	Token       string // the bearer token clients must present
	Fingerprint string // "sha256:<hex>" of the leaf cert, the value clients TOFU-pin
	SelfSigned  bool   // true for the generated self-signed default, false for a user cert
}

// startTCPListener binds the TLS TCP listener on cfg.ListenAddr and serves mux
// wrapped in a token-enforcing gate + the CORS allow-list. It returns a cleanup
// function that shuts the server down and the banner payload the caller logs.
//
// It resolves the TLS material (PR1: self-signed default, or the user's
// tls_cert/tls_key), ensures the bearer token exists before opening the port
// (so an operator enabling the listener always has a credential to present),
// and reads that token FRESH per auth event through the gate so `af token
// rotate` takes effect for new connections without a daemon restart.
func startTCPListener(mux http.Handler, cfg *config.Config) (func() error, tcpListenerInfo, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return nil, tcpListenerInfo{}, err
	}

	mat, err := ResolveTLSMaterial(dir, cfg.TLSCert, cfg.TLSKey)
	if err != nil {
		return nil, tcpListenerInfo{}, fmt.Errorf("resolve TLS material: %w", err)
	}
	cert, err := tls.LoadX509KeyPair(mat.CertPath, mat.KeyPath)
	if err != nil {
		return nil, tcpListenerInfo{}, fmt.Errorf("load TLS keypair: %w", err)
	}
	fingerprint, err := CertFingerprint(mat.CertPath)
	if err != nil {
		return nil, tcpListenerInfo{}, fmt.Errorf("compute cert fingerprint: %w", err)
	}

	// Generate-if-absent so enabling the listener always yields a usable token;
	// the gate below re-reads the file per auth event, so rotation stays live.
	tokenPath, err := TokenPath()
	if err != nil {
		return nil, tcpListenerInfo{}, err
	}
	token, err := EnsureToken(tokenPath)
	if err != nil {
		return nil, tcpListenerInfo{}, fmt.Errorf("ensure daemon token: %w", err)
	}
	gate := &authGate{expectedToken: func() (string, error) {
		return LoadToken(tokenPath)
	}}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tlsMinVersion,
	}
	// tls.Listen wraps each accepted conn in the TLS handshake, so srv.Serve
	// (not ServeTLS) is correct here — and Addr() reports the concrete port
	// even when cfg.ListenAddr requests :0 (used by the integration test).
	listener, err := tls.Listen("tcp", cfg.ListenAddr, tlsCfg)
	if err != nil {
		return nil, tcpListenerInfo{}, fmt.Errorf("bind TCP listener on %q: %w", cfg.ListenAddr, err)
	}

	// The TCP listener also serves the embedded browser SPA (#1592 Phase 5 PR2,
	// design §1). webShellHandler serves the static shell UNAUTHENTICATED on every
	// non-/v1 path (you cannot paste a token into a page that won't load) while
	// routing /v1/... through the token gate below exactly as before. This wrapper
	// is TCP-only: the unix socket keeps its bare mux (whose `/` still 404s), so
	// the web assets never appear on the socket path.
	srv := &http.Server{
		Handler:           webShellHandler(withAuth(mux, gate, cfg.CORSAllowedOrigins)),
		ReadHeaderTimeout: httpReadHeaderTimeout,
	}
	go func() {
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.WarningLog.Printf("daemon TCP listener stopped: %v", err)
		}
	}()

	info := tcpListenerInfo{
		Addr:        listener.Addr().String(),
		Token:       token,
		Fingerprint: fingerprint,
		SelfSigned:  mat.SelfSigned,
	}
	return srv.Close, info, nil
}
