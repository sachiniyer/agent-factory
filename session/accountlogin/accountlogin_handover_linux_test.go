//go:build linux

package accountlogin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/shellquote"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// A login flow can end at any moment relative to the handover, and what
// Supervisor.Start reports depends only on where that exit lands among
// tmux.Start's liveness observations. These tests used to launch a pane that
// exited at once and assert on whichever side the exit happened to land: on an
// idle machine the pane was gone before the attach probe, on a loaded one it was
// not, and the same code was reported both ways (#4406, #4217 census).
//
// Every ordering is now driven on purpose. The fixture is a real login pane
// launched through the production chain, but it holds until the test releases
// it, and the release happens at exactly one tmux.StartObservation — or after
// Start returns. The three placements are every side of Start's observations
// the exit can land on, so together they are exhaustive.

// loginExitOrdering is where, relative to Start's observations, the flow ends.
type loginExitOrdering struct {
	name string
	// at is the observation the flow ends before; zero means after Start
	// returned, when the pane has already been handed over.
	at tmux.StartObservation
	// launchError is the tmux.Start failure this placement produces, which a
	// failed login quotes: it proves the case took its own path through Start.
	launchError string
}

var endedBeforeHandover = []loginExitOrdering{
	{name: "gone before the existence poll", at: tmux.StartBeforeExistencePoll, launchError: "timed out waiting for tmux session"},
	{name: "gone before the attach probe", at: tmux.StartBeforeAttachProbe, launchError: "vanished before attach"},
}

// gatedLoginFlow is a codex login fixture whose exit the test controls: the
// pane announces it is running, waits for the release, then does the flow's
// work and exits.
type gatedLoginFlow struct {
	running string
	release string
	pane    *tmux.TmuxSession
}

