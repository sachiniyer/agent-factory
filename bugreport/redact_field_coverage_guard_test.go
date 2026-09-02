package bugreport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// Tests of the #3548 guard's own machinery, on shapes InstanceData does not have
// today.
//
// TestRedactInstanceDataCoversEveryStringField can only exercise the parts of
// that machinery the real record reaches, so the properties its verdict rests on
// are pinned here instead: an encoded fragment names exactly one field, an
// element type that renders itself is refused rather than probed blindly,
// capturing evidence does not destroy the evidence, and a path cannot be
// classified two contradictory ways at once. Each of these was a fail-OPEN — the
// guard reporting a clean field, or reporting a redacted one as leaking — which
// is why they are worth a fixture rather than a comment.

// guardWipingByte is a named byte type whose POINTER-receiver marshaler renders
// the element and then clears it: the wipe-after-read shape a secret-holding
// type plausibly has. encoding/json addresses an addressable element rather than
// copying it, so this mutation reaches whatever container it is asked to render.
type guardWipingByte uint8

func (b *guardWipingByte) MarshalJSON() ([]byte, error) {
	out := []byte(strconv.Itoa(int(*b)))
	*b = 0
	return out, nil
}

// TestGuardFragmentsNameExactlyOneField is the attribution property. Two
// containers are planted, the redactor blanks ONE, and the guard must report the
// survivor alone. A fragment shared between the two makes the redacted field
// read as leaked — and, worse for the debt register, lets a stale allowlist
// entry stay green on evidence belonging to its neighbour.
func TestGuardFragmentsNameExactlyOneField(t *testing.T) {
	for _, tc := range []struct {
		name  string
		probe any
	}{
		{"byte slices", &struct{ Redacted, Leaked []byte }{}},
		{"rune slices", &struct{ Redacted, Leaked []rune }{}},
		// Longer than any single marker, so a walk that plants a prefix and
		// leaves the rest zeroed gives both fields an identical zero tail.
		{"byte arrays", &struct{ Redacted, Leaked [64]byte }{}},
		{"rune arrays", &struct{ Redacted, Leaked [64]rune }{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := reflect.ValueOf(tc.probe).Elem()
			filler := &sentinelFiller{}
			filler.fill(record, "", 0, false)
			if len(filler.unsupported) > 0 || len(filler.tooDeep) > 0 {
				t.Fatalf("the walk could not plant the fixture: unsupported=%v tooDeep=%v",
					filler.unsupported, filler.tooDeep)
			}
			if len(filler.planted) != 2 {
				t.Fatalf("planted %d field(s), want 2: %+v", len(filler.planted), filler.planted)
			}
			if ambiguous := filler.ambiguousEvidence(); len(ambiguous) > 0 {
				t.Errorf("planted fragments are not field-specific:\n  %s",
					strings.Join(ambiguous, "\n  "))
			}

			// The redaction reached one field and missed the other.
			record.Field(0).Set(reflect.Zero(record.Field(0).Type()))
			doc, err := json.Marshal(record.Interface())
			if err != nil {
				t.Fatalf("marshalling the fixture failed: %v", err)
			}
			_, leaked := filler.leakedPaths(string(doc))
			if want := []string{"Leaked"}; !reflect.DeepEqual(leaked, want) {
				t.Errorf("leakedPaths = %v, want %v\ndocument: %s\n\n"+
					"Reporting the redacted field means one field's evidence matched the "+
					"other's bytes; reporting neither means the evidence does not survive a "+
					"real leak at all.", leaked, want, doc)
			}
		})
	}
}

// TestGuardSeesAPartiallyRedactedContainer is why fragments exist at all: a
// redactor that rewrites the head of a container and stops must not read as a
// complete redaction. The whole-value form is gone after such an edit, so only
// the windows over the part it left alone can still report the leak.
func TestGuardSeesAPartiallyRedactedContainer(t *testing.T) {
	var probe struct {
		Digest []byte
	}
	record := reflect.ValueOf(&probe).Elem()
	filler := &sentinelFiller{}
	filler.fill(record, "", 0, false)
	if len(filler.planted) != 1 {
		t.Fatalf("planted %d field(s), want 1", len(filler.planted))
	}

	// One unit blanked; every later byte still verbatim.
	for i := 0; i < guardUnitLen; i++ {
		probe.Digest[i] = 'X'
	}
	doc, err := json.Marshal(probe)
	if err != nil {
		t.Fatalf("marshalling the fixture failed: %v", err)
	}
	if _, leaked := filler.leakedPaths(string(doc)); len(leaked) != 1 {
		t.Errorf("leakedPaths = %v, want [Digest]: a container redacted only at the head "+
			"still ships every byte after it\ndocument: %s", leaked, doc)
	}
}

