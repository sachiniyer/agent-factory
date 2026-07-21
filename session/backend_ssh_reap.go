package session

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/log"

	"golang.org/x/crypto/ssh"
)

// reap tears down the remote sandbox idempotently: close the tunnel listener (stop
// accepting), kill the remote agent-server PID, remove the session dir, and close
// the ssh connection. It runs on the session's Kill (after the remote workspace is
// torn down over REST), on a provisioning failure, and on a bad-endpoint
// NewInstance failure — so a remote workspace/tunnel is never leaked.
//
// A completed directory-removal result (success or an exit status returned by the
// remote rm) latches to collapse repeated Kill calls and Kill-vs-provision-failure
// races. A timeout, lost transport, or answered identity-kill failure deliberately
// does not latch: cleanup may have stopped halfway through, so the row must remain
// and the next poll must make a real attempt. Because every reap closes the old SSH
// client to drain tunnel forwards, that retry first reconnects; otherwise a
// mutex/latch conversion alone would only retry on a closed client.
func (p *sshProvisioner) reap() error {
	p.reapMu.Lock()
	defer p.reapMu.Unlock()
	if p.reaped {
		return p.reapErr
	}
	// One ownership boundary for every non-latched attempt, including reconnect
	// failures. dialForReap may acquire both an SSH client and an ssh-agent socket;
	// neither may survive the attempt that acquired it.
	defer p.finishReapTransport()

	if p.tunnelLn != nil {
		_ = p.tunnelLn.Close()
		// Wait until Accept has returned before any tunnelWG.Wait below. This
		// closes the WaitGroup's Add-vs-Wait race: once the accept loop is gone,
		// every forward that can exist has already incremented tunnelWG.
		p.tunnelAcceptWG.Wait()
	}
	// authMethods can open the agent connection before dial succeeds. Release it
	// independently of the SSH client and nil it so retries do not re-close a
	// stale descriptor (#1684).
	if p.agentConn != nil {
		_ = p.agentConn.Close()
		p.agentConn = nil
	}

	// No remote directory means provisioning never created a workspace. Closing
	// any connection is sufficient, and that completed outcome can latch.
	if p.sessionDir == "" {
		p.reaped = true
		p.reapErr = nil
		return nil
	}

	// A previous timed-out attempt closed its client so tunnel forwards could
	// drain. Reconnect before retrying the remote rm; failure to connect tells us
	// nothing about whether the directory still exists, so retain and retry.
	if p.client == nil {
		if err := p.dialForReap(); err != nil {
			reapErr := fmt.Errorf("%w: backend=ssh: reconnecting to %s to remove remote session dir %q failed: %w",
				ErrWorkspaceStateUnknown, p.cfg.Host, p.sessionDir, err)
			log.WarningLog.Printf("%v", reapErr)
			return reapErr
		}
	}

	if remotePID := p.remotePID; remotePID != "" {
		if !positivePID(remotePID) {
			reapErr := fmt.Errorf("%w: backend=ssh: refusing to signal invalid remote PID %q on %s",
				ErrWorkspaceStateUnknown, remotePID, p.cfg.Host)
			log.WarningLog.Printf("%v", reapErr)
			return reapErr
		}
		// The command verifies that this numeric PID still belongs to the exact
		// per-session af binary before signalling it. That turns a later retry into
		// a safe identity check rather than another blind numeric kill: a recycled
		// PID cannot match the unique session-dir path in the original argv.
		accepted, killErr := p.runReapKill(sshShortStepTimeout, p.remotePIDKillScript(remotePID))
		if !accepted {
			// Opening a channel is not delivery. Keep the PID until the server accepts
			// the exec request, and abort before rm so the retained-row retry still has
			// the process handle it needs.
			if killErr == nil {
				killErr = errors.New("kill exec was not accepted")
			}
			reapErr := fmt.Errorf("%w: backend=ssh: remote PID %s kill exec on %s was not accepted: %w",
				ErrWorkspaceStateUnknown, remotePID, p.cfg.Host, killErr)
			log.WarningLog.Printf("%v", reapErr)
			return reapErr
		}
		if killErr != nil && !sshReapOutcomeUnknown(killErr) {
			// The server answered, and the identity-kill script's only non-zero exits
			// mean argv could not be verified or a signal failed. The PID is therefore
			// still a safe, necessary retry handle; do not remove its directory.
			reapErr := fmt.Errorf("%w: backend=ssh: remote PID %s identity kill on %s failed: %w",
				ErrWorkspaceStateUnknown, remotePID, p.cfg.Host, killErr)
			log.WarningLog.Printf("%v", reapErr)
			return reapErr
		}
		// Start returned success: the server accepted the exec, so the live PID is
		// spent exactly here if the command completed or its reply was lost. An
		// answered script failure returned above without spending it. A daemon restart
		// can replay the pre-kill tombstone, but that copy is paired with the immutable
		// session path and rechecks argv before either signal.
		p.remotePID = ""
		if killErr != nil {
			log.WarningLog.Printf("ssh runtime: remote PID %s kill exec was accepted but did not report completion: %v", remotePID, killErr)
		}
	}
	out, err := p.runReapCombined(sshReapTimeout, "rm -rf "+shellQuote(p.sessionDir))
	if err == nil {
		p.reaped = true
		p.reapErr = nil
		log.InfoLog.Printf("ssh runtime: reaped session %q on %s (remote dir %s)", p.spec.Title, p.cfg.Host, p.sessionDir)
		return nil
	}

	reapErr := fmt.Errorf("backend=ssh: removing remote session dir %q failed: %s: %w", p.sessionDir, strings.TrimSpace(string(out)), err)
	if sshReapOutcomeUnknown(err) {
		// Unknown-state and deliberately NOT latched: retain the record and let
		// finishUserKill call this closure again on the next poll.
		reapErr = fmt.Errorf("%w: %w", ErrWorkspaceStateUnknown, reapErr)
		log.WarningLog.Printf("%v", reapErr)
		return reapErr
	}

	// The remote command answered with an exit status. Preserve the established
	// teardown taxonomy: this is a completed, known-state outcome, so latch it.
	p.reaped = true
	p.reapErr = reapErr
	log.WarningLog.Printf("%v", reapErr)
	return reapErr
}

