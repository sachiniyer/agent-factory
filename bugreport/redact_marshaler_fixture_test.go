package bugreport

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Probes against the FIXTURE the marshaler contract builds, rather than against
// a reviewed type.
//
// redact_marshaler_contract_test.go asks whether a reviewed type's MarshalJSON
// still does what its entry says. These ask the prior question: does the fixture
// it is asked on actually reach the state it claims to, and does the comparison
// actually check what it claims to? A fixture that reports a gate as open
// without opening it, or a diff that accepts a value it never declared, makes
// every reading above it vacuous.
//
// Each probe runs a SYNTHETIC marshaler built to exhibit exactly one defect, so
// the assertion can be watched failing against the shape it exists for. No
// reviewed type has either shape today — which is precisely why the fixture
// could be wrong about them unnoticed (#3655).

// guardBroadInterfaceRecord hides an `any`, the broadest interface there is.
//
// A context satisfies it, and is no representative OF it: production stores
// whatever it likes behind an `any`, and the parent below reads it as a typed
// payload rather than as a non-nil gate.
type guardBroadInterfaceRecord struct {
	Name   string `json:"name"`
	hidden any
}

// MarshalJSON is the defect item 1 of #3655 names: the hidden field is not a
// gate that is merely present, it is ASSERTED to a concrete type and emitted.
func (r guardBroadInterfaceRecord) MarshalJSON() ([]byte, error) {
	out := map[string]string{"name": r.Name}
	if text, ok := r.hidden.(string); ok {
		out["leaked"] = text
	}
	return json.Marshal(out)
}

// guardContextGateRecord hides the shape the representative IS for — what
// GitWorktree.hooksCtx has — so the narrowing can be held to still populating
// it.
type guardContextGateRecord struct {
	Name   string `json:"name"`
	hidden context.Context
}

// MarshalJSON reads that context as a pure GATE, which is the reading the
// representative exists to open (#3592 review).
func (r guardContextGateRecord) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{"name": r.Name, "gated": r.hidden != nil})
}

// TestGuardFixturePopulatesOnlyTheContextInterfaceShape holds populateOpaque to
// the one interface it has a value for.
//
// The old test was "does a context satisfy this interface", which `any` answers
// yes to along with every other type in the language. The fixture then recorded
// the field as POPULATED, so nothing downstream — not the gap register, not the
// hidden-state comparison — had anything to report, while the state a marshaler
// would actually find there was never constructed (#3655 item 1).
//
// unsupported is the slice unclassifiedFixtureGaps reads, so a report here is a
// named failure demanding a register entry, not a silent skip.
func TestGuardFixturePopulatesOnlyTheContextInterfaceShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		zero any
		// populated is whether the fixture is expected to have a representative
		// for this shape. Its negation is whether the shape must be REPORTED:
		// every hidden interface is one or the other, never neither.
		populated bool
	}{
		{name: "context.Context", zero: guardContextGateRecord{}, populated: true},
		{name: "any", zero: guardBroadInterfaceRecord{}, populated: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := reflect.New(reflect.TypeOf(tc.zero)).Elem()
			filler := &sentinelFiller{}
			plantUnwalkedState(filler, value, false)

			var reported []string
			for _, gap := range filler.unsupported {
				if strings.HasPrefix(gap, "hidden ") {
					reported = append(reported, gap)
				}
			}
			if got := !value.FieldByName("hidden").IsNil(); got != tc.populated {
				t.Errorf("the fixture populated the hidden %s field: %t, want %t",
					tc.name, got, tc.populated)
			}
			if got := len(reported) > 0; got == tc.populated {
				t.Errorf("the fixture reported the hidden %s field as unpopulatable: %t (%v), want %t\n\n"+
					"A hidden interface is either populated with a representative value or written "+
					"down by name. Both at once claims a gate is open that is not; neither leaves "+
					"the fixture silently empty where a marshaler reads state.",
					tc.name, got, reported, !tc.populated)
			}
		})
	}
}

// TestGuardContextIsNoPayloadForABroadInterface is why that report is the only
// signal there could be.
//
// A context planted in an `any` renders identically to leaving it nil, so every
// comparison the contract makes is satisfied — the field-set diff, the
// with/without-hidden-state reading, both of them — by a fixture that opened
// nothing. The value production would put there renders differently.
func TestGuardContextIsNoPayloadForABroadInterface(t *testing.T) {
	marshal := func(r guardBroadInterfaceRecord) []byte {
		out, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshalling the probe record failed: %v", err)
		}
		return out
	}
	empty := marshal(guardBroadInterfaceRecord{Name: "n"})
	if withContext := marshal(guardBroadInterfaceRecord{Name: "n", hidden: context.Background()}); !bytes.Equal(empty, withContext) {
		t.Fatalf("the probe no longer exhibits the defect: a context in the hidden field renders "+
			"%s, an empty one %s — they must be identical for the point to hold", withContext, empty)
	}
	if withText := marshal(guardBroadInterfaceRecord{Name: "n", hidden: "s3cret"}); bytes.Equal(empty, withText) {
		t.Fatalf("the probe no longer exhibits the defect: the marshaler must emit the payload it "+
			"asserts, and it rendered %s for a record holding one", withText)
	}
}