// TestGuardRefusesAnArrayWithoutIndependentEvidence covers the other end of the
// same property. One complete unit is enough to NAME a field, but an array
// holding only one has no evidence beyond its exact whole-array encoding, and a
// redactor that rewrites a single element erases that — the field then reads as
// redacted while the rest of it ships verbatim.
func TestGuardRefusesAnArrayWithoutIndependentEvidence(t *testing.T) {
	var probe struct {
		Digest [guardMinContainer - 1]byte
	}
	filler := &sentinelFiller{}
	filler.fill(reflect.ValueOf(&probe).Elem(), "", 0, false)
	if len(filler.planted) != 0 {
		t.Errorf("planted %d marker(s) in an array too short to survive a partial edit",
			len(filler.planted))
	}
	if len(filler.unsupported) != 1 || !strings.Contains(filler.unsupported[0], "Digest") {
		t.Errorf("unsupported = %v, want one entry naming Digest", filler.unsupported)
	}
}

// TestGuardSeesAPartialEditOfTheSmallestArray is the accepted end of that
// boundary: the shortest array the guard WILL plant into must still be reported
// after a redaction that rewrites its head and stops.
func TestGuardSeesAPartialEditOfTheSmallestArray(t *testing.T) {
	var probe struct {
		Digest [guardMinContainer]byte
	}
	record := reflect.ValueOf(&probe).Elem()
	filler := &sentinelFiller{}
	filler.fill(record, "", 0, false)
	if len(filler.planted) != 1 {
		t.Fatalf("planted %d field(s), want 1: unsupported=%v", len(filler.planted), filler.unsupported)
	}
	for i := 0; i < guardUnitLen; i++ {
		probe.Digest[i] = 'X'
	}
	doc, err := json.Marshal(probe)
	if err != nil {
		t.Fatalf("marshalling the fixture failed: %v", err)
	}
	if _, leaked := filler.leakedPaths(string(doc)); len(leaked) != 1 {
		t.Errorf("leakedPaths = %v, want [Digest]\ndocument: %s", leaked, doc)
	}
}

// TestGuardRefusesSelfRenderingSequenceElements pins the element-type check. The
// byte and rune branches dispatch on KIND, so a named uint8 or int32 with its own
// marshaler would otherwise slip past the rendersItself rejection every other
// shape gets, and be probed as if its raw elements were what json emits.
func TestGuardRefusesSelfRenderingSequenceElements(t *testing.T) {
	for _, tc := range []struct {
		name  string
		probe any
	}{
		{"slice", &struct{ Digest []guardWipingByte }{}},
		{"array", &struct{ Digest [64]guardWipingByte }{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filler := &sentinelFiller{}
			filler.fill(reflect.ValueOf(tc.probe).Elem(), "", 0, false)
			if len(filler.planted) != 0 {
				t.Errorf("planted %d marker(s) in a container whose element renders itself",
					len(filler.planted))
			}
			if len(filler.unsupported) != 1 ||
				!strings.Contains(filler.unsupported[0], "Digest") ||
				!strings.Contains(filler.unsupported[0], "renders itself") {
				t.Errorf("unsupported = %v, want one entry naming Digest as self-rendering",
					filler.unsupported)
			}
		})
	}
}

// TestGuardEvidenceCaptureLeavesThePlantedValueIntact pins the other half of
// that hazard, which survives the rejection above for any element type that IS
// reviewed: capturing a container's encoding must not hand the live array to a
// marshaler that mutates it. If it does, the walk records evidence of a value
// the document no longer holds, and an entirely unredacted field reads as clean.
func TestGuardEvidenceCaptureLeavesThePlantedValueIntact(t *testing.T) {
	var probe struct {
		Digest []guardWipingByte
	}
	field := reflect.ValueOf(&probe).Elem().Field(0)

	filler := &sentinelFiller{}
	marker := filler.nextContainerMarker(guardContainerLen)
	raw := []byte(marker)
	planted := reflect.MakeSlice(field.Type(), len(raw), len(raw))
	for i, b := range raw {
		planted.Index(i).SetUint(uint64(b))
	}
	field.Set(planted)
	before := fmt.Sprint(probe.Digest)

	forms, encoded := filler.formsFor(field, marker, true)
	if after := fmt.Sprint(probe.Digest); after != before {
		t.Errorf("capturing evidence mutated the planted container:\n  before %s\n  after  %s\n\n"+
			"json addresses the live elements, so the capture must marshal a copy.", before, after)
	}

	// The recorded evidence has to describe what the finished document shows.
	doc, err := json.Marshal(probe.Digest)
	if err != nil {
		t.Fatalf("marshalling the planted container failed: %v", err)
	}
	if encoded != string(doc) {
		t.Errorf("recorded encoding %s does not match the document's %s", encoded, doc)
	}
	for _, form := range forms[1:] {
		if !strings.Contains(string(doc), form) {
			t.Errorf("recorded fragment %q is absent from the unredacted document %s", form, doc)
		}
	}
}

