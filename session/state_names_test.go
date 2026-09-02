package session

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The filter vocabulary, restated as a literal on purpose.
//
// Everything in production derives these words from ONE table, which is the
// point of #3631 — but a derivation chain cannot pin what the words ARE. A
// rename that swept the table, the flag help, and the payload together would be
// invisible to every consistency check and would still break every script that
// filters on "limit-reached". This literal is the contract those scripts hold,
// so changing it has to be a deliberate edit here.
var wantLivenessNames = map[Liveness]string{
	LiveRunning:      "running",
	LiveReady:        "ready",
	LiveLost:         "lost",
	LiveDead:         "dead",
	LiveArchived:     "archived",
	LiveLimitReached: "limit-reached",
}

var wantStatusNames = map[Status]string{
	Running:  "running",
	Ready:    "ready",
	Loading:  "loading",
	Deleting: "deleting",
	Dead:     "dead",
	Lost:     "lost",
	Archived: "archived",
}

var wantTabKindNames = map[TabKind]string{
	TabKindAgent:   "agent",
	TabKindShell:   "shell",
	TabKindProcess: "process",
	TabKindWeb:     "web",
	TabKindVSCode:  "vscode",
}

// TestStateNamesPinTheWireVocabulary is the spelling half: every declared value
// names itself exactly as the contract above says.
func TestStateNamesPinTheWireVocabulary(t *testing.T) {
	for liveness, want := range wantLivenessNames {
		require.Equal(t, want, LivenessName(liveness), "liveness %d", liveness)
		got, ok := ParseLivenessName(want)
		require.True(t, ok, "%q must parse back", want)
		require.Equal(t, liveness, got, "%q must round-trip to its own value", want)
	}
	for status, want := range wantStatusNames {
		require.Equal(t, want, StatusName(status), "status %d", status)
	}
	for kind, want := range wantTabKindNames {
		require.Equal(t, want, TabKindName(kind), "tab kind %d", kind)
	}
}

// TestLivenessNamesAreTheFilterVocabulary pins that the words a row reports are
// the words `af sessions list --status` accepts — the defect in #3631 was
// precisely a filter vocabulary that appeared nowhere in the output.
//
// LivenessUnset is excluded because it is not a state: it is the "no liveness
// recorded" zero value of a pre-#1195 record, and RecordedLiveness resolves it
// to a real one before anything is named (pinned below).
func TestLivenessNamesAreTheFilterVocabulary(t *testing.T) {
	require.Equal(t,
		[]string{"running", "ready", "lost", "dead", "archived", "limit-reached"},
		LivenessNameList(),
		"the advertised vocabulary, in enum order")

	require.Empty(t, LivenessName(LivenessUnset), "the zero value is not a filter word")
	_, ok := ParseLivenessName("")
	require.False(t, ok, "the empty string is not a status")
	_, ok = ParseLivenessName("limit_reached")
	require.False(t, ok, "only the hyphenated spelling is the vocabulary")
}

// TestEveryDeclaredEnumValueHasAName is the coverage half, and it reads the
// SOURCE rather than a list of constants a test author remembered to update.
//
// A hand-written loop over the values this file knows about would pass forever
// after someone appends an eighth Status: the new value would serialize as an
// integer nothing can name — the exact defect #3631 fixed — and the suite would
// stay green. Deriving the declared set from the AST makes an unnamed value fail
// here the day it is added.
//
// Each enum is a contiguous `= iota` run, so N declared constants ARE the values
// 0…N-1; the check is that the name table covers exactly that range. An added
// constant (with or without an explicit value) grows the declared count and
// fails until it is named.
func TestEveryDeclaredEnumValueHasAName(t *testing.T) {
	declared := declaredEnumConstants(t, "Status", "Liveness", "TabKind")

	requireCoversRange(t, "Status", declared["Status"], 0, func(i int) string { return StatusName(Status(i)) })

	// Liveness starts at 1: LivenessUnset is the "no liveness recorded" zero of a
	// pre-#1195 record, not a state, and RecordedLiveness resolves it to a real
	// value before anything is named (TestRecordedLivenessNamesEveryRecord).
	requireCoversRange(t, "Liveness", declared["Liveness"], 1, func(i int) string { return LivenessName(Liveness(i)) })

	requireCoversRange(t, "TabKind", declared["TabKind"], 0, func(i int) string { return TabKindName(TabKind(i)) })
}

