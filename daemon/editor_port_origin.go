package daemon

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// Per-session editor origins for REMOTE viewers (W5.2, #5571).
//
// editor_origin.go partitions an editor's browser storage per session with a
// *.localhost label, which only works for a viewer on the daemon's own machine: a
// remote viewer's browser resolves *.localhost to ITS loopback. Behind a single
// front-door hostname the one thing left that separates browser origins is the
// PORT, so an operator declares a pool (network.editor_origin_ports) and each
// session's editor is served on its own port from that pool:
//
//	https://<front-door hostname>:<session's port>
//
// The allocation is persisted (editor-origin-ports.json beside editor-origin-secret)
// so a session keeps its port, and with it its IndexedDB, across daemon restarts.
// The daemon binds each allocated port on LOOPBACK ONLY; the operator exposes the
// same ports (tailscale serve --https=<port> 127.0.0.1:<port>), exactly as the
// main UI is exposed.
//
// THE CREDENTIAL. What sits behind a port is a code-server with auth disabled, i.e.
// a shell in the worktree, so network reachability is not a credential (settled
// decision 4 keeps the single operator token and puts tailnet identity out of
// scope). Each session has an in-memory KEY, accepted only as an HttpOnly cookie
// named for that session's port. A client obtains it only through
// POST /v1/editor-origin — behind the operator token — which returns a SINGLE-USE
// bootstrap URL on the editor's own origin: visiting it sets the cookie and
// redirects, so the key never lives on in a URL. Cookies are scoped by host and
// path, not port (RFC 6265 §8.5), so the cookie NAME carries the port and the
// handler compares against that port's session key alone: session A's key is
// refused on session B's origin. Keys die with the session's editor
// (stopVSCodeForInstance) and with the daemon; clients re-bootstrap.
const (
	editorPortStoreFile      = "editor-origin-ports.json"
	editorPortBootstrapPath  = "/_af/editor-bootstrap"
	editorPortProbePath      = "/_af/editor-probe"
	editorPortCookiePrefix   = "af_editor_" //nolint:gosec // cookie name prefix, not a credential
	editorPortBootstrapTTL   = 60 * time.Second
	editorPortMaxPendingBoot = 4
)

// errEditorPortsOff names the key that turns the feature on.
var errEditorPortsOff = errors.New("per-session editor origins are off: set network.editor_origin_ports to a pool of loopback ports (for example 9100-9119) and expose them behind your front door (for example `tailscale serve --https=9100 127.0.0.1:9100`)")

type editorBootstrap struct {
	sessionID string
	expires   time.Time
}

type editorPortOrigins struct {
	mu      sync.Mutex
	alloc   map[string]int // session id -> port; mirrors the store file
	epochs  map[int]string // port -> token minted when it was last assigned
	freed   map[int]int64  // port -> unix seconds it last lost its session
	loaded  bool
	servers map[int]*http.Server
	keys    map[string]string
	boots   map[string]editorBootstrap
	now     func() time.Time
	// listen is a seam so tests need not bind a pool port.
	listen func(port int) (net.Listener, error)
}

func newEditorPortOrigins() *editorPortOrigins {
	return &editorPortOrigins{
		alloc:   map[string]int{},
		epochs:  map[int]string{},
		freed:   map[int]int64{},
		servers: map[int]*http.Server{},
		keys:    map[string]string{},
		boots:   map[string]editorBootstrap{},
		now:     time.Now,
		listen: func(port int) (net.Listener, error) {
			return net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		},
	}
}

func editorPortStorePath() (string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, editorPortStoreFile), nil
}

type editorPortStoreDoc struct {
	Ports map[string]int `json:"ports"`
	// Epochs is a random token per port, replaced every time the port is handed to a
	// session. It is what lets a browser be told its origin-scoped storage belongs to
	// a previous owner (see bootstrapPage).
	Epochs map[string]string `json:"epochs,omitempty"`
	// Freed is when each port last lost its session (unix seconds), so reassignment
	// can prefer the port that has been idle longest.
	Freed map[string]int64 `json:"freed,omitempty"`
}