// TestGuardRejectsContradictoryClassification pins the disjointness of the two
// classification maps. "Safe to publish" and "a tracked leak" are opposite
// verdicts; a path holding both is accepted by the guard because it holds one,
// while both stale checks stay quiet because the marker really does survive.
func TestGuardRejectsContradictoryClassification(t *testing.T) {
	safe := map[string]string{"Both": "judged safe", "OnlySafe": "judged safe"}
	known := map[string]string{"Both": "#3588", "OnlyKnown": "#3588"}

	if got := classificationOverlap(safe, known); !reflect.DeepEqual(got, []string{"Both"}) {
		t.Errorf("classificationOverlap = %v, want [Both]", got)
	}
	if got := classificationOverlap(safe, map[string]string{"OnlyKnown": "#3588"}); len(got) > 0 {
		t.Errorf("classificationOverlap = %v, want none for disjoint maps", got)
	}
	// The property the guard actually depends on, asserted against the real
	// maps too — the same check the main test makes, so a contradictory entry
	// cannot be introduced while this file is the only one being read.
	if got := classificationOverlap(verbatimInstanceFields, knownUnredactedFields); len(got) > 0 {
		t.Errorf("%d path(s) are classified both safe and leaking: %v", len(got), got)
	}
}

// guardTextScalar is a named int32 that renders itself as text. It is the one
// scalar shape that can carry arbitrary user text into a bundle, and the walk
// plants no marker in a scalar leaf — so it has to be refused before it gets
// there, or the guard would stay green over a field emitting whatever this
// method returns.
// TestGuardReportsEveryRepeatedScalarLeaf is where the scalar exemption stops.
//
// A scalar leaf is exempt because its capacity is BOUNDED, and repeating it
// removes exactly that. Assessed per LEAF, not per element: a sibling string in
// the same struct is evidence about the string and says nothing about the number
// beside it. Reproduced with []struct{ Code int32; Name string } — a redactor
// that cleared only Name left Code's 83 and 84 in the document while the guard
// reported nothing at all (#3592 review).
func TestGuardReportsEveryRepeatedScalarLeaf(t *testing.T) {
	type mixed struct {
		Code int32
		Name string
		When time.Time
	}
	var probe struct {
		Entries []mixed
		ByName  map[string]mixed
		Codes   []struct{ Code int32 }
		// NOT repeated: one bounded value at a fixed position, still exempt.
		Once mixed
	}
	filler := &sentinelFiller{}
	filler.fill(reflect.ValueOf(&probe).Elem(), "", 0, false)

	if len(filler.unsupported) > 0 {
		t.Errorf("unsupported = %v, want none: a repeated scalar is classified, not a hole, "+
			"and a timestamp is text-free", filler.unsupported)
	}
	got := dedupeSorted(filler.unplantable)
	want := []string{"ByName[].Code", "Codes[].Code", "Entries[].Code"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("unplantable = %v,\nwant %v\n\n"+
			"Once.Code must NOT appear — it sits at a fixed position, where the bounded-capacity "+
			"exemption still holds.", got, want)
	}
	// Reporting the value leaves must not cost the coverage the map keys had.
	var keys int
	for _, p := range filler.planted {
		if strings.HasSuffix(p.path, "[key]") {
			keys++
		}
	}
	if keys != guardSliceSeed {
		t.Errorf("planted %d map key(s), want %d", keys, guardSliceSeed)
	}
}

type guardTextScalar int32

func (guardTextScalar) MarshalText() ([]byte, error) { return []byte("secret-text"), nil }

func TestGuardRefusesSelfRenderingScalarLeaves(t *testing.T) {
	var probe struct {
		Key    guardTextScalar
		KeyPtr *guardTextScalar
	}
	filler := &sentinelFiller{}
	filler.fill(reflect.ValueOf(&probe).Elem(), "", 0, false)
	if len(filler.planted) != 0 {
		t.Errorf("planted %d marker(s) in a self-rendering scalar", len(filler.planted))
	}
	got := dedupeSorted(filler.unsupported)
	if len(got) != 2 || !strings.Contains(got[0], "Key ") || !strings.Contains(got[1], "KeyPtr") {
		t.Errorf("unsupported = %v, want both Key and KeyPtr reported as self-rendering", got)
	}
}

// guardNamedKey is a named string type that renders itself. encoding/json
// resolves a string-kind MAP KEY from its underlying string and never calls this
// method, which is why fillMap deliberately exempts such key types — but a
// capture that marshals the key VALUE does call it, recording a form the
// document never contains.
type guardNamedKey string

func (guardNamedKey) MarshalText() ([]byte, error) { return []byte("CONSTANT-KEY-FORM"), nil }