// sshReapOutcomeUnknown keeps the destructive decision at the SSH command
// boundary. CombinedOutput returns *ssh.ExitError only after the remote reports a
// non-zero exit status; every other error (deadline, EOF, channel loss, failure to
// open a session) lacks proof that rm completed and must retain the record.
func sshReapOutcomeUnknown(err error) bool {
	var exitErr *ssh.ExitError
	return !errors.As(err, &exitErr)
}

func (p *sshProvisioner) runReapCombined(timeout time.Duration, script string) ([]byte, error) {
	if p.reapRunCombined != nil {
		return p.reapRunCombined(timeout, script)
	}
	return p.runCombined(timeout, script)
}

// remotePIDKillScript makes a numeric-PID retry safe. Linux reads argv[0]
// directly from procfs (including BusyBox/Alpine hosts whose ps rejects procps
// flags); macOS and other hosts fall back to ps's command-only field. The unique
// per-session af path remains argv[0] for the agent-server's lifetime. A recycled
// PID whose argv[0] is not exactly that path is treated as already gone, never
// signalled; mentioning the path in an argument proves nothing.
// Re-check before SIGKILL as well so recycling during the grace sleep cannot
// redirect the second signal.
func (p *sshProvisioner) remotePIDKillScript(remotePID string) string {
	return fmt.Sprintf(
		`pid=%s; expected=%s; matches_session() { if [ -r "/proc/$pid/cmdline" ]; then actual=$(tr '\000' '\n' < "/proc/$pid/cmdline" | sed -n '1p') || return 2; [ "$actual" = "$expected" ]; return; fi; actual=$(ps -ww -p "$pid" -o comm= 2>/dev/null) || return 2; [ "$actual" = "$expected" ]; }; if ! kill -0 "$pid" 2>/dev/null; then exit 0; fi; matches_session; matched=$?; if [ "$matched" -eq 1 ]; then exit 0; elif [ "$matched" -ne 0 ]; then exit 75; fi; kill "$pid" 2>/dev/null || exit 76; sleep 0.3; if ! kill -0 "$pid" 2>/dev/null; then exit 0; fi; matches_session; matched=$?; if [ "$matched" -eq 1 ]; then exit 0; elif [ "$matched" -ne 0 ]; then exit 75; fi; kill -9 "$pid" 2>/dev/null || exit 77`,
		shellQuote(remotePID), shellQuote(p.afPath()))
}

// runReapKill separates exec acceptance from command completion. NewSession only
// opens an SSH channel; Session.Start is the protocol boundary where the server
// accepts the exec request. A Start failure retains the PID; once Start succeeds,
// transport loss spends it because delivery is uncertain, while an answered
// non-zero exit lets reap safely retain it for another identity-checked attempt.
func (p *sshProvisioner) runReapKill(timeout time.Duration, script string) (bool, error) {
	if p.reapRunKill != nil {
		return p.reapRunKill(timeout, script)
	}
	sess, err := p.openReapSession()
	if err != nil {
		return false, fmt.Errorf("opening ssh session failed: %w", err)
	}
	defer func() { _ = sess.Close() }()
	return runAcceptedSSHCommand(timeout, sess, "sh -c "+shellQuote(script))
}

func (p *sshProvisioner) openReapSession() (sshCommandSession, error) {
	if p.reapOpenSession != nil {
		return p.reapOpenSession()
	}
	return p.client.NewSession()
}

func (p *sshProvisioner) dialForReap() error {
	if p.reapDial != nil {
		return p.reapDial()
	}
	return p.dial()
}

// finishReapTransport closes every command/tunnel/auth transport acquired by an
// attempt. Keeping a timed-out client around would strand active forwards;
// leaving the ssh-agent connection from a re-dial would leak its socket/readLoop
// once a successful attempt latched and no later reap reached the next cleanup.
func (p *sshProvisioner) finishReapTransport() {
	client := p.client
	if client != nil {
		if p.reapCloseClient != nil {
			p.reapCloseClient()
		} else {
			_ = client.Close()
		}
	}
	if p.tunnelLn != nil {
		// Let in-flight forwards drain against the now-closed ssh connection.
		p.tunnelWG.Wait()
		p.tunnelLn = nil
	}
	// Do not nil the pointer before forwards drain: forward reads p.client when
	// it starts, and a just-accepted connection must see a closed client rather
	// than race into a nil dereference.
	if p.client == client {
		p.client = nil
	}
	if p.agentConn != nil {
		_ = p.agentConn.Close()
		p.agentConn = nil
	}
}
