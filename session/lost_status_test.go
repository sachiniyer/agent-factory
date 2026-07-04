package session

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStatusEnumValuesPinned pins every Status value's integer encoding.
// Status serializes as an int in instances.json, so these are an on-disk
// format: values may only ever be APPENDED (Lost, #1108, went after Dead) —
// reordering or inserting silently rewrites the meaning of every persisted
// record.
func TestStatusEnumValuesPinned(t *testing.T) {
	assert.Equal(t, Status(0), Running)
	assert.Equal(t, Status(1), Ready)
	assert.Equal(t, Status(2), Loading)
	assert.Equal(t, Status(3), Deleting)
	assert.Equal(t, Status(4), Dead)
	assert.Equal(t, Status(5), Lost)
}

// TestInstanceData_UserKilledTombstoneJSON pins the tombstone's wire format
// (#1108): user_killed serializes when set, is omitted when false (so healthy
// records don't grow), and defaults to false when absent — every record
// written before the field existed reads as not-tombstoned.
func TestInstanceData_UserKilledTombstoneJSON(t *testing.T) {
	out, err := json.Marshal(InstanceData{Title: "t", UserKilled: true})
	require.NoError(t, err)
	assert.Contains(t, string(out), `"user_killed":true`)

	out, err = json.Marshal(InstanceData{Title: "t"})
	require.NoError(t, err)
	assert.NotContains(t, string(out), "user_killed",
		"the tombstone must be omitempty: it only exists inside a kill's crash window")

	var legacy InstanceData
	require.NoError(t, json.Unmarshal([]byte(`{"title":"old"}`), &legacy))
	assert.False(t, legacy.UserKilled,
		"records written before #1108 must read as not-tombstoned")
}
