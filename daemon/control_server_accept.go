package daemon

import (
	"errors"
	"net"
	"net/rpc"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/internal/peercred"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/log"
)

// testHookControlListener wraps the control socket listener immediately after
// bind and chmod, before the accept loop consumes it. Production is the
// identity wrapper (the listener is used as-is); a test sets it to wrap the
// listener with one that injects Accept errors, so the accept loop's
// transient-error retry / fatal-close path — unreachable otherwise, since
// startControlServer owns its listener and never returns it — can be
// exercised. The wrapper's Close delegates to the underlying listener so net
// still unlinks the socket file. See control_server_accept_test.go.
var testHookControlListener = func(l net.Listener) net.Listener { return l }

// controlAcceptFatalErr carries a fatal control-socket accept error from the
// accept goroutine to runDaemon so the process exits non-zero and the autostart
// supervisor (systemd Restart=on-failure / launchd KeepAlive.SuccessfulExit)
// restarts the daemon — instead of the clean nil return the Shutdown RPC's
// shutdownCh close produces, which leaves the wedged daemon's scheduler
// stopped until a client explicitly starts it again. fatalAcceptClose stores
// the error BEFORE closing shutdownCh, so the close→receive happens-before edge
// makes the Store visible to runDaemon's Load after <-shutdownCh fires. runDaemon
// clears it at startup so a fatal error from a prior daemon in this process
// (tests run runDaemon multiple times in one binary) cannot leak into a later
// clean shutdown. The fatal path still closes shutdownCh so the graceful
// drain + SaveInstancesForShutdown runs (state survives the restart); only the
// exit code changes (non-zero) — the supervisor restart policy keys on that.
var controlAcceptFatalErr atomic.Pointer[error]

func setFatalAcceptErr(err error) {
	e := err
	controlAcceptFatalErr.Store(&e)
}

// fatalAcceptShutdownErr returns the fatal accept error if the shutdown was
// triggered by a fatal control-socket accept error, else nil. Called by runDaemon
// at each shutdown exit point to decide the exit code.
func fatalAcceptShutdownErr() error {
	if p := controlAcceptFatalErr.Load(); p != nil {
		return *p
	}
	return nil
}

func resetFatalAcceptErr() {
	controlAcceptFatalErr.Store(nil)
}

// fatalAcceptClose handles a non-recoverable accept error: it closes the
// listener so net unlinks the socket file (clients then get the recoverable
// "no such file" refusal instead of a dialable-but-dead socket that hangs
// forever) and propagates the failure to the daemon shutdown path so the
// supervisor (systemd/launchd) restarts the daemon rather than keeping a
// wedged-but-running process whose control plane is dead. It records the error
// via setFatalAcceptErr BEFORE closing shutdownCh so runDaemon returns non-zero
// (see controlAcceptFatalErr); the close still reuses the Shutdown RPC's own
// sync.Once so two independent closers of shutdownCh cannot panic. A nil
// shutdownCh (the test configuration that does not wire the daemon main loop)
// skips the shutdown trigger but still closes the listener.
func fatalAcceptClose(listener net.Listener, shutdownCh chan struct{}, shutdownOnce *sync.Once, err error) {
	_ = listener.Close()
	setFatalAcceptErr(err)
	if shutdownCh != nil {
		shutdownOnce.Do(func() { close(shutdownCh) })
	}
}

