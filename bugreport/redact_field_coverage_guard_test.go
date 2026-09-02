package bugreport

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
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
			filler.fill(record, "", 0)
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
	filler.fill(record, "", 0)
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

// TestGuardRefusesAShortArray covers the other end of the same property: an
// array too small to hold one complete unit could only be planted with a prefix
// every field shares, so it must be reported rather than probed.
func TestGuardRefusesAShortArray(t *testing.T) {
	var probe struct {
		Digest [guardMinContainer - 1]byte
	}
	filler := &sentinelFiller{}
	filler.fill(reflect.ValueOf(&probe).Elem(), "", 0)
	if len(filler.planted) != 0 {
		t.Errorf("planted %d marker(s) in an array too short to name its field", len(filler.planted))
	}
	if len(filler.unsupported) != 1 || !strings.Contains(filler.unsupported[0], "Digest") {
		t.Errorf("unsupported = %v, want one entry naming Digest", filler.unsupported)
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
			filler.fill(reflect.ValueOf(tc.probe).Elem(), "", 0)
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
