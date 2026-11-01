package daemon

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// TestIsoProofServer is NOT a unit test: it is the server half of the real-code-server
// Chromium proof for #6459. Gated by AF_ISO_PROOF_SERVER so a normal `go test` skips
// it. It stands up the real webtab proxy (the mirror route and an editor-origin port)
// in front of a REAL code-server — spawned through the production unix-socket path by
// the Manager, via the wrapper at AF_ISO_CODE_SERVER (named "code-server", so the
// code-server dialect is used) — prints one ISOPROOF_READY line for the driver, and
// blocks until signalled. It MUST run inside the Chromium rig container, never on the
// host (the no-local-daemon-test rule): run.sh starts it there with -timeout 0.
func TestIsoProofServer(t *testing.T) {
	if os.Getenv("AF_ISO_PROOF_SERVER") == "" {
		t.Skip("iso-proof harness server; set AF_ISO_PROOF_SERVER=1 (container only)")
	}
	wrapper := os.Getenv("AF_ISO_CODE_SERVER")
	if wrapper == "" {
		t.Fatal("AF_ISO_CODE_SERVER must point at the code-server wrapper")
	}
	ports := os.Getenv("AF_ISO_EDITOR_PORTS")
	if ports == "" {
		ports = "9100-9101"
	}

	shortAFHome(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	cfg := config.DefaultConfig()
	cfg.EditorOriginPorts = ports
	manager, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	manager.readyOnce.Do(func() { close(manager.ready) })

	const title = "isoproof"
	inst := startedLocalTabInstance(t, manager, repo.ID, repoPath, title, "af_"+title+"_agent")
	manager.vscode.configuredBinary = func() string { return wrapper }
	manager.vscode.startGrace = 120 * time.Second
	t.Cleanup(manager.vscode.Stop)

	if _, err := manager.CreateTab(CreateTabRequest{Title: title, RepoID: repo.ID, Kind: "vscode"}); err != nil {
		t.Fatalf("CreateTab(vscode): %v", err)
	}
	wt := inst.GetWorktreePath()
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, "README.md"), []byte("# Iso Proof\n\npreview me\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	cs := &controlServer{manager: manager}
	mirrorSrv := httptest.NewServer(newHTTPMux(cs))
	t.Cleanup(mirrorSrv.Close)
	mirror := mirrorSrv.URL

	// A permissive same-host page on ANOTHER port — the cross-port attacker origin.
	// It is same-site (127.0.0.1, ports ignored) with the editor-origin port, so a
	// SameSite=Strict editor cookie rides a request it makes, and it is a different
	// origin, so framing the editor's webview host from it is the cross-port case.
	// A plain page with no CSP, unlike the workbench, so the frame actually loads.
	attackerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html><title>x</title>"))
	}))
	t.Cleanup(attackerSrv.Close)

	port, err := manager.editorPortFor(cs, inst.ID)
	if err != nil {
		t.Fatalf("editorPortFor: %v", err)
	}
	tok, err := manager.editorPorts.mintBootstrap(inst.ID)
	if err != nil {
		t.Fatalf("mintBootstrap: %v", err)
	}
	tabID, ok := manager.liveVSCodeTab(inst.ID)
	if !ok {
		t.Fatal("no live vscode tab")
	}

	fmt.Printf("ISOPROOF_READY mirror=%s attacker=%s sid=%s vstab=%s editor_port=%d bootstrap=%s?t=%s\n",
		mirror, attackerSrv.URL, inst.ID, tabID, port, editorPortBootstrapPath, tok)
	os.Stdout.Sync()

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
}