// TestGuardRecordsMapKeysAsJSONEmitsThem pins that capture. With the key
// marshalled as a value, two such fields record the SAME constant form, so
// ambiguousEvidence fails the suite on the very shape the exemption supports —
// while the document holds their distinct raw markers all along.
func TestGuardRecordsMapKeysAsJSONEmitsThem(t *testing.T) {
	var probe struct {
		A map[guardNamedKey]string
		B map[guardNamedKey]string
	}
	record := reflect.ValueOf(&probe).Elem()
	filler := &sentinelFiller{}
	filler.fill(record, "", 0, false)
	if len(filler.unsupported) > 0 {
		t.Fatalf("the walk could not plant the fixture: %v", filler.unsupported)
	}
	if ambiguous := dedupeSorted(filler.ambiguousEvidence()); len(ambiguous) > 0 {
		t.Errorf("map-key evidence is not field-specific:\n  %s", strings.Join(ambiguous, "\n  "))
	}

	doc, err := json.Marshal(probe)
	if err != nil {
		t.Fatalf("marshalling the fixture failed: %v", err)
	}
	// Nothing redacted anything, so every planted path must be reported — and
	// each recorded key form must be one the document actually contains.
	_, leaked := filler.leakedPaths(string(doc))
	sort.Strings(leaked)
	want := []string{"A[]", "A[key]", "B[]", "B[key]"}
	if !reflect.DeepEqual(leaked, want) {
		t.Errorf("leakedPaths = %v, want %v\ndocument: %s", leaked, want, doc)
	}
	for _, p := range filler.planted {
		if p.encoded != "" && !strings.Contains(string(doc), p.encoded) {
			t.Errorf("%s recorded the encoding %s, which the document does not contain: %s",
				p.path, p.encoded, doc)
		}
	}
}

// guardMarshalerStates are the scalar values every reviewed marshaler is read
// at, and guardScalarPatterns are how those values are spread across the
// record's scalars.
//
// One value for every scalar reads only the DIAGONAL, and production
// combinations are not on it: Archived is 6 while LiveArchived is 5, so the pair
// a record actually carries when archived never appeared (#3592 review). The
// patterns spread neighbouring scalars apart instead.
//
// This is representative and cannot be made exhaustive: a marshaler may gate on
// any predicate over the record. What does not depend on guessing the gate is
// everything else below — the field-set diff runs at EVERY state, both
// independence checks compare two records differing in exactly one thing, and
// TestGuardInstanceDataMarshalerIgnoresHiddenStateWhenArchived reads by name the
// one combination production is known to populate.
var (
	guardMarshalerStates = []int{1, 2, 3, 4, 5, 6, 7, 8}
	guardScalarPatterns  = []func(index, state int) int{
		func(_, state int) int { return state },
		func(index, state int) int { return (state + index) % 9 },
		func(index, state int) int { return (state + 2*index) % 9 },
	}
)

// unpopulatedMarshalerState records the unwalked state a contract fixture cannot
// populate, keyed by "<type>: <report>".
//
// The contract plants every unexported and `json:"-"` field it can reach, at any
// depth, and compares the output with that state populated against the output
// with it empty. A leaf it cannot plant stays empty in BOTH, so nothing it might
// contribute is visible — a gap, not a pass, and written down here rather than
// skipped (#3592 review).
//
// The bar is the same as the classification maps: a reason saying why the
// contract still holds without it.
var unpopulatedMarshalerState = map[string]string{
	"session.InstanceData: archiveReportSource.hooksCtx (interface)": "an interface field on the git worktree HANDLE the record keeps for its archive report. " +
		"The walk cannot choose a concrete type for it, and the surrounding *git.GitWorktree IS allocated and planted — so anything the marshaler emitted " +
		"from that handle still moves the output and fails the independence check; only a value derived from this one nil field would not",
}

// unclassifiedFixtureGaps returns the unplantable unwalked state that is not
// recorded in unpopulatedMarshalerState.
func unclassifiedFixtureGaps(typ reflect.Type, filler *sentinelFiller) []string {
	var out []string
	for _, gap := range append(dedupeSorted(filler.unsupported), dedupeSorted(filler.tooDeep)...) {
		if _, ok := unpopulatedMarshalerState[typ.String()+": "+gap]; ok {
			continue
		}
		out = append(out, gap)
	}
	return out
}

// TestGuardFixtureGapRegisterIsCurrent keeps that register honest: every entry
// justified, and every entry still describing state the fixture really cannot
// populate.
//
// A separate test rather than a check deferred inside the contract, because the
// contract runs one subtest per type: under `go test -run …/git.ArchiveRetainedTree`
// a deferred check leaves every other entry unseen and reports it stale.
// Measured, while probing something else.
func TestGuardFixtureGapRegisterIsCurrent(t *testing.T) {
	assertJustified(t, "unpopulatedMarshalerState", unpopulatedMarshalerState,
		"every entry needs the reason the contract still holds without that state")

	met := map[string]struct{}{}
	for typ := range reviewedMarshalerTypes {
		if typ == reflect.TypeOf(time.Time{}) {
			continue
		}
		filler := &sentinelFiller{}
		value := reflect.New(typ).Elem()
		filler.fill(value, "", 0, false)
		filler.unsupported, filler.tooDeep = nil, nil
		plantUnwalkedState(filler, value)
		for _, gap := range append(dedupeSorted(filler.unsupported), dedupeSorted(filler.tooDeep)...) {
			met[typ.String()+": "+gap] = struct{}{}
		}
	}
	assertNoStaleEntries(t, "unpopulatedMarshalerState", unpopulatedMarshalerState, met,
		"The walk can populate them now, or they are gone. Remove the entry so the register "+
			"keeps describing the fixture that actually runs.")
}

