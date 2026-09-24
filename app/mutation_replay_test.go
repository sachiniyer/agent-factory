package app

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/task"
)

// routeCounter counts the requests each /v1 route received.
type routeCounter struct {
	mu   sync.Mutex
	hits map[string]int
}

func (r *routeCounter) add(path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hits[path]++
	return r.hits[path]
}

func (r *routeCounter) get(path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits[path]
}

// serveDaemonHTTP binds a Unix-socket HTTP server at sockPath. handle runs for
// every request with the request's 1-based arrival count on its route.
func serveDaemonHTTP(t *testing.T, sockPath string, handle func(w http.ResponseWriter, r *http.Request, n int)) *routeCounter {
	t.Helper()
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	counter := &routeCounter{hits: map[string]int{}}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handle(w, r, counter.add(r.URL.Path))
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return counter
}

// pointDaemonHTTPAt replaces only the ensure+dial step, so the production
// wrappers and retry policy run unchanged against a socket the test owns. Not
// parallel-safe: it swaps a package var.
func pointDaemonHTTPAt(t *testing.T, sockPath string) {
	t.Helper()
	prev := daemonHTTPClient
	t.Cleanup(func() { daemonHTTPClient = prev })
	daemonHTTPClient = func() (*apiclient.Client, error) { return apiclient.NewWithSocket(sockPath), nil }
}

// tuiMutations is every TUI seam that changes daemon state through the HTTP
// launcher, keyed by the /v1 route its apiclient method actually posts to. Enumerated from session_control.go:
// a new mutation seam belongs here, which is what holds it to the no-replay rule.
func tuiMutations() map[string]func() error {
	expect := task.ProjectExpectation{}
	return map[string]func() error{
		"/v1/CreateSession": func() error {
			_, err := startSessionThroughDaemon(nil, sessionStartRequest{Title: "s", RepoPath: "/r"})
			return err
		},
		"/v1/KillSession": func() error { return killSessionThroughDaemon(daemon.KillSessionRequest{ID: "i"}) },
		"/v1/ArchiveSession": func() error {
			_, err := archiveSessionThroughDaemon(daemon.ArchiveSessionRequest{ID: "i"})
			return err
		},
		"/v1/RestoreSession": func() error {
			_, err := restoreSessionThroughDaemon(daemon.RestoreSessionRequest{ID: "i"})
			return err
		},
		// The handoff client posts to the account-aware route, not /v1/HandoffSession.
		"/v1/" + daemon.AccountAwareHandoffMethod: func() error {
			_, err := handoffSessionThroughDaemon(daemon.HandoffSessionRequest{ID: "i"})
			return err
		},
		"/v1/ResumeFromLimit": func() error { return resumeFromLimitThroughDaemon(daemon.ResumeFromLimitRequest{ID: "i"}) },
		"/v1/CreateTab": func() error {
			_, err := createTabThroughDaemon(daemon.CreateTabRequest{ID: "i"})
			return err
		},
		"/v1/CloseTab": func() error { return closeTabThroughDaemon(daemon.CloseTabRequest{ID: "i"}) },
		"/v1/RenameTab": func() error {
			_, err := renameTabThroughDaemon(daemon.RenameTabRequest{ID: "i"})
			return err
		},
		"/v1/AddTask":    func() error { return addTaskThroughDaemon(task.Task{ID: "t", Name: "n"}) },
		"/v1/UpdateTask": func() error { return updateTaskThroughDaemon("t", task.TaskUpdate{}, expect) },
		"/v1/RemoveTask": func() error { return removeTaskThroughDaemon("t", expect) },
		"/v1/TriggerTask": func() error {
			return triggerTaskThroughDaemon("t", expect)
		},
		"/v1/RegisterProject": func() error { return registerProjectThroughDaemon("/r") },
		"/v1/DeleteProject": func() error {
			_, err := deleteProjectThroughDaemon("/r", "repo")
			return err
		},
		"/v1/PauseStatusPoll": func() error {
			return pauseStatusPollThroughDaemon(daemon.PauseStatusPollRequest{ID: "i"})
		},
		"/v1/ResumeStatusPoll": func() error {
			return resumeStatusPollThroughDaemon(daemon.ResumeStatusPollRequest{ID: "i"})
		},
	}
}