// loadLocked reads the persisted allocation once. A missing file is empty.
func (e *editorPortOrigins) loadLocked() error {
	if e.loaded {
		return nil
	}
	path, err := editorPortStorePath()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		var doc editorPortStoreDoc
		if jerr := json.Unmarshal(data, &doc); jerr != nil {
			log.WarningLog.Printf("editor origin port store %s is unreadable (%v); reallocating", path, jerr)
		} else {
			if doc.Ports != nil {
				e.alloc = doc.Ports
			}
			for k, v := range doc.Epochs {
				if p, aerr := strconv.Atoi(k); aerr == nil {
					e.epochs[p] = v
				}
			}
			for k, v := range doc.Freed {
				if p, aerr := strconv.Atoi(k); aerr == nil {
					e.freed[p] = v
				}
			}
		}
	}
	e.loaded = true
	return nil
}

func (e *editorPortOrigins) saveLocked() error {
	path, err := editorPortStorePath()
	if err != nil {
		return err
	}
	if err := config.MkdirAllUnderAFHome(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	doc := editorPortStoreDoc{Ports: e.alloc, Epochs: map[string]string{}, Freed: map[string]int64{}}
	for p, v := range e.epochs {
		doc.Epochs[strconv.Itoa(p)] = v
	}
	for p, v := range e.freed {
		doc.Freed[strconv.Itoa(p)] = v
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return config.WithFileLock(path, func() error {
		return config.AtomicWriteFileRefusingLink(path, append(data, '\n'), 0o600)
	})
}

// portFor allocates (or returns the persisted) pool port for sessionID and makes
// sure its loopback listener is up. reclaimable reports sessions whose port may be
// taken back when the pool is full (gone or archived).
func (m *Manager) editorPortFor(cs *controlServer, sessionID string) (int, error) {
	pool, err := config.ParseEditorOriginPorts(m.Config().EditorOriginPorts)
	if err != nil {
		return 0, err
	}
	if len(pool) == 0 {
		return 0, errEditorPortsOff
	}
	e := m.editorPorts
	if e == nil {
		return 0, errEditorPortsOff
	}
	// Resolved before taking e.mu: it takes m.mu, and nothing here may hold e.mu
	// across a manager lock.
	reclaimable := map[string]bool{}
	for id := range e.snapshotAlloc() {
		if inst := m.trackedStreamSession(id); inst == nil || inst.GetLiveness() == session.LiveArchived {
			reclaimable[id] = true
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.loadLocked(); err != nil {
		return 0, fmt.Errorf("load editor origin ports: %w", err)
	}
	inPool := map[int]bool{}
	for _, p := range pool {
		inPool[p] = true
	}
	port, ok := e.alloc[sessionID]
	if ok && !inPool[port] {
		ok = false // the pool moved; the old port is no longer ours to serve
		delete(e.alloc, sessionID)
	}
	if !ok {
		used := map[int]bool{}
		for _, p := range e.alloc {
			used[p] = true
		}
		free := func() []int {
			var out []int
			for _, p := range pool {
				if !used[p] {
					out = append(out, p)
				}
			}
			return out
		}
		candidates := free()
		if len(candidates) == 0 {
			// Full: take back ONE port whose session is gone or archived.
			var victim string
			for id, p := range e.alloc {
				if reclaimable[id] && id != sessionID && (victim == "" || p < e.alloc[victim]) {
					victim = id
				}
			}
			if victim != "" {
				p := e.alloc[victim]
				delete(e.alloc, victim)
				e.freed[p] = e.now().Unix()
				e.dropServerLocked(p)
				candidates = []int{p}
			}
		}
		if len(candidates) == 0 {
			return 0, fmt.Errorf("every port in network.editor_origin_ports (%d) is allocated to a live session; widen the pool (for example 9100-9139)", len(pool))
		}
		// Least recently used first: a never-used port (no freed time) before one
		// freed long ago before one freed just now, so the longest-idle origin is
		// reused and the window in which a browser may still hold its old owner's
		// storage is as short as the pool allows. Pool order breaks ties.
		port = candidates[0]
		for _, p := range candidates[1:] {
			if e.freed[p] < e.freed[port] {
				port = p
			}
		}
		epoch, err := generateToken()
		if err != nil {
			return 0, err
		}
		e.alloc[sessionID] = port
		e.epochs[port] = epoch
		delete(e.freed, port)
		if err := e.saveLocked(); err != nil {
			delete(e.alloc, sessionID)
			return 0, fmt.Errorf("persist editor origin port: %w", err)
		}
	}
	if err := e.serveLocked(cs, port); err != nil {
		return 0, err
	}
	return port, nil
}

func (e *editorPortOrigins) snapshotAlloc() map[string]int {
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.loadLocked()
	out := make(map[string]int, len(e.alloc))
	for k, v := range e.alloc {
		out[k] = v
	}
	return out
}

func (e *editorPortOrigins) dropServerLocked(port int) {
	if srv := e.servers[port]; srv != nil {
		_ = srv.Close()
		delete(e.servers, port)
	}
}

func (e *editorPortOrigins) serveLocked(cs *controlServer, port int) error {
	if e.servers[port] != nil {
		return nil
	}
	ln, err := e.listen(port)
	if err != nil {
		return fmt.Errorf("bind editor origin port %d on loopback: %w", port, err)
	}
	srv := &http.Server{
		Handler:           editorPortServerHandler(cs, port, ln.Addr().String()),
		ReadHeaderTimeout: 10 * time.Second,
	}
	e.servers[port] = srv
	go func() { _ = srv.Serve(ln) }()
	return nil
}

// editorPortServerHandler is the whole handler a pool port's server runs. The
// Host guard (hostguard.go) is its outermost layer: a pool port serves a shell in
// the worktree, so it answers only Hosts that name this daemon — localhost and
// loopback IPs, the address the client reached, and network.allowed_hosts (the front-door name
// tailscale serve forwards, port and all). The allow-list is read per request,
// like the control listener's, so an edit needs no rebind.
func editorPortServerHandler(cs *controlServer, port int, boundAddr string) http.Handler {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { cs.editorPortHandler(port, w, r) })
	return withHostGuard(inner, editorPortListenerLabel(port), config.ListenAddrHostName(boundAddr),
		func() []string { return editorPortAllowedHosts(cs) })
}

// editorPortAllowedHosts is network.allowed_hosts from the daemon's live config.
// A control server with no manager allows only this machine's own names.
func editorPortAllowedHosts(cs *controlServer) []string {
	if cs == nil || cs.manager == nil {
		return nil
	}
	if cfg := cs.manager.Config(); cfg != nil {
		return cfg.AllowedHosts
	}
	return nil
}

// sessionForPort is the reverse of the allocation.
func (e *editorPortOrigins) sessionForPort(port int) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, p := range e.alloc {
		if p == port {
			return id, true
		}
	}
	return "", false
}

// keyLocked returns the session's key, minting it on first use.
func (e *editorPortOrigins) keyLocked(sessionID string) (string, error) {
	if k := e.keys[sessionID]; k != "" {
		return k, nil
	}
	k, err := generateToken()
	if err != nil {
		return "", err
	}
	e.keys[sessionID] = k
	return k, nil
}

// mintBootstrap issues a single-use token for sessionID, bounded per session.
func (e *editorPortOrigins) mintBootstrap(sessionID string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.keyLocked(sessionID); err != nil {
		return "", err
	}
	now := e.now()
	pending := 0
	for tok, b := range e.boots {
		if now.After(b.expires) {
			delete(e.boots, tok)
		} else if b.sessionID == sessionID {
			pending++
		}
	}
	if pending >= editorPortMaxPendingBoot {
		for tok, b := range e.boots {
			if b.sessionID == sessionID {
				delete(e.boots, tok)
			}
		}
	}
	tok, err := generateToken()
	if err != nil {
		return "", err
	}
	e.boots[tok] = editorBootstrap{sessionID: sessionID, expires: now.Add(editorPortBootstrapTTL)}
	return tok, nil
}

// consumeBootstrap spends tok for sessionID: valid once, for that session only.
func (e *editorPortOrigins) consumeBootstrap(tok, sessionID string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, ok := e.boots[tok]
	if !ok {
		return "", false
	}
	delete(e.boots, tok) // spent whether or not it was valid for this port
	if e.now().After(b.expires) || b.sessionID != sessionID {
		return "", false
	}
	key := e.keys[sessionID]
	return key, key != ""
}

// invalidate withdraws every credential of a session whose editor is gone
// (archive, kill, restart of the editor).
func (e *editorPortOrigins) invalidate(sessionID string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.keys, sessionID)
	for tok, b := range e.boots {
		if b.sessionID == sessionID {
			delete(e.boots, tok)
		}
	}
}

func (e *editorPortOrigins) keyMatches(sessionID, presented string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	key := e.keys[sessionID]
	return key != "" && presented != "" && subtle.ConstantTimeCompare([]byte(key), []byte(presented)) == 1
}

func (e *editorPortOrigins) epochFor(port int) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.epochs[port]
}

