package session

import (
	"time"
)

// Bounds for retrying a retained record's cleanup. Base doubles per consecutive
// failure and settles at max, mirroring the root-ensure convention (#1122) so af
// has one shape for "a bounded operation that keeps failing" rather than a
// decision per site. Package vars so tests can shorten them.
var (
	cleanupRetryBackoffBase = 10 * time.Second
	cleanupRetryBackoffMax  = 5 * time.Minute
)

// cleanupRetryEscalationThreshold is the consecutive-failure count at which the
// cause stops looking transient and earns one ERROR log. Retrying continues at
// the settled cadence: an outage that ends must still heal on the next attempt,
// which is exactly what a permanent give-up cost the root agents in #1122.
const cleanupRetryEscalationThreshold = 6

// CleanupRetry paces the daemon's retry of a retained kill tombstone.
//
// Before this, a tombstone whose cleanup could not complete was retried on EVERY
// status poll — once a second, forever, two log lines per attempt (#2737). That
// is not resilience: for a cause that cannot heal on its own it is a hot loop
// burning a user's CPU and log volume with nothing to act on.
//
// So two bounds, for the two different kinds of failure:
//
//   - A failure that MIGHT heal (an unreachable host, a busy remote) backs off
//     exponentially to a settled cadence and keeps trying, because an outage that
//     ends must recover without a daemon restart.
//   - A failure that CANNOT heal by retrying — a cleanup handle that is
//     structurally unusable, such as a pre-#2704 SSH tombstone whose host-key
//     posture was never recorded — is RETIRED. Repeating identical inputs cannot
//     produce a different answer, so the record stops being retried and is
//     surfaced once for the operator to act on.
//
// The zero value is ready to use. State is in-memory on purpose: a daemon restart
// re-attempts a retired record once, which is the right behavior when an operator
// may have fixed the cause (added the host key, restored the remote) in between.
type CleanupRetry struct {
	consecutiveFailures int
	nextAttempt         time.Time
	escalated           bool
	retired             bool
	retirementReported  bool
}

// Due reports whether another attempt is allowed now. A retired entry never is.
func (r *CleanupRetry) Due(now time.Time) bool {
	if r.retired {
		return false
	}
	return !now.Before(r.nextAttempt)
}

// Retired reports that this cleanup can never succeed by retrying and has been
// given up on. It stays true until the entry is dropped (daemon restart) or a
// caller records a success.
func (r *CleanupRetry) Retired() bool { return r.retired }

// Failures is the consecutive-failure count, for reporting.
func (r *CleanupRetry) Failures() int { return r.consecutiveFailures }

// RecordFailure books one failed attempt and schedules the next. It reports
// whether this failure earns an operator-visible escalation — true exactly once
// per streak, either when the cause becomes unusable (retire) or when the
// failure count crosses the threshold.
func (r *CleanupRetry) RecordFailure(now time.Time, err error) bool {
	r.consecutiveFailures++
	if CleanupHandleUnusable(err) {
		// Nothing about a later attempt differs, so there is no cadence to settle
		// into — this one is done.
		r.retired = true
		// Report the RETIREMENT even if a backoff escalation already fired: those
		// say opposite things. "retrying every 5m" told the operator af was still
		// working on it; this says it has stopped and needs them.
		if r.retirementReported {
			return false
		}
		r.retirementReported = true
		r.escalated = true
		return true
	}
	r.nextAttempt = now.Add(cleanupRetryBackoff(r.consecutiveFailures))
	if r.consecutiveFailures < cleanupRetryEscalationThreshold || r.escalated {
		return false
	}
	r.escalated = true
	return true
}

// RecordSuccess clears the streak, so a cause that healed leaves no backoff
// behind for the next unrelated failure.
func (r *CleanupRetry) RecordSuccess() { *r = CleanupRetry{} }

// cleanupRetryBackoff doubles the base per consecutive failure and settles at the
// max, so a cause that never heals costs one attempt per max interval rather
// than one per poll.
func cleanupRetryBackoff(consecutiveFailures int) time.Duration {
	backoff := cleanupRetryBackoffMax
	if shift := consecutiveFailures - 1; shift >= 0 && shift < 32 {
		if b := cleanupRetryBackoffBase << shift; b > 0 && b < backoff {
			backoff = b
		}
	}
	return backoff
}

// CleanupRetrySettledInterval is the cadence a never-healing retry settles at,
// for callers that report it.
func CleanupRetrySettledInterval() time.Duration { return cleanupRetryBackoffMax }