// guardNormalizedEmptyRecord renders an ABSENT slice member as whichever empty
// form it is built with, which is the freedom a marshaler choosing its own
// normalization actually has.
type guardNormalizedEmptyRecord struct {
	Skipped []string `json:"skipped"`
	// as is the empty form MarshalJSON renders the absent member as. Unexported,
	// so plainTwinOf leaves it out and the baseline stays `null` — the same
	// shape ArchiveRetainedTree.skipped has under the sparse reading.
	as string
}

func (r guardNormalizedEmptyRecord) MarshalJSON() ([]byte, error) {
	if r.Skipped == nil {
		return []byte(`{"skipped":` + r.as + `}`), nil
	}
	return json.Marshal(map[string][]string{"skipped": r.Skipped})
}

// TestGuardNormalizedEmptyKeepsTheDeclaredForm holds the one permitted
// difference to the form the entry declares.
//
// normalizedEmpty used to accept `[]` and `{}` for any member the entry named,
// without retaining which the member is. The single declaration is a slice, so a
// regression rendering it as an object passed the sparse reading while changing
// the member's public JSON type — and since the marshaler chooses that form
// itself, it is a one-character edit away (#3655 item 6).
func TestGuardNormalizedEmptyKeepsTheDeclaredForm(t *testing.T) {
	typ := reflect.TypeOf(guardNormalizedEmptyRecord{})
	entry := reviewedMarshaler{
		why:             "synthetic: declares that an absent skipped normalizes to the empty ARRAY",
		normalizesEmpty: map[string]string{"skipped": "[]"},
	}
	for _, tc := range []struct {
		emits       string
		wantFinding bool
	}{
		{emits: "[]", wantFinding: false},
		{emits: "{}", wantFinding: true},
	} {
		t.Run("renders the absent member as "+tc.emits, func(t *testing.T) {
			value := reflect.ValueOf(&guardNormalizedEmptyRecord{as: tc.emits}).Elem()
			fixture := marshalerFixture{
				baseline: marshalOrFail(t, "the plain twin of "+typ.String(), plainTwinOf(t, value)),
				custom:   marshalReviewed(t, typ, value),
			}
			if want := `{"skipped":null}`; string(fixture.baseline) != want {
				t.Fatalf("the probe's baseline is %s, want %s — the point of the reading is that "+
					"the default encoder renders the ABSENT member as null", fixture.baseline, want)
			}

			report := &marshalerReport{}
			diffFixture(t, report, typ, entry, "nil slice", fixture, false)
			found := append(append(append([]string(nil), report.added...), report.changed...), report.dropped...)
			if got := len(found) > 0; got != tc.wantFinding {
				t.Errorf("the contract reported a difference for a marshaler emitting %s: %t (%v), want %t\n\n"+
					"The entry declares that member normalizes to []. The OTHER empty collection is a "+
					"different public JSON type, and accepting it makes the declaration say nothing.",
					tc.emits, got, found, tc.wantFinding)
			}
		})
	}
}

// TestGuardNormalizedEmptyFormsMatchTheirFields keeps each declared form tied to
// the member's Go kind, so the declaration cannot drift from the type it
// describes.
//
// The form is hand-written, and a hand-written claim about a member is exactly
// what the rest of this contract exists to execute rather than believe: a
// declaration of `{}` against a slice field would otherwise pin the contract to
// the wrong shape just as firmly as accepting either did.
func TestGuardNormalizedEmptyFormsMatchTheirFields(t *testing.T) {
	for typ, entry := range reviewedMarshalerTypes {
		for name, form := range entry.normalizesEmpty {
			field, found := jsonMemberField(typ, name)
			if !found {
				t.Errorf("%s declares that %q normalizes to %s, but it emits no member by that name.\n\n"+
					"Either the member is gone and the declaration is stale, or it is promoted out of "+
					"an embedding and jsonMemberField needs to learn the shape — a declaration that "+
					"names nothing checks nothing.", typ, name, form)
				continue
			}
			want, ok := emptyCollectionForm(field.Type)
			if !ok {
				t.Errorf("%s declares that %q normalizes to %s, but %s has no empty COLLECTION form: "+
					"json does not render it as [] or {} however empty it is.",
					typ, name, form, field.Type)
				continue
			}
			if form != want {
				t.Errorf("%s declares that %q normalizes to %s, but %s renders empty as %s.\n\n"+
					"The declared form is the member's public JSON type. Fix the declaration, or the "+
					"marshaler is changing the type and the exemption no longer describes it.",
					typ, name, form, field.Type, want)
			}
		}
	}
}

// jsonMemberField finds the field a top-level JSON member name comes from.
//
// Direct fields only: no reviewed type promotes a normalized member out of an
// embedding, and a lookup that guessed at the promotion rules would be a second
// implementation of what plainTwinOf already has to get right. A name it cannot
// find is reported rather than skipped.
func jsonMemberField(typ reflect.Type, name string) (reflect.StructField, bool) {
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.PkgPath != "" || field.Tag.Get("json") == "-" {
			continue
		}
		if jsonMemberName(field) == name {
			return field, true
		}
	}
	return reflect.StructField{}, false
}

// emptyCollectionForm is the JSON an EMPTY value of this type renders as, for
// the types that have such a form at all.
//
// A byte container does not: json renders it as a base64 STRING, so its empty
// form is `""` and normalizing it to a collection would be a type change rather
// than a normalization.
func emptyCollectionForm(typ reflect.Type) (string, bool) {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Slice, reflect.Array:
		if typ.Elem().Kind() == reflect.Uint8 {
			return "", false
		}
		return "[]", true
	case reflect.Map:
		return "{}", true
	}
	return "", false
}
