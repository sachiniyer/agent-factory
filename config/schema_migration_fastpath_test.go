package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const instancesFixtureDir = "testdata/instances"

// instancesFastPathFires classifies every fixture in testdata/instances by
// whether ProveJSONSchemaVersion is expected to prove it is already at
// InstancesSchemaVersion.
//
// The equivalence assertions below run over every fixture whether or not it is
// listed here as firing, so this map is not what catches a lying fast path — it
// is what stops the test going vacuous. Without it, a probe that answered false
// for everything would compare the full plan against itself and pass.
//
// Adding a fixture file therefore forces a deliberate answer to "should the fast
// path recognize this?", which is the question a future migration step makes
// interesting.
var instancesFastPathFires = map[string]bool{
	// Already at v1: the whole point of the fast path.
	"v1-envelope-indented.json":                true,
	"v1-envelope-compact.json":                 true,
	"v1-instances-empty.json":                  true,
	"v1-instances-null.json":                   true,
	"v1-instances-missing.json":                true,
	"v1-instances-not-array.json":              true,
	"v1-version-key-last.json":                 true,
	"v1-nested-schema-version-key.json":        true,
	"v1-braces-inside-strings.json":            true,
	"v1-whitespace-around-members.json":        true,
	"trap-duplicate-version-current-last.json": true,

	// Traps. Each of these decodes to something a cheaper probe would get
	// wrong, and each would skip a migration the store actually needs.
	//
	//   uppercase   — encoding/json matches struct tags case-insensitively, so a
	//                 probe built on json.Unmarshal into a schema_version field
	//                 reads this as v1 while the detector's map lookup reads it
	//                 as legacy v0.
	//   duplicate   — the decoder assigns members in order, so the LAST
	//                 schema_version wins; a probe that stops at the first one
	//                 reads v1 where the detector reads v0.
	//   escaped     — "schema_version" decodes to the field name without
	//                 matching its bytes. The probe refuses rather than guess,
	//                 which is slower but never wrong.
	"trap-uppercase-version-key.json":         false,
	"trap-duplicate-version-legacy-last.json": false,
	"trap-escaped-version-key.json":           false,

	// Legacy, so there is a migration to run.
	"legacy-array.json":                  false,
	"legacy-array-empty.json":            false,
	"legacy-object-without-version.json": false,
	"object-empty.json":                  false,
	"root-null.json":                     false,

	// Not provable: malformed, not an object, or a version that is not a plain
	// JSON integer literal. Every one of these must reach the full plan, which
	// owns the error message callers classify on.
	"empty.json":                        false,
	"corrupt-truncated.json":            false,
	"corrupt-trailing-data.json":        false,
	"corrupt-leading-zero-version.json": false,
	"root-primitive.json":               false,
	"version-string.json":               false,
	"version-float.json":                false,
	"version-exponent.json":             false,
	"version-negative.json":             false,
	"version-oversized.json":            false,
	"version-null.json":                 false,
	"version-boolean.json":              false,
	"version-newer-than-binary.json":    false,
}

// TestFastPathMatchesFullPlanOverInstancesFixtures is the guard the fast path
// exists behind (#3652): for every instances fixture in the tree,
// MigrateSchemaBytes — probe and all — must produce byte-identical output, an
// identical result, and an identical error to running the migration plan end to
// end.
//
// This is what a future migration step has to keep true. Register a v1 -> v2
// migrator and every v1 fixture below stops being current: the full plan starts
// rewriting them, and any probe still claiming they are current turns this test
// red instead of silently serving unmigrated bytes to the daemon.
func TestFastPathMatchesFullPlanOverInstancesFixtures(t *testing.T) {
	fixtures := readInstancesFixtures(t)
	require.NotEmpty(t, fixtures)

	fired := 0
	for _, name := range sortedFixtureNames(fixtures) {
		t.Run(name, func(t *testing.T) {
			raw := fixtures[name]
			plan := NewInstancesSchemaMigrationPlan("/repo/alpha/" + InstancesFileName)

			wantFires, classified := instancesFastPathFires[name]
			require.True(t, classified,
				"new fixture %s is unclassified: add it to instancesFastPathFires with the answer to "+
					"'should the fast path recognize this as already current?'", name)

			require.NotNil(t, plan.ProveCurrentVersion, "the instances plan must wire a fast-path probe")
			gotFires := plan.ProveCurrentVersion(raw, plan.CurrentVersion)
			assert.Equal(t, wantFires, gotFires, "fast path engagement changed for %s", name)

			// The probe's one hard contract: a true answer must agree with the
			// detector it is standing in for. A false answer promises nothing.
			if gotFires {
				version, err := plan.DetectVersion(raw)
				require.NoError(t, err,
					"probe proved %s is current, but the detector cannot even read it", name)
				require.Equal(t, plan.CurrentVersion, version,
					"probe proved %s is at v%d, but the detector says v%d", name, plan.CurrentVersion, version)
			}

			fastBytes, fastResult, fastErr := MigrateSchemaBytes(raw, plan)
			fullBytes, fullResult, fullErr := migrateSchemaBytesFullPlan(raw, plan)

			assert.Equal(t, errorText(fullErr), errorText(fastErr), "error text diverged for %s", name)
			assert.Equal(t, errorShape(fullErr), errorShape(fastErr), "error classification diverged for %s", name)
			assert.Equal(t, fullResult, fastResult, "migration result diverged for %s", name)
			assert.Equal(t, string(fullBytes), string(fastBytes), "migrated bytes diverged for %s", name)
			assert.Equal(t, fullBytes == nil, fastBytes == nil, "nil-ness of migrated bytes diverged for %s", name)
		})
		if instancesFastPathFires[name] {
			fired++
		}
	}

	// Guard the corpus itself. The comparisons above catch a probe that stops
	// firing, because each one is checked against its classification — but if the
	// classification were edited to expect no fast path anywhere, they would all
	// still pass while comparing the full plan against itself.
	assert.GreaterOrEqual(t, fired, 5,
		"the corpus must keep several fixtures classified onto the fast path, or this test proves nothing")
}