func editorPortEpochCookieName(port int) string {
	return editorPortCookiePrefix + "epoch_" + strconv.Itoa(port)
}

func editorPortCookieName(port int) string { return editorPortCookiePrefix + strconv.Itoa(port) }

// editorPortHandler serves one pool port: only its session's editor, only to a
// request carrying that session's cookie (or spending its bootstrap token).
func (cs *controlServer) editorPortHandler(port int, w http.ResponseWriter, r *http.Request) {
	m := cs.manager
	if m == nil || m.editorPorts == nil {
		writeHTTPError(w, r, http.StatusServiceUnavailable, errors.New("daemon has no session manager"))
		return
	}
	sid, ok := m.editorPorts.sessionForPort(port)
	if !ok {
		writeEditorPortExpiredPage(w)
		return
	}
	if r.URL.Path == editorPortProbePath && r.Method == http.MethodGet {
		writeEditorPortProbePage(w)
		return
	}
	if r.URL.Path == editorPortBootstrapPath && r.Method == http.MethodGet {
		key, ok := m.editorPorts.consumeBootstrap(r.URL.Query().Get("t"), sid)
		if !ok {
			w.WriteHeader(http.StatusForbidden)
			writeEditorPortExpiredPage(w)
			return
		}
		epoch := m.editorPorts.epochFor(port)
		secure := requestIsHTTPS(r)
		http.SetCookie(w, &http.Cookie{
			Name:     editorPortCookieName(port),
			Value:    key,
			Path:     "/",
			HttpOnly: true,
			Secure:   secure,
			SameSite: http.SameSiteStrictMode,
			MaxAge:   30 * 24 * 3600,
		})
		// The epoch cookie records which owner of this port the browser's storage
		// belongs to. Long-lived on purpose: expiring it would clear a session's own
		// editor state for nothing.
		http.SetCookie(w, &http.Cookie{
			Name:     editorPortEpochCookieName(port),
			Value:    epoch,
			Path:     "/",
			HttpOnly: true,
			Secure:   secure,
			SameSite: http.SameSiteStrictMode,
			MaxAge:   400 * 24 * 3600,
		})
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if ec, err := r.Cookie(editorPortEpochCookieName(port)); err == nil && epoch != "" && ec.Value == epoch {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		// First visit, or this port has been reassigned since: whatever this browser
		// holds for the origin belongs to a previous session's editor (or to nobody),
		// and the new session's editor must not inherit it.
		writeEditorPortClearPage(w)
		return
	}
	c, err := r.Cookie(editorPortCookieName(port))
	if err != nil || !m.editorPorts.keyMatches(sid, c.Value) {
		w.WriteHeader(http.StatusUnauthorized)
		writeEditorPortExpiredPage(w)
		return
	}
	// The key is only as good as the session behind it.
	inst := m.trackedStreamSession(sid)
	tabID, live := m.liveVSCodeTab(sid)
	if inst == nil || inst.GetLiveness() == session.LiveArchived || !live {
		m.editorPorts.invalidate(sid)
		writeEditorPortExpiredPage(w)
		return
	}
	if stripAfCredentialQuery(w, r) {
		return
	}
	escaped := strings.TrimPrefix(r.URL.EscapedPath(), "/")
	rest, err := url.PathUnescape(escaped)
	if err != nil {
		writeHTTPError(w, r, http.StatusBadRequest, errors.New("invalid editor path"))
		return
	}
	editorPortServe(cs, w, r, webTabRoute{
		sessionID:     sid,
		tabID:         tabID,
		prefix:        "",
		escapedRest:   escaped,
		rest:          rest,
		previewOrigin: true,
	})
}

// editorPortProbeMessage is what the probe page posts to its parent: a fixed,
// secret-free string, so the signal is that it arrived at all.
const editorPortProbeMessage = "af-editor-origin-ok"

// writeEditorPortProbePage answers the browser's reachability probe (#6002). The
// daemon cannot tell whether the viewer can reach an allocated editor port (it may
// not be forwarded or exposed through `tailscale serve`), so the client frames this
// page before navigating. It is served before the key check — the viewer has no
// credential yet and the page carries nothing — and no-store so a stale yes cannot
// outlive the listener.
func writeEditorPortProbePage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.WriteString(w, `<!doctype html><html><head><meta charset="utf-8"><title>af editor probe</title></head>`+
		`<body><script>parent.postMessage(`+strconv.Quote(editorPortProbeMessage)+`,"*")</script></body></html>`)
}