// marshalerFixture is one planted record and what its marshaler made of it.
type marshalerFixture struct {
	custom   []byte
	baseline []byte
	// unwalked are the forms planted into fields encoding/json would never emit
	// on its own — unexported, and exported-but-`json:"-"`, at any depth.
	unwalked []string
}

// newMarshalerFixture builds a FRESH record and marshals it.
//
// Fresh every time on purpose. A marshaler that mutates what it renders — hidden
// state, or a shared backing array — taints the value for every later reading,
// and the state that would have leaked then sees only what the previous call
// left behind (#3592 review).
//
// seq fixes the marker sequence, so two fixtures built with the same seq carry
// IDENTICAL planted text and differ only in whatever else the caller varies.
// That is what makes the independence checks possible.
func newMarshalerFixture(t *testing.T, typ reflect.Type, seq int, pattern func(int, int) int, state int, withUnwalked bool) marshalerFixture {
	t.Helper()
	filler := &sentinelFiller{seq: seq}
	value := reflect.New(typ).Elem()
	filler.fill(value, "", 0, false)
	var unwalked []string
	if withUnwalked {
		unwalked = plantUnwalkedState(filler, value)
		// An unwalked field the walk could not populate stays at its zero value,
		// and a marshaler that emits it only when it is set would then pass on
		// an empty fixture (#3592 review). Each one is written down by name.
		for _, gap := range unclassifiedFixtureGaps(typ, filler) {
			t.Errorf("%s has state encoding/json cannot reach that this fixture cannot populate "+
				"either:\n  %s\n\nA marshaler that emits it only when it is set would pass on an "+
				"empty fixture. Teach the walk to populate it, or record it in "+
				"unpopulatedMarshalerState with the reason the contract still holds without it.",
				typ, gap)
		}
	}
	if pattern != nil {
		seedScalars(value, pattern, state, "")
	}
	// The baseline is captured FIRST. The twin shares the planted slices and
	// maps, so a value-receiver marshaler that rewrites what it renders would
	// otherwise be compared against a baseline it had already rewritten.
	return marshalerFixture{
		baseline: marshalOrFail(t, "the plain twin of "+typ.String(), plainTwinOf(t, value)),
		custom:   marshalReviewed(t, typ, value),
		unwalked: unwalked,
	}
}

