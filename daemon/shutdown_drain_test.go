package daemon

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPRequestDrainJoinsDispatchedHandlerAndRefusesLaterRequest(t *testing.T) {
	drain := newHTTPRequestDrain()
	server := &controlServer{httpRequests: drain}
	entered := make(chan struct{})
	release := make(chan struct{})
	handler := server.trackHTTPRPC(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	})
	handlerDone := make(chan struct{})
	go func() {
		handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/Test", nil))
		close(handlerDone)
	}()
	<-entered

	drain.closeAdmission()
	drained := make(chan struct{})
	go func() {
		drain.wait()
		close(drained)
	}()
	select {
	case <-drained:
		t.Fatal("HTTP drain returned while a dispatched handler was still running")
	default:
	}

	rejected := httptest.NewRecorder()
	handler(rejected, httptest.NewRequest(http.MethodPost, "/v1/Test", nil))
	if rejected.Code != http.StatusServiceUnavailable || !strings.Contains(rejected.Body.String(), "quiescing") {
		t.Fatalf("request admitted after the HTTP drain closed: status=%d body=%q", rejected.Code, rejected.Body.String())
	}
	close(release)
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("dispatched HTTP handler did not return")
	}
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("HTTP drain did not join the dispatched handler")
	}
}

func TestBackgroundMutationShutdownClosesAdmissionAndJoinsWriter(t *testing.T) {
	manager := &Manager{}
	entered := make(chan struct{})
	release := make(chan struct{})
	if !manager.launchBackgroundMutation(func(<-chan struct{}) {
		close(entered)
		<-release
	}) {
		t.Fatal("initial background writer was refused")
	}
	<-entered
	manager.backgroundMutationMu.Lock()
	stop := manager.backgroundMutationStop
	manager.backgroundMutationMu.Unlock()

	drained := make(chan struct{})
	go func() {
		manager.stopAndWaitBackgroundMutationsForShutdown()
		close(drained)
	}()
	<-stop
	if manager.launchBackgroundMutation(func(<-chan struct{}) {}) {
		t.Fatal("background writer was admitted after shutdown closed the gate")
	}
	select {
	case <-drained:
		t.Fatal("background drain returned while a registered writer was still running")
	default:
	}
	close(release)
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("background drain did not join the registered writer")
	}
}

func TestBackgroundMutationShutdownAbandonsPermanentlyStalledGhostCleanup(t *testing.T) {
	manager := &Manager{}
	lateResult := make(chan error)
	manager.reconcileLateGhostCleanup("repo", "ghost", "repo\x00ghost", "ghost-id", lateResult)

	drained := make(chan struct{})
	go func() {
		manager.stopAndWaitBackgroundMutationsForShutdown()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("shutdown remained blocked on a permanently stalled ghost cleanup worker")
	}
	manager.lateGhostCleanupWG.Wait()

	if _, admitted := manager.beginBackgroundMutation(); admitted {
		manager.backgroundMutationWG.Done()
		t.Fatal("late ghost checkpoint callback was admitted after shutdown")
	}
}