// editorPortServe is the proxy core an authorized request reaches; a var so a test
// can observe that the gate admitted a request without starting a real editor.
var editorPortServe = func(cs *controlServer, w http.ResponseWriter, r *http.Request, route webTabRoute) {
	cs.serveWebTabRoute(w, r, route)
}

func writeEditorPortExpiredPage(w http.ResponseWriter) {
	writeTabNoticePage(w, "Editor address expired",
		"This editor needs a fresh session key — reload the af web UI to reopen it.", false)
}

type editorOriginResponse struct {
	Origin       string `json:"origin"`
	BootstrapURL string `json:"bootstrap_url"`
	Port         int    `json:"port"`
}

// editorOriginHandler answers POST /v1/editor-origin?session=<sid> on the control
// listener, behind the operator token like every control route. POST so a
// cross-origin no-cors GET cannot drive an allocation.
func (cs *controlServer) editorOriginHandler(w http.ResponseWriter, r *http.Request) {
	m := cs.manager
	if m == nil {
		writeHTTPError(w, r, http.StatusServiceUnavailable, errors.New("daemon has no session manager"))
		return
	}
	sid := r.URL.Query().Get("session")
	if sid == "" {
		writeHTTPError(w, r, http.StatusBadRequest, errors.New("/v1/editor-origin requires a non-empty session query param"))
		return
	}
	if inst := m.trackedStreamSession(sid); inst == nil || inst.GetLiveness() == session.LiveArchived {
		writeHTTPError(w, r, http.StatusNotFound, errors.New("no live session with that id"))
		return
	}
	if _, live := m.liveVSCodeTab(sid); !live {
		writeHTTPError(w, r, http.StatusConflict, errors.New("the session has no editor tab"))
		return
	}
	port, err := m.editorPortFor(cs, sid)
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, errEditorPortsOff) {
			status = http.StatusNotFound
		}
		writeHTTPError(w, r, status, err)
		return
	}
	tok, err := m.editorPorts.mintBootstrap(sid)
	if err != nil {
		writeHTTPError(w, r, http.StatusInternalServerError, err)
		return
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	scheme := "http"
	if requestIsHTTPS(r) {
		scheme = "https"
	}
	origin := scheme + "://" + net.JoinHostPort(host, strconv.Itoa(port))
	w.Header().Set("Cache-Control", "no-store")
	writeHTTPSuccess(w, r, editorOriginResponse{
		Origin:       origin,
		BootstrapURL: origin + editorPortBootstrapPath + "?t=" + url.QueryEscape(tok),
		Port:         port,
	})
}