// startControlServer registers the control RPC service on the Unix socket and
// returns a cleanup function that closes the listener and accepted connections,
// then joins methods already dispatched (the listener close also unlinks the
// socket file). When shutdownCh is non-nil, the Shutdown RPC will close it on the
// first invocation, allowing the daemon main loop to exit on RPC request.
// scheduler may be nil for servers that do not host task schedules (tests);
// the ReloadTasks RPC then returns an error. watchers may likewise be nil,
// in which case ReloadTasks only refreshes cron entries.
func startControlServer(manager *Manager, scheduler *taskScheduler, watchers *watcherSupervisor, shutdownCh chan struct{}) (func() error, error) {
	socketPath, err := DaemonSocketPath()
	if err != nil {
		return nil, err
	}
	if err := requireDaemonHomePresent("daemon control socket", socketPath); err != nil {
		return nil, err
	}
	_ = os.Remove(socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	listener = testHookControlListener(listener)

	// Every accepted connection gets its own rpc.Server because its
	// controlServer carries that connection's kernel-verified requester
	// identity (#5182): SO_PEERCRED/LOCAL_PEERPID names the process that
	// CONNECTED, resolved to its (pid, start-stamp) instance at accept, and
	// teardown handlers register it so the reaper exempts the caller still
	// blocked on their reply. shutdownOnce is shared by all of them — one
	// Shutdown still fires once for the whole listener. Registration runs once
	// eagerly so a service misconfiguration fails at bind time, not per
	// connection.
	shutdownOnce := &sync.Once{}
	newConnServer := func(requester *proctree.Process, pending *pendingUntracks) (*rpc.Server, error) {
		server := rpc.NewServer()
		err := server.RegisterName(controlServiceName, &controlServer{
			manager:        manager,
			scheduler:      scheduler,
			watchers:       watchers,
			shutdownCh:     shutdownCh,
			shutdownOnce:   shutdownOnce,
			requester:      requester,
			pendingReplies: pending,
		})
		return server, err
	}
	if _, err := newConnServer(nil, nil); err != nil {
		_ = listener.Close()
		return nil, err
	}

	var (
		connectionsMu sync.Mutex
		connections   = make(map[net.Conn]struct{})
		closing       bool
		serveWG       sync.WaitGroup
		closeOnce     sync.Once
		closeErr      error
	)
	serveWG.Add(1)
	go func() {
		defer serveWG.Done()
		// Classify accept errors, not treat all as fatal. A bare return on
		// any error used to silently kill the control plane on a transient
		// EMFILE/ENFILE (#718/#767 orphan class); net/http retries.
		var tempDelay time.Duration
		for {
			conn, err := listener.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return // shutdown/cleanup closed the listener
				}
				if errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) {
					// Transient fd exhaustion: retry with exponential backoff so
					// a momentary shortage cannot wedge the plane.
					if tempDelay == 0 {
						tempDelay = 5 * time.Millisecond
					} else {
						tempDelay *= 2
					}
					if max := time.Second; tempDelay > max {
						tempDelay = max
					}
					log.WarningLog.Printf("daemon control socket: accept error: %v; retrying in %v", err, tempDelay)
					time.Sleep(tempDelay)
					continue
				}
				// Non-recoverable: log, close the listener (net unlinks the socket so clients get "no such
				// file", not a hang), and propagate to the daemon shutdown path so the supervisor restarts it.
				log.WarningLog.Printf("daemon control socket: fatal accept error, closing listener: %v", err)
				fatalAcceptClose(listener, shutdownCh, shutdownOnce, err)
				return
			}
			tempDelay = 0
			connectionsMu.Lock()
			if closing {
				connectionsMu.Unlock()
				_ = conn.Close()
				continue
			}
			connections[conn] = struct{}{}
			serveWG.Add(1)
			connectionsMu.Unlock()
			// The kernel's record of who CONNECTED — the only requester
			// identity a teardown handler may trust (#5182). The pid is
			// resolved to its (pid, start-stamp) instance HERE, synchronously
			// in the accept loop while the peer provably still owns the slot:
			// a lookup deferred even to the freshly spawned goroutine could
			// land after the peer exited and its pid was recycled by another
			// pane-tree process, which would then inherit an exemption meant
			// for the dead caller (Codex on #5186). A read failure degrades to
			// no requester (the pre-#5182 posture): the connection still works,
			// teardown proceeds, and the caller is reaped as it always was.
			var requester *proctree.Process
			if peerPID, credErr := peercred.ConnPID(conn); credErr != nil {
				log.WarningLog.Printf("daemon control connection: cannot read peer credentials, teardown requester exemption disabled: %v", credErr)
			} else if proc, lookupErr := proctree.Lookup(peerPID); lookupErr != nil {
				log.WarningLog.Printf("daemon control connection: cannot resolve peer pid %d, teardown requester exemption disabled: %v", peerPID, lookupErr)
			} else {
				requester = &proc
			}
			go func() {
				defer serveWG.Done()
				pending := &pendingUntracks{}
				connServer, err := newConnServer(requester, pending)
				if err != nil {
					log.WarningLog.Printf("daemon control connection: cannot register RPC service: %v", err)
					return
				}
				// Our own codec, not ServeConn's built-in one: WriteResponse
				// releases one parked unregister per reply written, so an
				// exemption outlives only the response it is for — never the
				// whole connection (Codex on #5186).
				connServer.ServeCodec(newGobServerCodec(conn, pending))
				// Backstop for a codec path that returned without Close: any
				// unregister still parked is for a reply the client will never
				// read.
				pending.drain()
				connectionsMu.Lock()
				delete(connections, conn)
				connectionsMu.Unlock()
			}()
		}
	}()

	return func() error {
		closeOnce.Do(func() {
			connectionsMu.Lock()
			closing = true
			accepted := make([]net.Conn, 0, len(connections))
			for conn := range connections {
				accepted = append(accepted, conn)
			}
			connectionsMu.Unlock()

			// Closing the listener also unlinks the socket file (net's default
			// unlink-on-close for unix listeners it created). Deliberately no
			// explicit os.Remove here: between the close-unlink and an explicit
			// Remove, a new daemon can pass its ping check and bind a fresh
			// socket at the same path, and the Remove would delete the new
			// daemon's socket, orphaning it — the same race class as #718/#767.
			if err := listener.Close(); !errors.Is(err, net.ErrClosed) {
				closeErr = err
			}
			// Closing accepted connections stops them from admitting another RPC.
			// net/rpc's ServeConn waits for methods already dispatched before it
			// returns; joining our ServeConn goroutines therefore joins every call
			// that passed mutation admission before shutdown quiesced the daemon.
			for _, conn := range accepted {
				_ = conn.Close()
			}
			serveWG.Wait()
		})
		return closeErr
	}, nil
}
