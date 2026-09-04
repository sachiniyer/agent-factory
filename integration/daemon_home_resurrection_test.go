package integration_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// TestDaemonDoesNotResurrectAHomeDeletedUnderIt is the end-to-end oracle for
// #3845: an `rm -rf` of a live daemon's AF home must STAY done, and the daemon
// must notice and shut itself down.
//
// #3843 closed the socket binds. This is the write side, and it is the one that
// was actually observed: with a session on the box, the daemon's status poll
// saves instances/<repoID>/instances.json every tick, and that write's
// os.MkdirAll re-created the whole home 0.5s after the deletion. applyHomeCheck
// then stats a home that is present again and clears its consecutive-miss
// counter, so watchDaemonHome (#1093/#1094) never reaches its threshold and the
// daemon runs forever against a directory the user deleted — the 9,892 /tmp/af-*
// dirs #3842 counted.
//
// Anti-vacuity: "the home did not come back" is trivially true of a daemon that
// is not writing, so the test pins that the daemon is ALIVE for the whole
// observation window (it polls every 100ms in this config, so it is attempting
// the save that used to resurrect the home dozens of times), and it requires the
// pre-deletion instances file to exist first, so the exact write path under test
// is known to be live for this session.
func TestDaemonDoesNotResurrectAHomeDeletedUnderIt(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real daemon and waits out two home checks; skipped under -short — see #2052")
	}

	// The daemon is a separate process, so its 60s home check is only reachable
	// through the environment it inherits from the CLI that spawns it. Set BEFORE
	// newHarness, whose first `af` call is what starts the daemon.
	const homeCheck = 2 * time.Second
	t.Setenv("AF_HOME_CHECK_INTERVAL", homeCheck.String())

	h := newHarness(t)
	// A session gives the status poll something to persist every tick. Without
	// one the daemon is idle and writes nothing, and the deletion would go
	// unchallenged for reasons that have nothing to do with the fix.
	h.createSession("doomed")
	pid := readDaemonPID(t, h.home)

	instances := filepath.Join(h.home, "instances")
	waitUntil(t, 30*time.Second, "the daemon to persist this session's instances", func() bool {
		_, err := os.Stat(instances)
		return err == nil
	})

	if err := os.RemoveAll(h.home); err != nil {
		t.Fatalf("remove the daemon's home: %v", err)
	}

	// homeMissingChecksToExit is 2, so the daemon exits between one and two
	// intervals after the deletion; the budget is generous over that because a
	// loaded box may stretch the ticker, and a slow exit is not the failure this
	// test is about.
	deadline := time.Now().Add(6 * homeCheck)
	sawDaemonAlive := false
	exited := false
	for time.Now().Before(deadline) {
		if _, err := os.Stat(h.home); err == nil {
			var came []string
			if entries, readErr := os.ReadDir(h.home); readErr == nil {
				for _, e := range entries {
					came = append(came, e.Name())
				}
			}
			t.Fatalf("the daemon re-created its deleted home %s (holding %v); it will now never "+
				"observe the deletion and will keep firing schedules forever (#3845)", h.home, came)
		}
		if !pidAlive(pid) {
			exited = true
			break
		}
		sawDaemonAlive = true
		time.Sleep(50 * time.Millisecond)
	}

	if !sawDaemonAlive {
		t.Fatalf("daemon pid %d was never observed alive after the deletion, so nothing proves it "+
			"attempted the state write this test is about", pid)
	}
	if !exited {
		t.Fatalf("daemon pid %d is still running %v after its home was deleted; watchDaemonHome "+
			"should have shut it down within two %v home checks (#1093/#3845)", pid, 6*homeCheck, homeCheck)
	}
}