// The tasks and TUI state plans wire the same probe because they detect their
// version the same way. Their detector IS DetectJSONSchemaVersion, so proving
// the probe agrees with it over the whole corpus covers those plans too.
func TestProveJSONSchemaVersionAgreesWithDetector(t *testing.T) {
	fixtures := readInstancesFixtures(t)
	for _, name := range sortedFixtureNames(fixtures) {
		t.Run(name, func(t *testing.T) {
			raw := fixtures[name]
			for want := 0; want <= 3; want++ {
				if !ProveJSONSchemaVersion(raw, want) {
					continue
				}
				version, err := DetectJSONSchemaVersion(raw)
				require.NoError(t, err, "probe proved v%d for %s, detector errored", want, name)
				require.Equal(t, want, version, "probe proved v%d for %s, detector says v%d", want, name, version)
			}
		})
	}
}

// A negative target version is not a version any store can be at, so the probe
// must refuse rather than let a malformed plan take the fast path.
func TestProveJSONSchemaVersionRefusesNegativeTarget(t *testing.T) {
	assert.False(t, ProveJSONSchemaVersion([]byte(`{"schema_version":1}`), -1))
}

// The fast path exists to stop allocating, so assert that rather than trusting a
// benchmark nobody runs. The full plan decodes a 1 MB instances.json into
// map[string]any to read one integer — 92,035 allocations on this repo's largest
// one — where the probe steadily makes none.
//
// "Steadily" is the honest word, and AllocsPerRun measures exactly that: it
// reports the mean over repeated runs and rounds, so the sync.Pool refill
// json.Valid takes after a GC drains the pool is amortized away rather than
// absent. A daemon profile still attributes a little to the probe; what it must
// not do is scale with the size of the document.
func TestFastPathProbeAllocatesNothingInSteadyState(t *testing.T) {
	raw := syntheticInstancesEnvelope(400)
	require.True(t, ProveJSONSchemaVersion(raw, InstancesSchemaVersion))

	allocs := testing.AllocsPerRun(50, func() {
		if !ProveJSONSchemaVersion(raw, InstancesSchemaVersion) {
			t.Fatal("probe stopped proving the fixture")
		}
	})
	assert.Zero(t, allocs, "the fast-path probe must scan the bytes without allocating per run")
}

func BenchmarkMigrateInstancesSchemaBytesAlreadyCurrent(b *testing.B) {
	raw := syntheticInstancesEnvelope(400)
	plan := NewInstancesSchemaMigrationPlan("/repo/alpha/" + InstancesFileName)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := MigrateSchemaBytes(raw, plan); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMigrateInstancesSchemaBytesFullPlan(b *testing.B) {
	raw := syntheticInstancesEnvelope(400)
	plan := NewInstancesSchemaMigrationPlan("/repo/alpha/" + InstancesFileName)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := migrateSchemaBytesFullPlan(raw, plan); err != nil {
			b.Fatal(err)
		}
	}
}

func readInstancesFixtures(t testing.TB) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(instancesFixtureDir)
	require.NoError(t, err)
	fixtures := make(map[string][]byte, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(instancesFixtureDir, entry.Name()))
		require.NoError(t, err)
		fixtures[entry.Name()] = raw
	}
	for name := range instancesFastPathFires {
		require.Contains(t, fixtures, name, "classified fixture %s no longer exists", name)
	}
	return fixtures
}

func sortedFixtureNames(fixtures map[string][]byte) []string {
	names := make([]string, 0, len(fixtures))
	for name := range fixtures {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// errorShape captures the classifications callers actually branch on:
// MigrateAllRepoInstancesForDaemonLoad aborts on a newer schema version and
// skips on corrupt content, so the fast path must not turn one into the other.
func errorShape(err error) string {
	if err == nil {
		return "nil"
	}
	shape := "error"
	var newer *UnsupportedSchemaVersionError
	if errors.As(err, &newer) {
		shape += "+unsupported-version"
	}
	if errors.Is(err, errInstancesSchemaContent) {
		shape += "+schema-content"
	}
	return shape
}

func syntheticInstancesEnvelope(records int) []byte {
	items := make([]json.RawMessage, 0, records)
	for i := 0; i < records; i++ {
		items = append(items, json.RawMessage(fmt.Sprintf(
			`{"id":"6b1f%04d-d978-4397-9dd3-2a70dc42bd34","title":"session %d","path":"/repo/alpha",`+
				`"branch":"siyer/session-%d","status":6,"tabs":[{"kind":"agent","title":"agent"}]}`,
			i, i, i)))
	}
	raw, err := marshalInstancesEnvelope(mustMarshal(items))
	if err != nil {
		panic(err)
	}
	return raw
}

func mustMarshal(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}