// TestGuardReviewedMarshalersMatchTheirFieldSet turns every entry of
// reviewedMarshalerTypes from a point-in-time reading into a checked contract.
//
// An entry claims the type's MarshalJSON emits the same exported fields this
// walk plants into, plus the extras it declares. Nothing tied that claim to the
// code. A marshaler that later base64-encodes a field keeps its exemption while
// the walk records evidence the document no longer shows; one that starts
// emitting text out of state the walk cannot reach keeps it while shipping
// something the guard has never seen (#3592 review).
//
// So the claim is executed, four ways, on a fresh record every time:
//
//   - a DIFF against a plain twin built with reflect.StructOf — the same
//     exported fields, same tags, no method set — so encoding/json itself
//     produces the baseline. Run at EVERY state, because a state-gated marshaler
//     can replace or drop an ordinary member as easily as add one.
//   - the declared extras' VALUES, pinned at the state the entry describes.
//   - extras INDEPENDENT of planted text: two records differing only in the text
//     planted in them must produce identical extras, which catches a derivation
//     no substring search can — base64 of a name, a hash of it.
//   - output INDEPENDENT of unwalked state: two records differing only in
//     whether the unexported and `json:"-"` fields are populated must marshal
//     BYTE-IDENTICALLY, for the same reason.
func TestGuardReviewedMarshalersMatchTheirFieldSet(t *testing.T) {
	for typ, entry := range reviewedMarshalerTypes {
		t.Run(typ.String(), func(t *testing.T) {
			if typ == reflect.TypeOf(time.Time{}) {
				// The one entry exempt as TEXT-FREE rather than field-faithful:
				// it renders as a quoted timestamp, not an object of its fields,
				// and the walk plants nothing in it. There is no field-set claim
				// to execute.
				return
			}
			probe := &sentinelFiller{}
			probe.fill(reflect.New(typ).Elem(), "", 0, false)
			if len(probe.unsupported) > 0 || len(probe.tooDeep) > 0 {
				t.Fatalf("the walk could not plant %s: unsupported=%v tooDeep=%v",
					typ, probe.unsupported, probe.tooDeep)
			}
			if len(probe.planted) == 0 {
				t.Fatalf("planted nothing in %s, so this contract check would pass vacuously", typ)
			}

			var added, changed, dropped []string
			note := func(into *[]string, format string, args ...any) {
				*into = append(*into, fmt.Sprintf(format, args...))
			}
			diff := func(where string, fixture marshalerFixture, pinExtras bool) {
				customMembers := decodeMembers(t, typ.String()+".MarshalJSON", fixture.custom)
				baselineMembers := decodeMembers(t, "the plain twin of "+typ.String(), fixture.baseline)
				for name, got := range customMembers {
					want, isField := baselineMembers[name]
					switch {
					case !isField:
						declared, ok := entry.extra[name]
						if !ok {
							note(&added, "%s: %s = %s", where, name, got)
						} else if pinExtras && string(got) != declared {
							note(&changed, "%s: %s emits %s, the entry declares %s", where, name, got, declared)
						}
					case string(got) != string(want):
						note(&changed, "%s: %s emits %s, the field holds %s", where, name, got, want)
					}
				}
				for name := range baselineMembers {
					if _, ok := customMembers[name]; !ok {
						note(&dropped, "%s: %s", where, name)
					}
				}
				for name := range entry.extra {
					if _, ok := customMembers[name]; !ok {
						note(&dropped, "%s: %s (declared as an extra, not emitted)", where, name)
					}
				}
				for _, form := range fixture.unwalked {
					if strings.Contains(string(fixture.custom), form) {
						note(&added, "%s: text planted where encoding/json would never reach it "+
							"(an unexported or json:\"-\" field): %s", where, form)
					}
				}
			}

			// The state the walk leaves behind is the one the declared extra
			// VALUES describe, so it is the only place they are pinned.
			diff("unseeded", newMarshalerFixture(t, typ, 0, nil, 0, true), true)

			seq := 1000
			for pattern, spread := range guardScalarPatterns {
				for _, state := range guardMarshalerStates {
					where := fmt.Sprintf("pattern %d state %d", pattern, state)
					first := newMarshalerFixture(t, typ, seq, spread, state, true)
					diff(where, first, false)

					// Same state, DIFFERENT planted text: an extra derived from
					// any of it differs, however it was encoded on the way out.
					second := newMarshalerFixture(t, typ, seq+500, spread, state, true)
					firstMembers := decodeMembers(t, typ.String()+".MarshalJSON", first.custom)
					secondMembers := decodeMembers(t, typ.String()+".MarshalJSON", second.custom)
					for name := range entry.extra {
						if string(firstMembers[name]) != string(secondMembers[name]) {
							note(&changed, "%s: %s emits %s for one record and %s for another whose "+
								"only difference is the text planted in it — the entry claims it is "+
								"derived from the enum beside it, never from user text",
								where, name, firstMembers[name], secondMembers[name])
						}
					}

					// Same state, same planted text, unwalked state EMPTY: the
					// output must not move at all.
					bare := newMarshalerFixture(t, typ, seq, spread, state, false)
					if !bytes.Equal(first.custom, bare.custom) {
						note(&added, "%s: the output depends on state encoding/json cannot reach — "+
							"with the unexported and json:\"-\" fields populated it emits %s, with "+
							"them empty %s", where, first.custom, bare.custom)
					}
					seq += 1000
				}
			}

			for label, found := range map[string][]string{
				"emits member(s) the entry does not declare":        added,
				"renders member(s) differently from what it claims": changed,
				"no longer emits member(s)":                         dropped,
			} {
				if len(found) == 0 {
					continue
				}
				t.Errorf("%s is exempted as %q, but its MarshalJSON %s:\n  %s\n\n"+
					"The exemption describes code that no longer runs. Re-read the marshaler, then "+
					"update the entry or delete it — an entry that overstates what a marshaler emits "+
					"lets the walk record evidence the bundle never shows, and understates what it "+
					"adds beside it.",
					typ, entry.why, label, strings.Join(dedupeSorted(found), "\n  "))
			}
		})
	}
}

// plainTwinOf returns the same value in a generated struct type with the same
// exported fields and tags and NO methods, so encoding/json renders it by its
// own field rules instead of through the type's MarshalJSON.
//
// Unexported fields are left out: json never emits them, so their absence cannot
// change the baseline — which is exactly what makes an unexported field's text
// show up as an undeclared member above rather than as a matching one.
func plainTwinOf(t *testing.T, value reflect.Value) reflect.Value {
	t.Helper()
	typ := value.Type()
	var fields []reflect.StructField
	var sources []int
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.PkgPath != "" {
			continue
		}
		fields = append(fields, reflect.StructField{Name: f.Name, Type: f.Type, Tag: f.Tag, Anonymous: f.Anonymous})
		sources = append(sources, i)
	}
	twin := reflect.New(reflect.StructOf(fields)).Elem()
	for i, src := range sources {
		twin.Field(i).Set(value.Field(src))
	}
	if rendersItself(twin.Type()) {
		t.Fatalf("the plain twin of %s still renders itself, so it is no baseline", typ)
	}
	return twin
}