// requireCoversRange asserts that name() answers for every declared value from
// first onward, and reports the offending constant BY NAME so the failure says
// which value to teach rather than which index.
func requireCoversRange(t *testing.T, typeName string, declared []string, first int, name func(int) string) {
	t.Helper()
	require.NotEmpty(t, declared)
	for i := first; i < len(declared); i++ {
		require.NotEmptyf(t, name(i),
			"%s value %d (%s) has no name. Add it to the vocabulary table in "+
				"session/state_names.go and to this file's contract literal — a value "+
				"with no name serializes as an integer no consumer can decode (#3631).",
			typeName, i, declared[i])
	}
	require.Lenf(t, name(len(declared)), 0,
		"%s has no value %d, yet something named it — the name table has drifted past the enum",
		typeName, len(declared))
}

// TestRecordedLivenessNamesEveryRecord pins the property the round-trip rests
// on: a record always resolves to a NAMED liveness, including one written before
// the `liveness` field existed and one caught mid-operation.
func TestRecordedLivenessNamesEveryRecord(t *testing.T) {
	for _, tc := range []struct {
		name string
		data InstanceData
		want string
	}{
		{"explicit liveness wins", InstanceData{Status: Ready, Liveness: LiveLimitReached}, "limit-reached"},
		{"legacy record falls back to status", InstanceData{Status: Archived}, "archived"},
		{"legacy dead is NOT renamed to lost", InstanceData{Status: Dead}, "dead"},
		{"explicit dead is NOT renamed to lost", InstanceData{Liveness: LiveDead}, "dead"},
		{"legacy transient resolves to a real state", InstanceData{Status: Deleting}, "ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, LivenessName(RecordedLiveness(tc.data)))
			require.NotEmpty(t, LivenessName(RecordedLiveness(tc.data)))
		})
	}
}

// TestInstanceDataJSONAlwaysCarriesItsNames is the payload contract: the twins
// are present on every record, never omitempty, and they name the integer that
// sits beside them.
func TestInstanceDataJSONAlwaysCarriesItsNames(t *testing.T) {
	for _, tc := range []struct {
		name         string
		data         InstanceData
		status       float64
		statusName   string
		livenessName string
	}{
		{"live row", InstanceData{Status: Ready, Liveness: LiveReady}, 1, "ready", "ready"},
		{"limit-parked row composes to the legacy Ready", InstanceData{Status: Ready, Liveness: LiveLimitReached}, 1, "ready", "limit-reached"},
		{"mid-create row", InstanceData{Status: Loading, Liveness: LiveRunning}, 2, "loading", "running"},
		// Dead must NOT be renamed to Lost on the way out. EffectiveLiveness
		// applies that rewrite because a dead record is recovery-eligible when an
		// Instance is rebuilt from it (#1108); naming through it would print
		// "lost" beside a `liveness` that reads 4 and would desync the name from
		// `--status dead`, which selects on the record's own value.
		{"dead row keeps its own name", InstanceData{Status: Dead, Liveness: LiveDead}, 4, "dead", "dead"},
		{"legacy dead row keeps its own name", InstanceData{Status: Dead}, 4, "dead", "dead"},
		{"zero value", InstanceData{}, 0, "running", "running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := marshalToMap(t, tc.data)
			require.Equal(t, tc.status, got["status"], "the integer keeps its type and value")
			require.Equal(t, tc.statusName, got["status_name"])
			require.Equal(t, tc.livenessName, got["liveness_name"])
			require.Contains(t, got, "status_name", "always present, never omitempty")
			require.Contains(t, got, "liveness_name", "always present, never omitempty")
		})
	}
}

// TestTabKindNameMatchesTabKindsArray closes the in-document inconsistency #3631
// reported: `tabs[].kind` and `tab_kinds[].kind` are one concept and must now
// spell it identically.
func TestTabKindNameMatchesTabKindsArray(t *testing.T) {
	data := InstanceData{Tabs: []TabData{
		{Name: "agent", Kind: TabKindAgent},
		{Name: "shell", Kind: TabKindShell},
		{Name: "logs", Kind: TabKindProcess},
		{Name: "web", Kind: TabKindWeb},
		{Name: "vscode", Kind: TabKindVSCode},
	}}
	got := marshalToMap(t, data)
	tabs, ok := got["tabs"].([]any)
	require.True(t, ok)
	require.Len(t, tabs, 5)

	var names []string
	for i, raw := range tabs {
		tab, ok := raw.(map[string]any)
		require.True(t, ok)
		require.Equal(t, float64(i), tab["kind"], "the integer keeps its type and value")
		require.Contains(t, tab, "kind_name", "always present, never omitempty")
		names = append(names, tab["kind_name"].(string))
	}
	require.Equal(t, []string{"agent", "shell", "process", "web", "vscode"}, names)

	// Every creatable kind's `--kind` spelling is the same word `kind_name` uses,
	// so a client can hand a tab's reported kind straight back to tab-create.
	for _, name := range TabKindNameList() {
		kind, ok := ParseTabKindName(name)
		require.True(t, ok)
		require.Equal(t, name, TabKindName(kind), "one word per kind, both directions")
	}
}