// editorPortClearScript removes every origin-scoped store a browser may hold for the
// origin, then enters the editor. It is the fallback for what Clear-Site-Data cannot
// do: that header is honoured only in a secure context, and this origin may be plain
// HTTP on a tailnet. Both run; either alone suffices where it works.
const editorPortClearScript = `(async function(){
var done=function(){location.replace("/");};
try{localStorage.clear();}catch(e){}
try{sessionStorage.clear();}catch(e){}
try{
 if(navigator.serviceWorker){var rs=await navigator.serviceWorker.getRegistrations();await Promise.all(rs.map(function(r){return r.unregister();}));}
}catch(e){}
try{
 if(window.caches){var ks=await caches.keys();await Promise.all(ks.map(function(k){return caches.delete(k);}));}
}catch(e){}
try{
 if(indexedDB&&indexedDB.databases){var dbs=await indexedDB.databases();await Promise.all(dbs.map(function(d){return new Promise(function(res){var q=indexedDB.deleteDatabase(d.name);q.onsuccess=q.onerror=q.onblocked=function(){res();};});}));}
}catch(e){}
done();
})();`

// writeEditorPortClearPage answers a bootstrap whose browser may hold a previous
// owner's storage for this origin: it clears it (Clear-Site-Data where the browser
// honours it, the script everywhere) and then enters the editor. The cookies were
// set on this same response and survive: "cookies" is deliberately not in the header.
func writeEditorPortClearPage(w http.ResponseWriter) {
	w.Header().Set("Clear-Site-Data", `"cache", "storage"`)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>Opening editor…</title>`+
		`<noscript><meta http-equiv="refresh" content="0;url=/"></noscript>`+
		`<p>Opening editor…</p><script>%s</script>`, editorPortClearScript)
}
