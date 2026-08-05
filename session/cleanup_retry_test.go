package session

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #2737: a retained tombstone was retried on every status poll — once a second,
// forever. The pacing is the fix, so pin the cadence rather than just the state.
func TestCleanupRetryPacesRetriesInsteadOfEveryPoll(t *testing.T) {
	var retry CleanupRetry
	now := time.Unix(1_800_000_000, 0)

	require.True(t, retry.Due(now), "the first attempt is always allowed")

	// One failure must stop the next poll a second later from attempting again.
	retry.RecordFailure(now, errors.New("host unreachable"))
	assert.False(t, retry.Due(now.Add(time.Second)), "a poll one second later must not retry")
	assert.False(t, retry.Due(now.Add(cleanupRetryBackoffBase-time.Millisecond)))
	assert.True(t, retry.Due(now.Add(cleanupRetryBackoffBase)), "the backoff must expire")

	// Backoff doubles and then settles, so a cause that never heals costs one
	// attempt per settled interval rather than one per poll.
	var settled CleanupRetry
	prev := time.Duration(0)
	for i := 1; i <= 12; i++ {
		settled.RecordFailure(now, errors.New("still unreachable"))
		got := cleanupRetryBackoff(i)
		assert.LessOrEqual(t, got, cleanupRetryBackoffMax, "backoff must never exceed the settled cadence")
		assert.GreaterOrEqual(t, got, prev, "backoff must not shrink while failures continue")
		prev = got
	}
	assert.Equal(t, cleanupRetryBackoffMax, cleanupRetryBackoff(12), "it must settle at the max, not grow without bound")
}

// The escalation fires once per streak, not on every failure — the old loop
// logged two lines every second.
func TestCleanupRetryEscalatesOnce(t *testing.T) {
	var retry CleanupRetry
	now := time.Unix(1_800_000_000, 0)

	escalations := 0
	for i := 0; i < cleanupRetryEscalationThreshold*3; i++ {
		if retry.RecordFailure(now, errors.New("still unreachable")) {
			escalations++
		}
		now = now.Add(cleanupRetryBackoffMax)
	}
	assert.Equal(t, 1, escalations, "a persistent cause earns exactly one escalation")
	assert.False(t, retry.Retired(), "a cause that might heal must keep retrying (#1122)")
}

// A handle that cannot be fixed by retrying is retired, not backed off: repeating
// identical inputs cannot produce a different answer.
func TestCleanupRetryRetiresAnUnusableHandle(t *testing.T) {
	var retry CleanupRetry
	now := time.Unix(1_800_000_000, 0)

	unusable := fmt.Errorf("%w: %w", ErrCleanupHandleUnusable,
		fmt.Errorf("%w: reconnecting failed", ErrWorkspaceStateUnknown))

	assert.True(t, retry.RecordFailure(now, unusable), "retiring is worth one report")
	assert.True(t, retry.Retired())
	assert.False(t, retry.Due(now), "a retired handle is never due again")
	assert.False(t, retry.Due(now.Add(24*time.Hour)), "not even much later — no cadence can help it")
	assert.False(t, retry.RecordFailure(now, unusable), "and it is not reported twice")

	// Retiring must not weaken the retention rule: the workspace state is still
	// unknown, so the record is still kept.
	assert.True(t, TeardownStateUnknown(unusable),
		"an unusable handle still reports unknown workspace state, so the record is retained")
}

// A cause that heals leaves no backoff behind for the next unrelated failure.
func TestCleanupRetrySuccessClearsTheStreak(t *testing.T) {
	var retry CleanupRetry
	now := time.Unix(1_800_000_000, 0)
	for i := 0; i < cleanupRetryEscalationThreshold+2; i++ {
		retry.RecordFailure(now, errors.New("transient"))
	}
	require.False(t, retry.Due(now))

	retry.RecordSuccess()
	assert.True(t, retry.Due(now), "a healed cause starts clean")
	assert.Zero(t, retry.Failures())
	assert.False(t, retry.Retired())
}

// CleanupHandleUnusable must not fire on the ordinary retained-record error, or
// every transient failure would be retired.
func TestCleanupHandleUnusableIsNarrow(t *testing.T) {
	assert.False(t, CleanupHandleUnusable(nil))
	assert.False(t, CleanupHandleUnusable(errors.New("host unreachable")))
	assert.False(t, CleanupHandleUnusable(fmt.Errorf("%w: dir removal failed", ErrWorkspaceStateUnknown)),
		"an unknown workspace state is retried, not retired")
	assert.True(t, CleanupHandleUnusable(fmt.Errorf("%w: no posture recorded", ErrCleanupHandleUnusable)))
}

// Review finding on #2855: a cleanup that backs off, escalates, and only THEN
// turns out to be unusable must still report the retirement. "retrying every 5m"
// and "af has stopped" say opposite things, and the operator acted on the first.
func TestCleanupRetryReportsRetirementAfterAnEscalation(t *testing.T) {
	var retry CleanupRetry
	now := time.Unix(1_800_000_000, 0)

	escalations := 0
	for i := 0; i < cleanupRetryEscalationThreshold; i++ {
		if retry.RecordFailure(now, errors.New("host unreachable")) {
			escalations++
		}
		now = now.Add(cleanupRetryBackoffMax)
	}
	require.Equal(t, 1, escalations, "the backoff escalation fires once")
	require.False(t, retry.Retired())

	unusable := fmt.Errorf("%w: posture never recorded", ErrCleanupHandleUnusable)
	assert.True(t, retry.RecordFailure(now, unusable),
		"discovering the cause is permanent must be reported even after a backoff escalation")
	assert.True(t, retry.Retired())
	assert.False(t, retry.RecordFailure(now, unusable), "but only once")
}