// TestSessionCreateDoesNotResurrectAHomeDeletedUnderIt is the end-to-end oracle
// for #3850, the door #3846 left open.
//
// #3846 latched the STATE WRITES. It is not on the session-create path, and on
// that path every unguarded os.MkdirAll runs BEFORE the first write the latch
// could refuse: daemon/manager_create.go starts the session (Provision →
// injectSystemPrompt → ensurePluginDir) and only registers and persists the row
// afterwards. So a create against a deleted home used to re-create it as an
// ancestor of <home>/plugin/commands, applyHomeCheck's consecutive-miss counter
// cleared on the next tick, and the abandoned daemon ran forever — the #1093
// outcome through a fourth door. Two of the three routes to CreateSession are
// automatic (delivery auto-create, the root-agent ensure loop), so this needs no
// user present.
//
// Anti-vacuity, in three parts. The FIRST create is required to succeed and to
// leave <home>/plugin/commands behind, which pins that this configuration really
// does reach ensurePluginDir — the exact site under test — rather than skipping
// it for some unrelated reason. The stand-in agent lives OUTSIDE the home, so
// the second create cannot fail merely because its program was deleted along
// with the directory: the assertion is about the home, not the agent. And the
// second create's error must NAME the removed home, so "the create failed" is
// never enough on its own.
//
// The create is driven over a connection dialled BEFORE the deletion. The
// daemon's HTTP plane listens on <home>/daemon-http.sock, and `rm -rf` takes
// that path with it — a CLI create would find no socket, autostart a SECOND
// daemon, and legitimately create the home, which is the install path rather
// than the defect. An already-established unix connection survives the unlink,
// so this drives the RPC into the daemon that is actually under test.
func TestSessionCreateDoesNotResurrectAHomeDeletedUnderIt(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real daemon and waits out two home checks; skipped under -short — see #2052")
	}

	for _, tc := range []struct {
		name         string
		worktreeRoot string
	}{
		// The default. The worktree lands beside the repo, so the ONLY home
		// creates on this launch are the agent-skill ones — ensurePluginDir.
		{"sibling worktrees", config.WorktreeRootSibling},
		// worktree_root=subdirectory puts the worktree root at <home>/worktrees,
		// so session/git/worktree_ops.go's MkdirAll is a second, earlier door on
		// the same launch (#3850's table).
		{"subdirectory worktrees", config.WorktreeRootSubdirectory},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertCreateLeavesADeletedHomeDeleted(t, tc.worktreeRoot)
		})
	}
}

