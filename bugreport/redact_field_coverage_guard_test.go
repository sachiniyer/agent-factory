package bugreport

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
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

// TestGuardReviewedMarshalersMatchTheirFieldSet turns every entry of
// reviewedMarshalerTypes from a point-in-time reading into a checked contract.
//
// An entry claims the type's MarshalJSON emits the same exported fields this
// walk plants into, plus the extras it declares. Nothing tied that claim to the
// code. A marshaler that later base64-encodes a field keeps its exemption while
// the walk records evidence the document no longer shows; one that starts
// emitting text out of an UNEXPORTED field — which the walk can never plant into
// — keeps it while shipping something the guard has never seen (#3592 review).
//
// So the claim is executed, by DIFF. A plain twin of the type is built with
// reflect.StructOf: the same exported fields, same tags, no method set to
// intercept — so encoding/json itself produces the baseline the custom output is
// compared against. Every member must match value for value, and any member the
// marshaler adds must be declared in the entry.
func TestGuardReviewedMarshalersMatchTheirFieldSet(t *testing.T) {
	for typ, entry := range reviewedMarshalerTypes {
		t.Run(typ.String(), func(t *testing.T) {
			if typ == reflect.TypeOf(time.Time{}) {
				// The one entry exempt as TEXT-FREE rather than field-faithful:
				// it renders as a quoted timestamp, not an object of its fields,
				// and the walk plants nothing in it. There is no field-set claim
				// to diff.
				return
			}
			value := reflect.New(typ).Elem()
			filler := &sentinelFiller{}
			filler.fill(value, "", 0, false)
			if len(filler.unsupported) > 0 || len(filler.tooDeep) > 0 {
				t.Fatalf("the walk could not plant %s: unsupported=%v tooDeep=%v",
					typ, filler.unsupported, filler.tooDeep)
			}
			if len(filler.planted) == 0 {
				t.Fatalf("planted nothing in %s, so this contract check would pass vacuously", typ)
			}

			custom := marshalOrFail(t, typ.String()+".MarshalJSON", value)
			baseline := marshalOrFail(t, "the plain twin of "+typ.String(), plainTwinOf(t, value))

			var customMembers, baselineMembers map[string]json.RawMessage
			if err := json.Unmarshal(custom, &customMembers); err != nil {
				t.Fatalf("%s did not render a JSON object: %v\n%s", typ, err, custom)
			}
			if err := json.Unmarshal(baseline, &baselineMembers); err != nil {
				t.Fatalf("the plain twin of %s did not render a JSON object: %v", typ, err)
			}

			declared := make(map[string]struct{}, len(entry.extra))
			for _, name := range entry.extra {
				declared[name] = struct{}{}
			}
			var added, changed, dropped []string
			for name, got := range customMembers {
				want, isField := baselineMembers[name]
				switch {
				case !isField:
					if _, ok := declared[name]; !ok {
						added = append(added, fmt.Sprintf("%s = %s", name, got))
					}
				case string(got) != string(want):
					changed = append(changed, fmt.Sprintf("%s: emits %s, the field holds %s", name, got, want))
				}
			}
			for name := range baselineMembers {
				if _, ok := customMembers[name]; !ok {
					dropped = append(dropped, name)
				}
			}
			for _, name := range entry.extra {
				if _, ok := customMembers[name]; !ok {
					dropped = append(dropped, name+" (declared as an extra, not emitted)")
				}
			}

			for label, found := range map[string][]string{
				"emits member(s) the entry does not declare":            added,
				"renders field(s) differently from the value they hold": changed,
				"no longer emits member(s)":                             dropped,
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

func marshalOrFail(t *testing.T, what string, value reflect.Value) []byte {
	t.Helper()
	out, err := json.Marshal(value.Interface())
	if err != nil {
		t.Fatalf("%s failed: %v", what, err)
	}
	return out
}
