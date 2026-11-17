//go:build linux

package systemdunit

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// restartJobHelperOut names the file the helper process records its job reads
// in; the helper runs as the transient unit under test.
const restartJobHelperOut = "AF_RESTARTJOB_HELPER_OUT"

// TestRestartJobHelperProcess is not a test: it is the transient unit's main
// process for TestRestartJobReadsAsRestartAndStopAsFinal. It records that it is
// up, then, on the unit's SIGTERM, reads its unit's job from inside the unit as
// the daemon does, and records the answer. It reads it once more for a pid that
// is not the unit's MainPID (systemd's own), which must answer final, then exits.
func TestRestartJobHelperProcess(t *testing.T) {
	out := os.Getenv(restartJobHelperOut)
	if out == "" {
		t.Skip("the transient unit's main process for TestRestartJobReadsAsRestartAndStopAsFinal")
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM)
	appendRestartJobRead(out, "ready")
	<-sigs
	unit := os.Getenv("AF_RESTARTJOB_HELPER_UNIT")
	read := func(pid int) string {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		if RestartJobPending(ctx, unit, pid) {
			return "restart"
		}
		return "final"
	}
	appendRestartJobRead(out, read(os.Getpid()))
	appendRestartJobRead(out, "notmain-"+read(os.Getppid()))
	os.Exit(0)
}

func appendRestartJobRead(path, line string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(3)
	}
	_, _ = fmt.Fprintln(f, line)
	_ = f.Close()
}

func waitForRestartJobReads(t *testing.T, path string, want []string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var got []string
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(path)
		got = strings.Fields(string(data))
		if strings.Join(got, " ") == strings.Join(want, " ") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the unit's reads were %q, want %q", got, want)
}

// TestRestartJobReadsAsRestartAndStopAsFinal proves the job read against a real
// systemd user manager (#7474): a unit's process, reading its own job while it
// handles the SIGTERM, sees a `systemctl --user restart` as a restart and a
// `systemctl --user stop` as a final stop. And a pid that is not the unit's
// MainPID reads both as final (#7480). Only a real manager can show that the
// restart job is still pending while its stop phase runs, and that MainPID names
// the process it is stopping, on each systemd the supported runners ship.
func TestRestartJobReadsAsRestartAndStopAsFinal(t *testing.T) {
	requireRealUserManager(t)
	if out, err := exec.Command("systemctl", "--version").Output(); err == nil {
		t.Logf("%s", strings.SplitN(string(out), "\n", 2)[0])
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	unit := fmt.Sprintf("af-restartjob-test-%d.service", os.Getpid())
	out := filepath.Join(t.TempDir(), "reads")
	t.Cleanup(func() { stopUnit(unit) })

	if b, err := exec.Command("systemd-run", "--user", "--quiet", "--unit="+unit,
		"--setenv="+restartJobHelperOut+"="+out, "--setenv=AF_RESTARTJOB_HELPER_UNIT="+unit,
		exe, "-test.run=^TestRestartJobHelperProcess$").CombinedOutput(); err != nil {
		t.Fatalf("systemd-run: %v\n%s", err, b)
	}
	waitForRestartJobReads(t, out, []string{"ready"})

	if b, err := exec.Command("systemctl", "--user", "restart", unit).CombinedOutput(); err != nil {
		t.Fatalf("systemctl --user restart: %v\n%s", err, b)
	}
	waitForRestartJobReads(t, out, []string{"ready", "restart", "notmain-final", "ready"})

	if b, err := exec.Command("systemctl", "--user", "stop", unit).CombinedOutput(); err != nil {
		t.Fatalf("systemctl --user stop: %v\n%s", err, b)
	}
	waitForRestartJobReads(t, out, []string{"ready", "restart", "notmain-final", "ready", "final", "notmain-final"})
}