// plantUnwalkedState plants markers into every field encoding/json would never
// emit on its own, at ANY depth — UNEXPORTED ones, which reflect will not set
// and which it reaches through the pointer their address gives, and exported
// ones tagged `json:"-"`, which mustVisit skips.
//
// Both are invisible to the plain twin, so anything the custom marshaler makes
// of them shows up as an added member. The `json:"-"` half matters because that
// tag constrains the DEFAULT encoder and says nothing about a custom parent: a
// sensitive field can be tagged out of the ordinary document and still be
// exposed under another member name (#3592 review).
//
// It RECURSES rather than reading direct fields only. An exported nested struct
// with an unexported string inside is skipped by the ordinary walk and reachable
// by the parent marshaler, so a pass that stopped at the top would hand it an
// empty fixture (#3592 review).
//
// It runs after the exported checks, on the same filler, so its markers continue
// the same sequence and cannot collide with theirs.
func plantUnwalkedState(filler *sentinelFiller, value reflect.Value) []string {
	first := len(filler.planted)
	plantUnwalkedInto(filler, value, "", 0)
	var forms []string
	for _, planted := range filler.planted[first:] {
		forms = append(forms, planted.forms...)
	}
	return forms
}

func plantUnwalkedInto(filler *sentinelFiller, value reflect.Value, path string, depth int) {
	if depth > guardMaxDepth {
		return
	}
	switch value.Kind() {
	case reflect.Pointer:
		if !value.IsNil() {
			plantUnwalkedInto(filler, value.Elem(), path, depth+1)
		}
	case reflect.Slice, reflect.Array:
		if elem := value.Type().Elem().Kind(); elem == reflect.Uint8 || elem == reflect.Int32 {
			return
		}
		for i := 0; i < value.Len(); i++ {
			plantUnwalkedInto(filler, value.Index(i), path+"[]", depth+1)
		}
	case reflect.Struct:
		if value.Type() == reflect.TypeOf(time.Time{}) {
			return
		}
		typ := value.Type()
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			at := join(path, field.Name)
			switch {
			case field.PkgPath != "":
				hidden := value.Field(i)
				settable := reflect.NewAt(hidden.Type(), hidden.Addr().UnsafePointer()).Elem()
				filler.fill(settable, at, 0, false)
				plantUnwalkedInto(filler, settable, at, depth+1)
			case field.Tag.Get("json") == "-":
				filler.fill(value.Field(i), at, 0, false)
				plantUnwalkedInto(filler, value.Field(i), at, depth+1)
			default:
				plantUnwalkedInto(filler, value.Field(i), at, depth+1)
			}
		}
	}
}

// marshalReviewed marshals through the form that actually implements the
// reviewed interface. A MarshalJSON declared on *T is not invoked for a
// non-addressable T, so marshalling the value would compare json's DEFAULT
// encoding against the plain twin and agree every time, whatever *T emits
// (#3592 review). fill already accepts a reviewed type through a pointer, so
// this is a shape the walk admits.
func marshalReviewed(t *testing.T, typ reflect.Type, value reflect.Value) []byte {
	t.Helper()
	target := value
	if !typ.Implements(jsonMarshalerType) && !typ.Implements(textMarshalerType) {
		target = value.Addr()
	}
	return marshalOrFail(t, typ.String()+".MarshalJSON", target)
}

// decodeMembers reads a JSON object's top-level members and REFUSES a repeated
// name.
//
// Unmarshalling into a map keeps one occurrence and silently drops the rest, so
// a marshaler that emits a member twice — which this repo's own
// AppendJSONMember makes easy to do by splicing — could carry user text in the
// first and the expected value in the second, match the baseline, and still ship
// the text in the bundle that consumers actually read (#3592 review).
func decodeMembers(t *testing.T, what string, doc []byte) map[string]json.RawMessage {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(doc))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("%s did not render a JSON object (%v): %s", what, err, doc)
	}
	members := map[string]json.RawMessage{}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			t.Fatalf("%s: reading a member name failed: %v", what, err)
		}
		name, ok := key.(string)
		if !ok {
			t.Fatalf("%s: member name %v is not a string", what, key)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			t.Fatalf("%s: reading the value of %q failed: %v", what, name, err)
		}
		if previous, dup := members[name]; dup {
			t.Errorf("%s emits the member %q more than once — %s, then %s.\n\n"+
				"Only the last survives a map decode, so every check below reads one of them "+
				"and never sees the other, while the bundle carries both and different readers "+
				"pick different ones.", what, name, previous, raw)
		}
		members[name] = raw
	}
	return members
}