// TestUnknownEnumValueNamesNothing pins the forward-compatibility posture: a
// record from a newer af reports no name rather than a confident wrong one.
func TestUnknownEnumValueNamesNothing(t *testing.T) {
	require.Empty(t, StatusName(Status(99)))
	require.Empty(t, LivenessName(Liveness(99)))
	require.Empty(t, TabKindName(TabKind(99)))

	got := marshalToMap(t, InstanceData{Status: Status(99), Liveness: Liveness(99)})
	require.Equal(t, "", got["status_name"])
	require.Equal(t, "", got["liveness_name"])
	require.Contains(t, got, "status_name", "present even when it cannot be spelled")
}

// TestStateNamesSurviveADecodeEncodeRoundTrip proves the names cannot go stale:
// they are derived at encode time, not stored, so a record that crossed the wire
// from a daemon too old to send them is still named by the binary printing it.
func TestStateNamesSurviveADecodeEncodeRoundTrip(t *testing.T) {
	const fromAnOlderDaemon = `{"title":"skew","status":1,"liveness":6,"tabs":[{"name":"agent","kind":0}]}`

	var decoded InstanceData
	require.NoError(t, json.Unmarshal([]byte(fromAnOlderDaemon), &decoded))
	got := marshalToMap(t, decoded)
	require.Equal(t, "ready", got["status_name"])
	require.Equal(t, "limit-reached", got["liveness_name"])

	// And a payload that claims a WRONG name cannot poison the re-encode, because
	// nothing decodes the names back into the record.
	const lying = `{"title":"x","status":1,"liveness":2,"status_name":"archived","liveness_name":"archived"}`
	var relayed InstanceData
	require.NoError(t, json.Unmarshal([]byte(lying), &relayed))
	got = marshalToMap(t, relayed)
	require.Equal(t, "ready", got["status_name"], "the name is re-derived, never relayed")
	require.Equal(t, "ready", got["liveness_name"])
}

// TestAppendJSONMemberKeepsFieldOrder pins the splice used by the one type that
// embeds InstanceData (api.sessionGetResult).
func TestAppendJSONMemberKeepsFieldOrder(t *testing.T) {
	out, err := AppendJSONMember([]byte(`{"b":1,"a":2}`), "warnings", []byte(`["x"]`))
	require.NoError(t, err)
	require.Equal(t, `{"b":1,"a":2,"warnings":["x"]}`, string(out))

	out, err = AppendJSONMember([]byte(`{}`), "warnings", []byte(`["x"]`))
	require.NoError(t, err)
	require.Equal(t, `{"warnings":["x"]}`, string(out))

	// A pretty-printed empty object is still empty: a length test would read its
	// newline as a member and emit an invalid leading comma.
	out, err = AppendJSONMember([]byte("{\n}"), "warnings", []byte(`["x"]`))
	require.NoError(t, err)
	require.JSONEq(t, `{"warnings":["x"]}`, string(out))

	_, err = AppendJSONMember([]byte(`[1,2]`), "warnings", []byte(`["x"]`))
	require.Error(t, err, "a non-object must be refused rather than corrupted")
}

func marshalToMap(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	return got
}

// declaredEnumConstants returns, per type name, the identifiers declared as
// constants of that type anywhere in this package's non-test source. A spec with
// no type inherits the previous spec's type within the same const block, which
// is how every one of these enums is written (`Ready` after `Running Status =
// iota`), so the walk tracks that carry-over rather than reading only the first
// line of each block.
func declaredEnumConstants(t *testing.T, typeNames ...string) map[string][]string {
	t.Helper()

	wanted := map[string]bool{}
	for _, name := range typeNames {
		wanted[name] = true
	}

	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	out := map[string][]string{}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		require.NoError(t, err, name)
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			current := ""
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				if value.Type != nil {
					ident, ok := value.Type.(*ast.Ident)
					if !ok {
						current = ""
						continue
					}
					current = ident.Name
				} else if len(value.Values) > 0 {
					// A spec with its own value and no type starts a new, untyped
					// run rather than continuing the iota one above it.
					current = ""
				}
				if !wanted[current] {
					continue
				}
				for _, ident := range value.Names {
					out[current] = append(out[current], ident.Name)
				}
			}
		}
	}
	for name := range wanted {
		require.NotEmpty(t, out[name], "found no declared %s constants — the AST walk is broken, not the enum", name)
	}
	return out
}
