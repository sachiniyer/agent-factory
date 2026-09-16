package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/rpc"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/internal/upgradetxn"
)

// daemonAdmissionRetryWait bounds the complete admission phase, including the
// initial EnsureDaemon gate check. Every retry sleep, dial, and re-ensure
// receives this same absolute deadline; an inner gate/readiness timeout may
// shorten it but may never extend it. One documented exception: a SUCCESSFUL
// ensure that outlived the window (a unit-path reclaim can legitimately —
// RestartSec alone exceeds the budget) renews the follow-up dial by one dial
// timeout via postEnsureDialDeadline, since the deadline bounds the wait FOR
// a daemon, not the RPC that a now-reachable daemon is owed.
// daemonAdmissionRetryPoll is the cadence.
const (
	daemonAdmissionRetryWait = daemonReadyTimeout
	daemonAdmissionRetryPoll = 100 * time.Millisecond
)

// callDaemon retries exactly two kinds of transient failure: lifecycle
// admission refusals, and failed dials during a proven upgrade hand-off. The
// proof is either a quiescing response or a typed live upgrade-gate result.
// Once net/rpc starts a request, every connection loss remains final even with
// that proof: the handler may have committed before its response disappeared.
func callDaemon(method string, req any, resp any) error {
	deadline := time.Now().Add(daemonAdmissionRetryWait)
	var (
		handoffSeen bool
		fallbackErr error
	)
	ensureErr := ensureDaemonWithLauncherUntil(launchDaemonProcessFn, deadline)
	var attempt daemonCallAttempt
	if ensureErr != nil {
		if !isLiveUpgradeGateErr(ensureErr) {
			return ensureErr
		}
		handoffSeen = true
		fallbackErr = ensureErr
		attempt.err = ensureErr
	} else {
		attempt = callDaemonNoEnsureAttemptUntil(method, req, resp, postEnsureDialDeadline(deadline))
	}

	for attempt.err != nil {
		err := attempt.err
		if IsDaemonQuiescingErr(err) {
			handoffSeen = true
			if fallbackErr == nil {
				fallbackErr = err
			}
		}

		admissionErr := IsDaemonAdmissionRetryable(err)
		connectionLoss := isDaemonHandoffConnectionErr(err)
		dialTimeout := !attempt.requestStarted && isDaemonConnectionTimeout(err)
		retryableDial := !attempt.requestStarted && (connectionLoss || dialTimeout)
		liveGateErr := isLiveUpgradeGateErr(err)
		if !admissionErr && !retryableDial && !liveGateErr {
			return err
		}

		if retryableDial {
			// A timeout does not prove that the listener is gone: a healthy
			// daemon's accept backlog can be saturated. Never enter the reclaiming
			// EnsureDaemon path on that observation alone.
			if dialTimeout && !handoffSeen {
				return err
			}
			if admissionDeadlineExpired(deadline) {
				break
			}
			ensureErr := ensureDaemonWithLauncherUntil(launchDaemonProcessFn, deadline)
			switch {
			case ensureErr == nil:
				// A failed dial proves the request was never delivered, so one
				// retry is safe once EnsureDaemon has made a daemon reachable. A
				// non-absence dial failure still needs positive hand-off proof.
				if !handoffSeen && !isDaemonAbsentErr(err) {
					return err
				}
				// The re-ensure can itself outlive the admission window — the
				// unit-path reclaim draws its own budgets. The daemon it just
				// repaired is still owed this request, so dial on the renewed
				// window rather than letting the stale deadline below discard
				// the repair (Codex on #4475).
				attempt = callDaemonNoEnsureAttemptUntil(method, req, resp, postEnsureDialDeadline(deadline))
				continue
			case isLiveUpgradeGateErr(ensureErr):
				handoffSeen = true
				fallbackErr = ensureErr
			case handoffSeen:
				// Keep retrying a hand-off connection loss. Preserve a useful
				// ensure failure, but do not replace the lifecycle fallback with
				// the outer deadline expiring.
				if !errors.Is(ensureErr, context.DeadlineExceeded) {
					fallbackErr = ensureErr
				}
			default:
				// A first post-ensure transport error is not enough by itself:
				// without a live upgrade gate, keep the cold/ordinary failure
				// behavior and return it immediately.
				return err
			}
		}

		if !waitUntilAdmissionDeadline(deadline, daemonAdmissionRetryPoll) {
			break
		}
		attempt = callDaemonNoEnsureAttemptUntil(method, req, resp, deadline)
	}

	if attempt.err != nil && fallbackErr != nil && admissionDeadlineExpired(deadline) {
		return fallbackErr
	}
	return attempt.err
}

// isDaemonHandoffConnectionErr recognizes only definite connection loss. It
// deliberately excludes rpc.ServerError (an application handler error) and
// generic timeouts (a live listener may merely have a saturated backlog).
// callDaemon separately requires a failed dial before replaying any match.
func isDaemonHandoffConnectionErr(err error) bool {
	if err == nil {
		return false
	}
	var serverErr rpc.ServerError
	if errors.As(err, &serverErr) {
		return false
	}
	if isDaemonAbsentErr(err) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EPIPE) {
		return true
	}
	return false
}

func isDaemonConnectionTimeout(err error) bool {
	if err == nil {
		return false
	}
	var serverErr rpc.ServerError
	if errors.As(err, &serverErr) {
		return false
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func isLiveUpgradeGateErr(err error) bool {
	var inProgress *upgradetxn.UpgradeInProgressError
	return errors.As(err, &inProgress)
}

func lockEnsureDaemonUntil(deadline time.Time) bool {
	if deadline.IsZero() {
		ensureDaemonMu.Lock()
		return true
	}
	for {
		if ensureDaemonMu.TryLock() {
			return true
		}
		if !waitUntilAdmissionDeadline(deadline, 10*time.Millisecond) {
			return false
		}
	}
}

func admissionBoundedDeadline(outer time.Time, allowance time.Duration) time.Time {
	inner := time.Now().Add(allowance)
	if !outer.IsZero() && outer.Before(inner) {
		return outer
	}
	return inner
}

func admissionDeadlineExpired(deadline time.Time) bool {
	return !deadline.IsZero() && !time.Now().Before(deadline)
}

// postEnsureDialDeadline renews an admission deadline that expired while a
// SUCCESSFUL ensure was still doing legitimate work. The unit-path reclaim
// draws its own bounded budgets (the readiness wait can spend the whole
// admission window before is-active/restart/ready get their turn), so a
// reclaim that repaired the daemon returns after the caller's deadline — and
// handing that stale deadline to the dial would fail the request the repair
// just made deliverable. The renewal is one dial timeout, not a fresh
// admission window: retry sleeps and re-ensures still check the original
// deadline, so only this first post-ensure attempt is unblocked (Codex on
// #4475).
func postEnsureDialDeadline(deadline time.Time) time.Time {
	if deadline.IsZero() || time.Now().Before(deadline) {
		return deadline
	}
	return time.Now().Add(daemonDialTimeout)
}

func daemonAdmissionDeadlineError() error {
	return fmt.Errorf("daemon admission retry deadline elapsed: %w", context.DeadlineExceeded)
}

func waitUntilAdmissionDeadline(deadline time.Time, delay time.Duration) bool {
	if deadline.IsZero() {
		time.Sleep(delay)
		return true
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}
	if delay > remaining {
		delay = remaining
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	<-timer.C
	return time.Now().Before(deadline)
}
