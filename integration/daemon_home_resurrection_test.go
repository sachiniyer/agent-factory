package integration_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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