// writeGatedLoginFlow installs the fixture as `codex` on PATH. work is the shell
// the flow runs once released; CODEX_HOME is the account root the login
// boundary injects.
func writeGatedLoginFlow(t *testing.T, work string) gatedLoginFlow {
	t.Helper()
	signals := t.TempDir()
	flow := gatedLoginFlow{
		running: filepath.Join(signals, "running"),
		release: filepath.Join(signals, "release"),
		pane:    tmux.NewTmuxSession(agentaccount.LoginSessionName("codex", "work"), "codex"),
	}
	script := "#!/bin/sh\n" +
		": > " + shellquote.Quote(flow.running) + "\n" +
		"while [ ! -e " + shellquote.Quote(flow.release) + " ]; do sleep 0.02; done\n" +
		work + "\n"
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "codex"), []byte(script), 0o700); err != nil {
		t.Fatalf("write codex login fixture: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return flow
}

// end lets the flow finish and returns once tmux positively reports its pane
// gone. Both waits are for events the release makes certain; the deadline only
// bounds a broken fixture.
func (f gatedLoginFlow) end(t *testing.T) {
	t.Helper()
	waitFor(t, "the login pane to start running", func() bool {
		_, err := os.Stat(f.running)
		return err == nil
	})
	if err := os.WriteFile(f.release, nil, 0o600); err != nil {
		t.Fatalf("release the login flow: %v", err)
	}
	waitFor(t, "the login pane to exit", func() bool {
		exists, known := f.pane.ProbeSession()
		return known && !exists
	})
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(reportDeadline)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", reportDeadline, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startLogin runs Supervisor.Start for codex/work with the flow ending at
// ordering. It fails the test if Start never reached that observation, so an
// ordering cannot silently degrade into a different one.
func startLogin(t *testing.T, flow gatedLoginFlow, ordering loginExitOrdering) (*Supervisor, string, Session, error) {
	t.Helper()
	isolateLoginTmux(t)
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	supervisor := New()
	t.Cleanup(supervisor.Stop)

	var reached []tmux.StartObservation
	restore := tmux.SetStartObservationHookForTest(func(name string, at tmux.StartObservation) {
		if name != flow.pane.SanitizedName() {
			return
		}
		reached = append(reached, at)
		if at == ordering.at {
			flow.end(t)
		}
	})
	defer restore()
	login, err := supervisor.Start(context.Background(), Request{Home: home, Agent: "codex", Name: "work"})
	if ordering.at == 0 {
		return supervisor, home, login, err
	}
	for _, at := range reached {
		if at == ordering.at {
			return supervisor, home, login, err
		}
	}
	t.Fatalf("Start never reached observation %d (reached %v), so the flow did not end where this case says", ordering.at, reached)
	return nil, "", Session{}, nil
}

// credentialFlowWork is the agent's own flow against an account it can complete
// without the human: it leaves the credential and exits.
const credentialFlowWork = `printf '{}' > "$CODEX_HOME/auth.json"`

// TestLoginReportsAFlowThatEndedBeforeTheHandover covers the login that
// completes without ever needing the terminal — `codex login` against a
// credential that is already there, or a flow that answers itself. tmux.Start
// reports that as a pane that timed out or vanished, worded for a broken
// install; af has to tell the two apart by the ACCOUNT, not by the launch error.
//
// Intent: whenever the pane is gone before Start could hand it over, a flow that
// left a credential is a finished login with nothing to attach to — never a
// failure.
func TestLoginReportsAFlowThatEndedBeforeTheHandover(t *testing.T) {
	for _, ordering := range endedBeforeHandover {
		t.Run(ordering.name, func(t *testing.T) {
			flow := writeGatedLoginFlow(t, credentialFlowWork)
			_, _, login, err := startLogin(t, flow, ordering)
			if err != nil {
				t.Fatalf("a completed login was reported as a failure: %v", err)
			}
			if !login.Finished {
				t.Fatal("a flow that ended before the handover did not report Finished")
			}
			if !login.LoggedIn {
				t.Fatal("a flow that wrote the credential after the launch-time snapshot did not report the account logged in")
			}
			if login.TmuxName != "" {
				t.Fatalf("a finished flow named %q to attach to", login.TmuxName)
			}
			if !strings.Contains(strings.Join(login.Notices, "\n"), "ended before af could hand over the terminal") {
				t.Fatalf("the finished login does not say why there is nothing to attach to: %q", login.Notices)
			}
		})
	}
}

// TestLoginReportsANoOpAsFailure is #3384's verification requirement at its
// sharpest.
//
// Intent: whenever the pane is gone before Start could hand it over, a flow that
// left the account empty is a failure that says the account is not logged in —
// the shape of an OAuth flow abandoned at the browser step, which several of
// these CLIs report as success. The alternative is a registered account that
// looks fine and fails much later, at session start, naming none of this.
func TestLoginReportsANoOpAsFailure(t *testing.T) {
	for _, ordering := range endedBeforeHandover {
		t.Run(ordering.name, func(t *testing.T) {
			flow := writeGatedLoginFlow(t, "exit 0")
			_, _, login, err := startLogin(t, flow, ordering)
			if err == nil {
				t.Fatalf("a login that left the account empty was reported as success: %+v", login)
			}
			for _, want := range []string{"without leaving a credential", "not logged in"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("failure %q does not say the account is still not logged in (missing %q)", err, want)
				}
			}
			if !strings.Contains(err.Error(), ordering.launchError) {
				t.Fatalf("failure %q does not quote the launch error this ordering produces (%q)", err, ordering.launchError)
			}
			if errors.Is(err, tmux.ErrSessionNameTaken) || strings.Contains(err.Error(), "already exist") {
				t.Fatalf("a flow that ran and ended was reported as a name collision: %v", err)
			}
		})
	}
}

// TestLoginHandsOverAFlowThatEndsAfterStart is the third placement: the pane is
// alive at every observation, so Start hands it over whatever the flow later
// does. Neither outcome is knowable at that point, and Start must not guess one.
//
// Intent: a live pane is always a handover — a name to attach to, not Finished,
// and LoggedIn as the launch-time snapshot saw it — and once the flow ends the
// supervisor stops reporting it live while the account shows what it left.
func TestLoginHandsOverAFlowThatEndsAfterStart(t *testing.T) {
	for _, tc := range []struct {
		name     string
		work     string
		loggedIn bool
	}{
		{name: "flow that leaves a credential", work: credentialFlowWork, loggedIn: true},
		{name: "flow that leaves nothing", work: "exit 0", loggedIn: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flow := writeGatedLoginFlow(t, tc.work)
			supervisor, home, login, err := startLogin(t, flow, loginExitOrdering{})
			if err != nil {
				t.Fatalf("a flow that was still running at the handover was reported as a failure: %v", err)
			}
			if login.Finished {
				t.Fatal("a flow that was still running at the handover was reported Finished")
			}
			if login.LoggedIn {
				t.Fatal("the handover reported a credential the flow had not written yet")
			}
			if login.TmuxName != flow.pane.SanitizedName() {
				t.Fatalf("the handover named %q, not the login pane %q", login.TmuxName, flow.pane.SanitizedName())
			}
			if !supervisor.Live("codex", "work") {
				t.Fatal("a handed-over flow is not reported live")
			}

			flow.end(t)
			if supervisor.Live("codex", "work") {
				t.Fatal("a flow whose pane exited is still reported live")
			}
			loggedIn, err := agentaccount.LoggedIn(home, "codex", "work")
			if err != nil {
				t.Fatalf("probe logged-in state: %v", err)
			}
			if loggedIn != tc.loggedIn {
				t.Fatalf("after the flow ended the account reads logged-in=%v, want %v", loggedIn, tc.loggedIn)
			}
		})
	}
}
