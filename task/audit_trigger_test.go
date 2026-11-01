package task

// The `triggered` audit entries (#6318): the store-side RecordTaskTrigger
// write, and the bound rule that keeps a stream of manual fires from pushing
// the task's configuration history out of the trail. Split out of
// audit_test.go — the trail's base fixtures (seedAuditTask, auditOf) live
// there — because that file reached the test-file length limit.

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAudit_TriggerRecordsTheSurface: a manual trigger leaves a `triggered`
// entry naming the surface that asked for it — the record that makes "who ran
// this off-schedule?" answerable from the trail.
func TestAudit_TriggerRecordsTheSurface(t *testing.T) {
	id := seedAuditTask(t, ActorCLI)

	require.NoError(t, RecordTaskTrigger(id, ActorTUI))

	trail := auditOf(t, id)
	require.Len(t, trail, 2)
	assert.Equal(t, AuditTriggered, trail[1].Action)
	assert.Equal(t, ActorTUI, trail[1].Actor)
	assert.Empty(t, trail[1].Fields, "a fire moves no configuration field")
	assert.False(t, trail[1].At.IsZero())
}

// TestAudit_TriggerActorIsParsedLikeEveryEntry: an undeclared or unrecognized
// surface records as the explicit "unknown" rather than blank — the same rule
// the create/update entries already follow.
func TestAudit_TriggerActorIsParsedLikeEveryEntry(t *testing.T) {
	id := seedAuditTask(t, ActorCLI)

	require.NoError(t, RecordTaskTrigger(id, ""))

	trail := auditOf(t, id)
	require.Len(t, trail, 2)
	assert.Equal(t, ActorUnknown, trail[1].Actor)
}

// TestAudit_TriggersNeverEvictCreated is the bound rule #6318 requires:
// `triggered` entries are routine fires, not configuration history, so any
// number of them leaves the create — and every config entry — in place.
func TestAudit_TriggersNeverEvictCreated(t *testing.T) {
	id := seedAuditTask(t, ActorCLI)

	for i := 0; i < 50; i++ {
		require.NoError(t, RecordTaskTrigger(id, ActorCLI))
	}

	trail := auditOf(t, id)
	require.Len(t, trail, AuditLimit)
	assert.Equal(t, AuditCreated, trail[0].Action,
		"the create must survive any number of manual triggers")
	for _, e := range trail[1:] {
		assert.Equal(t, AuditTriggered, e.Action)
	}
}

// TestAudit_TriggersNeverEvictConfigHistory is the same rule through the whole
// trail, not just its first entry: interleaved edits and fires drop ONLY the
// oldest fires, so a trigger-happy user cannot push a disable or a retime out
// of the bounded window — the entries the trail exists to keep.
func TestAudit_TriggersNeverEvictConfigHistory(t *testing.T) {
	id := seedAuditTask(t, ActorCLI)

	// 12 real edits (each name differs, so every one records) interleaved with
	// 12 fires: 25 entries over the bound, and the overflow is all fires.
	for i := 0; i < 12; i++ {
		require.NoError(t, RecordTaskTrigger(id, ActorCLI))
		name := fmt.Sprintf("renamed-%d", i)
		_, err := UpdateTaskChecked(id, TaskUpdate{Name: &name}, ProjectExpectation{}, ActorCLI, nil)
		require.NoError(t, err)
	}

	trail := auditOf(t, id)
	require.Len(t, trail, AuditLimit)
	config, fired := 0, 0
	for _, e := range trail {
		if e.Action == AuditTriggered {
			fired++
		} else {
			config++
		}
	}
	assert.Equal(t, 13, config, "the create plus all 12 edits survive")
	assert.Equal(t, AuditLimit-13, fired, "only the newest fires were kept")
	assert.Equal(t, AuditCreated, trail[0].Action)
}

// TestAudit_MixedTrailKeepsBothKinds: triggers interleaved with edits share
// the bound: the newest entries of each kind survive together, because the
// overflow is absorbed by the oldest fires first.
func TestAudit_MixedTrailKeepsBothKinds(t *testing.T) {
	id := seedAuditTask(t, ActorCLI)

	name := "late rename"
	// 18 fires + create + 1 edit = 20 entries, at the bound exactly.
	for i := 0; i < AuditLimit-2; i++ {
		require.NoError(t, RecordTaskTrigger(id, ActorCLI))
	}
	_, err := UpdateTaskChecked(id, TaskUpdate{Name: &name}, ProjectExpectation{}, ActorTUI, nil)
	require.NoError(t, err)
	require.Len(t, auditOf(t, id), AuditLimit, "precondition: the trail is exactly full")

	// One more fire overflows it — and evicts the OLDEST fire, not the edit.
	require.NoError(t, RecordTaskTrigger(id, ActorAPI))
	trail := auditOf(t, id)
	require.Len(t, trail, AuditLimit)
	assert.Equal(t, AuditCreated, trail[0].Action)
	assert.Equal(t, AuditUpdated, trail[len(trail)-2].Action)
	assert.Equal(t, AuditTriggered, trail[len(trail)-1].Action)
	assert.Equal(t, ActorAPI, trail[len(trail)-1].Actor)
}

// TestAudit_RecordTaskTriggerRefusesUnknownTask: the entry rides the row it
// describes, so firing a task that does not exist writes nothing.
func TestAudit_RecordTaskTriggerRefusesUnknownTask(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	require.Error(t, RecordTaskTrigger("nosuch01", ActorCLI))
	require.Error(t, RecordTaskTrigger("../etc/passwd", ActorCLI))
}