// seedScalars sets every scalar leaf reachable from value — exported and
// unexported alike — to a value the pattern derives from the leaf's PATH and the
// state, so the record can be read somewhere other than the all-zero corner the
// walk leaves behind.
//
// Keyed by path, not by a running counter. A counter makes the value assigned to
// a field depend on how many scalars came before it, so allocating one hidden
// pointer shifts every later EXPORTED scalar — measured: the independence check
// then reported archive_report, runtime_cleanup and two bools as differing
// between two records that were supposed to differ only in hidden state, all of
// it the fixture's own doing.
//
// Byte and rune containers are stepped over: their elements are the planted text
// itself, and overwriting them would destroy the evidence the rest of the test
// searches for. Maps are stepped over too — their values were planted through
// reflect.MakeMap and are not addressable here.
func seedScalars(value reflect.Value, pattern func(index, state int) int, state int, path string) {
	switch value.Kind() {
	case reflect.Pointer:
		if !value.IsNil() {
			seedScalars(value.Elem(), pattern, state, path)
		}
	case reflect.Struct:
		if value.Type() == reflect.TypeOf(time.Time{}) {
			return
		}
		for i := 0; i < value.NumField(); i++ {
			field := value.Field(i)
			if value.Type().Field(i).PkgPath != "" {
				field = reflect.NewAt(field.Type(), field.Addr().UnsafePointer()).Elem()
			}
			seedScalars(field, pattern, state, join(path, value.Type().Field(i).Name))
		}
	case reflect.Slice, reflect.Array:
		if elem := value.Type().Elem().Kind(); elem == reflect.Uint8 || elem == reflect.Int32 {
			return
		}
		for i := 0; i < value.Len(); i++ {
			seedScalars(value.Index(i), pattern, state, path+"[]")
		}
	case reflect.Bool:
		value.SetBool(pattern(pathIndex(path), state)%2 == 1)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value.SetInt(int64(pattern(pathIndex(path), state)))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		value.SetUint(uint64(pattern(pathIndex(path), state)))
	case reflect.Float32, reflect.Float64:
		value.SetFloat(float64(pattern(pathIndex(path), state)))
	}
}

// pathIndex is a small stable hash of a field path, used to spread neighbouring
// scalars apart. Any stable function of the path works; this one is chosen for
// being obvious rather than for its distribution.
func pathIndex(path string) int {
	sum := 0
	for i := 0; i < len(path); i++ {
		sum = sum*31 + int(path[i])
	}
	if sum < 0 {
		sum = -sum
	}
	return sum
}

func marshalOrFail(t *testing.T, what string, value reflect.Value) []byte {
	t.Helper()
	out, err := json.Marshal(value.Interface())
	if err != nil {
		t.Fatalf("%s failed: %v", what, err)
	}
	return out
}

// TestGuardInstanceDataMarshalerIgnoresHiddenStateWhenArchived reads the one
// combination the generic patterns cannot promise to hit.
//
// Those patterns spread scalars by a hash of their path, which covers many
// combinations and guarantees none: a marshaler may gate on any predicate over
// the record, and no bounded set of fixtures rules that out. This one is named
// because production names it — session/instance_data.go projects the hidden tab
// roster exactly when the record is archived, so a marshaler that emitted it
// would do so in this state and no other (#3592 review).
//
// Both spellings of archived are read: the composed Status with its matching
// Liveness, and the legacy row a pre-#1195 daemon wrote, whose Liveness is unset
// and whose archived-ness lives in the Status integer alone.
func TestGuardInstanceDataMarshalerIgnoresHiddenStateWhenArchived(t *testing.T) {
	typ := reflect.TypeOf(session.InstanceData{})
	for _, tc := range []struct {
		name     string
		status   session.Status
		liveness session.Liveness
	}{
		{"composed", session.Archived, session.LiveArchived},
		{"legacy row", session.Archived, session.LivenessUnset},
	} {
		t.Run(tc.name, func(t *testing.T) {
			archived := func(value reflect.Value) {
				record := value.Addr().Interface().(*session.InstanceData)
				record.Status = tc.status
				record.Liveness = tc.liveness
			}
			withHidden := archivedFixture(t, typ, 7000, true, archived)
			without := archivedFixture(t, typ, 7000, false, archived)
			if !bytes.Equal(withHidden.custom, without.custom) {
				t.Errorf("session.InstanceData.MarshalJSON reads state encoding/json cannot see, "+
					"in the state production populates it:\n  with it: %s\n  without: %s",
					withHidden.custom, without.custom)
			}
		})
	}
}

// archivedFixture is newMarshalerFixture with the scalars set by hand rather
// than by a pattern.
func archivedFixture(t *testing.T, typ reflect.Type, seq int, withUnwalked bool, set func(reflect.Value)) marshalerFixture {
	t.Helper()
	filler := &sentinelFiller{seq: seq}
	value := reflect.New(typ).Elem()
	filler.fill(value, "", 0, false)
	var unwalked []string
	if withUnwalked {
		unwalked = plantUnwalkedState(filler, value)
	}
	set(value)
	return marshalerFixture{
		baseline: marshalOrFail(t, "the plain twin of "+typ.String(), plainTwinOf(t, value)),
		custom:   marshalReviewed(t, typ, value),
		unwalked: unwalked,
	}
}