func assertCreateLeavesADeletedHomeDeleted(t *testing.T, worktreeRoot string) {
	t.Helper()

	// The daemon is a separate process, so its 60s home check is only reachable
	// through the environment it inherits. Set BEFORE newHarness.
	//
	// Longer than the other test's 2s on purpose: the create under test must
	// finish before the daemon shuts ITSELF down, or the pinned connection dies
	// mid-RPC and there is no error to read the home's name out of. The exit
	// lands two intervals (20s) after the deletion and a create answers in a few
	// seconds, so this is headroom for a loaded runner rather than a wait.
	const homeCheck = 10 * time.Second
	t.Setenv("AF_HOME_CHECK_INTERVAL", homeCheck.String())

	h := newHarness(t)

	// The stand-in agent must OUTLIVE the home. newHarness writes it into the
	// home, where `rm -rf` would take it, and a create that fails with "no such
	// file or directory" for the program proves nothing about the home. Rewrite
	// the config (still before the daemon's first start, so it is the config the
	// daemon loads) to point at a copy outside the home.
	//
	// Named `claude`, not `fake-agent.sh` like the shared harness's: injection is
	// selected by DetectAgentFromCommand, which matches on the command's BASENAME
	// (session/tmux/resume.go:343). Under any other name injectSystemPrompt takes
	// its "no known agent" branch, ensurePluginDir never runs, and the site this
	// test exists for is never reached — which is exactly what the plugin/commands
	// assertion below caught on the first run.
	agent := filepath.Join(t.TempDir(), "claude")
	writeFile(t, agent, "#!/bin/sh\nprintf '❯ '\nexec cat\n", 0755)
	cfg := testConfig()
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: agent}
	cfg.WorktreeRoot = worktreeRoot
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	writeFile(t, filepath.Join(h.home, config.ConfigFileName), string(raw), 0644)

	h.startDaemon()
	pid := readDaemonPID(t, h.home)

	// Anti-vacuity 1: this configuration reaches ensurePluginDir, and that is
	// the site whose MkdirAll used to bring the home back.
	h.createSession("before-deletion")
	pluginCommands := filepath.Join(h.home, "plugin", "commands")
	if _, err := os.Stat(pluginCommands); err != nil {
		t.Fatalf("a claude create did not produce %s (%v); this test cannot show that a "+
			"create resurrects the home if the launch never creates a directory under it", pluginCommands, err)
	}

	// Dialled while the socket still has a name. Everything after this point
	// speaks to the daemon over a connection the deletion cannot take away.
	conn, err := net.Dial("unix", filepath.Join(h.home, "daemon-http.sock"))
	if err != nil {
		t.Fatalf("dial the daemon's HTTP socket before deleting its home: %v", err)
	}
	defer conn.Close()

	deletedAt := time.Now()
	if err := os.RemoveAll(h.home); err != nil {
		t.Fatalf("remove the daemon's home: %v", err)
	}

	createErr := postOverConn(conn, "/v1/CreateSession", map[string]string{
		"title":     "after-deletion",
		"repo_path": h.repo,
		"program":   tmux.ProgramClaude,
	}, 45*time.Second)
	if createErr == nil {
		t.Errorf("the create against a deleted home SUCCEEDED; it must be refused rather than " +
			"quietly re-creating the home it is writing into (#3850)")
	} else if !strings.Contains(createErr.Error(), h.home) {
		// Name the one benign cause of an unnamed error, so a future reader does
		// not have to rediscover it: a daemon that self-terminated while the create
		// was still running takes the pinned connection with it, and the client is
		// left with an EOF that says nothing about the home. It is still a failure
		// — the create's refusal is what this test asserts — but it is fixed by
		// raising homeCheck, not by chasing the guard.
		hint := ""
		if !pidAlive(pid) {
			hint = fmt.Sprintf(" (the daemon had already exited, so this may be the RPC being cut off "+
				"rather than refused — raise homeCheck above %v if a slow runner keeps landing here)", homeCheck)
		}
		t.Errorf("the create failed with %v, which does not name the removed home %s%s; a create that "+
			"fails for some other reason proves nothing about the home guard (#3850)", createErr, h.home, hint)
	}

	// homeMissingChecksToExit is 2, so the daemon exits between one and two
	// intervals after the deletion. The budget is generous over that because a
	// loaded box stretches the ticker, and a slow exit is not the failure this
	// test is about; the home coming back at ANY point is.
	deadline := deletedAt.Add(4 * homeCheck)
	sawDaemonAlive := false
	exited := false
	for time.Now().Before(deadline) {
		if _, err := os.Stat(h.home); err == nil {
			var came []string
			if entries, readErr := os.ReadDir(h.home); readErr == nil {
				for _, e := range entries {
					came = append(came, e.Name())
				}
			}
			t.Fatalf("the session create re-created the daemon's deleted home %s (holding %v); it will "+
				"now never observe the deletion and will keep firing schedules forever (#3850)", h.home, came)
		}
		if !pidAlive(pid) {
			exited = true
			break
		}
		sawDaemonAlive = true
		time.Sleep(50 * time.Millisecond)
	}

	if !sawDaemonAlive {
		t.Fatalf("daemon pid %d was never observed alive after the deletion, so nothing proves the "+
			"create this test drove ran against a live daemon", pid)
	}
	if !exited {
		t.Fatalf("daemon pid %d is still running %v after its home was deleted; watchDaemonHome should "+
			"have shut it down within two %v home checks (#1093/#3845/#3850)", pid, 4*homeCheck, homeCheck)
	}
}

// postOverConn writes one HTTP request onto an ALREADY-DIALLED connection and
// returns the daemon's envelope error, if any.
//
// It exists because http.Client cannot be told "use this connection and never
// dial another": once the home is gone its socket path is gone too, so a pooled
// transport that decided to re-dial would report a connection error instead of
// the daemon's answer, and the test would be asserting on the wrong failure.
func postOverConn(conn net.Conn, path string, payload any, timeout time.Duration) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s request: %w", path, err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://unix"+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build %s request: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("set deadline on the pinned connection: %w", err)
	}
	if err := req.Write(conn); err != nil {
		return fmt.Errorf("write %s over the pinned connection: %w", path, err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return fmt.Errorf("read %s response over the pinned connection: %w", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read %s body: %w", path, err)
	}
	var env remoteHTTPEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode %s envelope: %w\n%s", path, err, raw)
	}
	if resp.StatusCode != http.StatusOK || env.Error != nil {
		msg := "<nil>"
		if env.Error != nil {
			msg = env.Error.Message
		}
		return fmt.Errorf("POST %s status=%d error=%s", path, resp.StatusCode, msg)
	}
	return nil
}