// TestMutationCommittedThenReplyLost_SentExactlyOnce is the #4820 regression
// lock. The daemon runs the handler — the mutation commits — and the connection
// drops before the reply. Every later attempt would succeed, so a replaying
// launcher reports success and has sent the mutation twice: a duplicate session,
// tab, task or run, or an overwrite of a concurrent change. The fix sends it
// once and surfaces the outcome as uncertain.
func TestMutationCommittedThenReplyLost_SentExactlyOnce(t *testing.T) {
	sock := testguard.SocketPath(t, "daemon-http.sock")
	counter := serveDaemonHTTP(t, sock, func(w http.ResponseWriter, _ *http.Request, n int) {
		if n > 1 {
			// A replay: answer success so the replaying launcher would stop here.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{},"error":null}`))
			return
		}
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	})
	pointDaemonHTTPAt(t, sock)

	mutations := tuiMutations()
	if len(mutations) != 17 {
		t.Fatalf("expected the 17 TUI mutation seams #4820 enumerates, got %d", len(mutations))
	}
	for route, call := range mutations {
		t.Run(strings.TrimPrefix(route, "/v1/"), func(t *testing.T) {
			err := call()
			if got := counter.get(route); got != 1 {
				t.Fatalf("%s was sent %d times; a mutation the daemon may have received must be sent exactly once", route, got)
			}
			if err == nil {
				t.Fatal("a lost reply must not read as success")
			}
			if !apiclient.IsMutationOutcomeUncertain(err) {
				t.Fatalf("a lost reply must surface as an uncertain outcome, got %v", err)
			}
		})
	}
}

// TestMutationRefusedDial_StillRetried is the control: while the daemon's HTTP
// socket has not bound yet, nothing was sent, so the launcher keeps the bind-race
// retry for mutations and the call lands once the socket appears.
func TestMutationRefusedDial_StillRetried(t *testing.T) {
	sock := testguard.SocketPath(t, "daemon-http.sock")
	pointDaemonHTTPAt(t, sock)

	counterCh := make(chan *routeCounter, 1)
	go func() {
		time.Sleep(3 * daemonHTTPRetryPoll)
		counterCh <- serveDaemonHTTP(t, sock, func(w http.ResponseWriter, _ *http.Request, _ int) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{},"error":null}`))
		})
	}()

	if err := addTaskThroughDaemon(task.Task{ID: "t", Name: "n"}); err != nil {
		t.Fatalf("a mutation fired during the bind race must be retried until it lands, got %v", err)
	}
	if got := (<-counterCh).get("/v1/AddTask"); got != 1 {
		t.Fatalf("AddTask reached the daemon %d times, want 1", got)
	}
}

// TestMutationAdmissionRefusal_StillRetried keeps the other half of the retry:
// the daemon refuses admission ("still restoring") without running the handler,
// so re-sending is safe and the call lands once admission opens.
func TestMutationAdmissionRefusal_StillRetried(t *testing.T) {
	sock := testguard.SocketPath(t, "daemon-http.sock")
	counter := serveDaemonHTTP(t, sock, func(w http.ResponseWriter, _ *http.Request, n int) {
		w.Header().Set("Content-Type", "application/json")
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"data":null,"error":{"message":"agent-factory daemon is starting (restoring sessions); retry shortly"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{},"error":null}`))
	})
	pointDaemonHTTPAt(t, sock)

	if err := addTaskThroughDaemon(task.Task{ID: "t", Name: "n"}); err != nil {
		t.Fatalf("an admission refusal must be retried until admission opens, got %v", err)
	}
	if got := counter.get("/v1/AddTask"); got != 3 {
		t.Fatalf("AddTask reached the daemon %d times, want 3 (two refusals, one success)", got)
	}
}

// TestReadReplyLost_StillRetried pins that reads keep today's behavior: a read
// that loses its reply is re-sent, because re-reading changes nothing.
func TestReadReplyLost_StillRetried(t *testing.T) {
	sock := testguard.SocketPath(t, "daemon-http.sock")
	counter := serveDaemonHTTP(t, sock, func(w http.ResponseWriter, _ *http.Request, n int) {
		if n == 1 {
			if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
				_ = conn.Close()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"instances":[]},"error":null}`))
	})
	pointDaemonHTTPAt(t, sock)

	if _, err := snapshotThroughDaemon("repo"); err != nil {
		t.Fatalf("a read must be retried after a lost reply, got %v", err)
	}
	if got := counter.get("/v1/Snapshot"); got != 2 {
		t.Fatalf("Snapshot reached the daemon %d times, want 2", got)
	}
}
