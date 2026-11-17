package tmux

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// handleDaemonFixtureExec runs the daemon roles whose argv is a pure
// `af --daemon` claim, before flag parsing ever sees argv. Since #7494 item 3
// the classifier reads a daemon claim only where af's own root parser would —
// a flags-only zone — so a re-exec'd helper can no longer select its test with
// `-test.run` in the same argv: `-t` is not a root shorthand and the child
// would stop claiming the daemon. The env marker picks the role instead; the
// test binary's kernel-attested build is what VERIFIES the claim (#7491), so
// argv stays `["af", "--daemon"]` and nothing else. Each role ends in
// os.Exit; falling through means this process is not a daemon fixture.
//
// These helpers take no *testing.T — TestMain runs them before testing
// exists. A fixture that cannot perform its role exits nonzero and the
// driver's Eventually on its pidfile times out, which is the intended
// failure mode for a malformed spec.
func handleDaemonFixtureExec() {
	switch {
	case os.Getenv(afDaemonSleepEnv) == "1":
		// A verified daemon that only parks: the positive placement
		// fixtures need a live af-attested process under daemon argv.
		time.Sleep(5 * time.Minute)
		os.Exit(0)
	case os.Getenv(decisionDaemonEnv) == "1":
		// The decision-table daemon: publish a marked child's pid, then
		// park. The child gets a clean env of just the fixture's markers.
		spec := strings.Split(os.Getenv(decisionDaemonSpecEnv), "\t")
		if len(spec) != 4 {
			os.Exit(2)
		}
		child := exec.Command("sleep", "300")
		child.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			EnvMarkerSession + "=" + spec[0],
			EnvMarkerHome + "=" + spec[1],
			EnvMarkerGeneration + "=" + spec[2],
		}
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if err := os.WriteFile(spec[3],
			[]byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			os.Exit(2)
		}
		_, _ = child.Process.Wait()
		time.Sleep(5 * time.Minute)
		os.Exit(0)
	case os.Getenv(reapChainRoleEnv) == "afdaemon":
		// The daemonchain fixture's afdaemon role: spawn the child the
		// subtree exclusion covers, publish its pid, park.
		child := exec.Command("sleep", "300")
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if err := os.WriteFile(os.Getenv(subtreeAfPidFileEnv),
			[]byte("afchild="+strconv.Itoa(child.Process.Pid)+"\n"), 0o600); err != nil {
			os.Exit(2)
		}
		_, _ = child.Process.Wait()
		time.Sleep(5 * time.Minute)
		os.Exit(0)
	}
}
