package daemon

import (
	"fmt"
	"os"
	"time"
)

// The control server's lifecycle RPCs — liveness, shutdown, and upgrade
// probation release — split from control_server.go by topic. They are the
// daemon side of the #5007 shutdown state machine: Ping reports the phase and
// the responder's PID, and a Shutdown ack moves the daemon to quiescing and
// names the acker.

func (s *controlServer) Ping(_ PingRequest, resp *PingResponse) error {
	resp.OK = true
	resp.AccountHandoff = true
	resp.Version = Version()
	resp.PID = os.Getpid()
	if s.manager != nil && s.manager.lifecycle != nil {
		state := s.manager.lifecycle.snapshot()
		resp.BootID = state.bootID
		resp.TransactionID = state.transactionID
		resp.Phase = state.phase
		resp.Listeners = state.listeners
	}
	if s.manager != nil && s.manager.cfg != nil {
		resp.BootConfig = &DaemonBootConfig{
			ListenAddr:           s.manager.cfg.ListenAddr,
			RequireToken:         s.manager.cfg.RequireToken,
			RequireLoopbackToken: s.manager.cfg.RequireLoopbackToken,
		}
	}
	return nil
}

// ReleaseUpgradeProbation lifts this daemon's upgrade probation once its
// previous-binary supervisor has validated it (#2212 R2). A release for a
// non-matching or non-probationary transaction is refused. It is deliberately NOT
// gated on requireMutationAdmission — probation is exactly what it lifts, so
// gating it on admission would deadlock. (Authority rests on the control socket
// being owner-only and already privileged, NOT on the transaction id being
// secret: Ping reports that id, `af daemon status` prints it, and it rides argv
// visible in /proc — it is an identity check, never a capability.)
func (s *controlServer) ReleaseUpgradeProbation(req ReleaseUpgradeProbationRequest, resp *ReleaseUpgradeProbationResponse) error {
	if s.manager == nil || s.manager.lifecycle == nil {
		return fmt.Errorf("daemon has no lifecycle to release from upgrade probation")
	}
	if err := s.manager.lifecycle.releaseUpgradeProbation(req.TransactionID); err != nil {
		return err
	}
	resp.OK = true
	return nil
}

// Shutdown acknowledges a request to terminate the daemon, then asynchronously
// signals the main loop to tear down after a short grace period. The grace
// lets the RPC response flush back to the caller before the listener closes.
//
// The ack carries this process's PID and moves the lifecycle to quiescing at
// once (#5007): teardown can keep the socket answering for seconds, and every
// Ping in that window must read "leaving", never "ready" — including the
// warm-up exits that never reach drainDaemon's own markQuiescing — so a
// respawn's liveness check cannot mistake this daemon for a live one.
func (s *controlServer) Shutdown(_ ShutdownRequest, resp *ShutdownResponse) error {
	resp.OK = true
	resp.PID = os.Getpid()
	if s.manager != nil && s.manager.lifecycle != nil {
		s.manager.lifecycle.markQuiescing()
	}
	if s.shutdownCh == nil {
		return nil
	}
	s.shutdownOnce.Do(func() {
		go func() {
			time.Sleep(shutdownAckGrace)
			close(s.shutdownCh)
		}()
	})
	return nil
}
