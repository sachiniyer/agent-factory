package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// extractInstancesArrayTwoDecodeOracle is the implementation #3726 replaced,
// preserved verbatim as this test's oracle: migrate (which validates by decoding
// the envelope and normalizing the array), throw that away, then decode the same
// bytes again for the array it wanted.
//
// Keeping it here rather than describing it is the point. "One decode instead of
// two" is only worth anything if the one produces exactly what the two did, and
// the only way to assert that is to still be able to run the two.
func extractInstancesArrayTwoDecodeOracle(raw []byte, path string) (json.RawMessage, error) {
	upgraded, _, err := MigrateSchemaBytes(raw, NewInstancesSchemaMigrationPlan(path))
	if err != nil {
		return nil, err
	}
	var envelope instancesEnvelope
	if err := json.Unmarshal(upgraded, &envelope); err != nil {
		return nil, fmt.Errorf("%w: failed to parse instances envelope: %v", errInstancesSchemaContent, err)
	}
	return normalizeJSONRawArray(envelope.Instances, "instances")
}

// The read path is what LoadRepoInstances returns and what the daemon merges
// against, so collapsing its two decodes into one has to be invisible: same
// bytes, same error text, same error CLASS. The class matters as much as the
// text here — loadRepoInstancesForAll branches on *UnsupportedSchemaVersionError
// to decide whether to surface the error or fall back to raw bytes, and
// MigrateAllRepoInstancesForDaemonLoad branches on errInstancesSchemaContent to
// decide skip-and-warn versus abort.
//
// It runs over the same corpus #3724 added, for the same reason: a fixture that
// exercises the fast path, the migrator chain, or a rejection is exactly where a
// dropped decode would show up.
func TestExtractInstancesArrayMatchesTheTwoDecodeOracle(t *testing.T) {
	fixtures := readInstancesFixtures(t)
	require.NotEmpty(t, fixtures)

	nonEmpty := 0
	for _, name := range sortedFixtureNames(fixtures) {
		t.Run(name, func(t *testing.T) {
			raw := fixtures[name]
			path := "/repo/alpha/" + InstancesFileName

			got, gotErr := extractInstancesArray(raw, path)
			want, wantErr := extractInstancesArrayTwoDecodeOracle(raw, path)

			assert.Equal(t, errorText(wantErr), errorText(gotErr), "error text diverged for %s", name)
			assert.Equal(t, errorShape(wantErr), errorShape(gotErr), "error classification diverged for %s", name)
			assert.Equal(t, string(want), string(got), "instances array diverged for %s", name)
			assert.Equal(t, want == nil, got == nil, "nil-ness of the instances array diverged for %s", name)

			// A nil array with no error is the failure this refactor could
			// introduce and nothing else would catch: callers read it as "this
			// repo has no sessions" and a read-modify-write writer would then
			// clobber every live row (#766).
			if gotErr == nil {
				assert.NotNil(t, got, "%s decoded to a nil array with no error", name)
			}
		})
		if out, err := extractInstancesArray(fixtures[name], "/repo/alpha/"+InstancesFileName); err == nil && string(out) != "[]" {
			nonEmpty++
		}
	}

	// Guard the corpus: if every fixture errored or decoded to "[]", the
	// comparisons above would pass without ever exercising a real array.
	assert.GreaterOrEqual(t, nonEmpty, 3,
		"the corpus must keep several fixtures that decode to a non-empty array")
}

// The whole point is one decode rather than two, and the thing a decode costs is
// a copy of the instances array — json.RawMessage copies what it captures, and
// normalization unmarshals that into a []json.RawMessage and marshals it back.
//
// So the assertion is derived rather than a magic percentage: dropping one of
// two decodes must save at least one whole copy of the array being decoded. That
// is true however encoding/json's constant factors move, and it fails for any
// change that quietly reintroduces the second pass.
func TestExtractInstancesArrayDecodesTheEnvelopeOnce(t *testing.T) {
	raw := syntheticInstancesEnvelope(400)
	path := "/repo/alpha/" + InstancesFileName

	array, err := extractInstancesArray(raw, path)
	require.NoError(t, err)
	require.NotEmpty(t, array)

	once := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := extractInstancesArray(raw, path); err != nil {
				b.Fatal(err)
			}
		}
	}).AllocedBytesPerOp()
	twice := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := extractInstancesArrayTwoDecodeOracle(raw, path); err != nil {
				b.Fatal(err)
			}
		}
	}).AllocedBytesPerOp()

	saved := twice - once
	assert.GreaterOrEqual(t, saved, int64(len(array)),
		"one decode instead of two must save at least one copy of the %d-byte instances array; "+
			"saved %d bytes per read (%d -> %d)", len(array), saved, twice, once)
}

// errorShape is defined in schema_migration_fastpath_test.go; assert here that it
// really does separate the two classes the read path branches on, so the
// comparison above cannot pass by treating every error alike.
func TestErrorShapeSeparatesTheClassesTheReadPathBranchesOn(t *testing.T) {
	content := fmt.Errorf("%w: bad", errInstancesSchemaContent)
	newer := &UnsupportedSchemaVersionError{StoreName: InstancesFileName, FileVersion: 99, SupportedVersion: 1}

	require.True(t, errors.Is(content, errInstancesSchemaContent))
	assert.NotEqual(t, errorShape(content), errorShape(newer))
	assert.NotEqual(t, errorShape(content), errorShape(errors.New("plain")))
	assert.NotEqual(t, errorShape(nil), errorShape(content))
}
