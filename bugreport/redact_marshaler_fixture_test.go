package bugreport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
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
		// The normalization DISAPPEARING is the third outcome, and the one a
		// diff keyed on "the two sides differ" cannot see: both sides render
		// null, so the member matches its field exactly and the declaration
		// quietly describes nothing (#3686 review). One word inside
		// cloneArchiveSkippedEntries — returning its argument rather than a
		// make()d slice — is the whole regression.
		{emits: "null", wantFinding: true},
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
					"The entry declares that member normalizes to []. The other empty collection is a "+
					"different public JSON type, and null is the normalization having gone away — "+
					"accepting either makes the declaration say nothing.",
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

// reviewAs registers a synthetic type as a reviewed marshaler for the duration
// of one probe.
//
// fill refuses a self-rendering type it has not been told about, and refuses it
// at the ROOT before planting anything — rightly, since a marshaler it has not
// read can emit text out of state it never planted. Every probe below is such a
// type, so without this the fixture built for one is empty and the reading is
// vacuous. The entry registered is the entry the probe is then read against, so
// nothing is claimed here that the assertion does not also use.
//
// The tests in this package do not run in parallel, and this is why they must
// not start: reviewedMarshalerTypes is shared, so two probes registered at once
// would show each other's synthetic types to the contract — which iterates the
// whole map — and to the gap register.
func reviewAs(t *testing.T, typ reflect.Type, entry reviewedMarshaler) {
	t.Helper()
	if _, taken := reviewedMarshalerTypes[typ]; taken {
		t.Fatalf("%s is already a reviewed marshaler; registering the probe would overwrite it", typ)
	}
	reviewedMarshalerTypes[typ] = entry
	t.Cleanup(func() { delete(reviewedMarshalerTypes, typ) })
}

// guardStructuralGateRecord reaches for hidden state only in the two structural
// shapes the hidden-state comparison never used to be built for: an optional
// pointer ABSENT, and a collection allocated-but-EMPTY.
//
// The member it moves is one the entry DECLARES, which is what makes the defect
// invisible to everything but the byte-identity reading — the member diff
// accepts a declared extra by name whatever it holds, and the text-independence
// reading sees no movement because every marker is the same length.
type guardStructuralGateRecord struct {
	Name     string   `json:"name"`
	Optional *string  `json:"optional"`
	Items    []string `json:"items"`
	hidden   string
}

func (r guardStructuralGateRecord) MarshalJSON() ([]byte, error) {
	tag := "constant"
	if r.Optional == nil || (r.Items != nil && len(r.Items) == 0) {
		tag = fmt.Sprintf("hidden:%d", len(r.hidden))
	}
	type wire guardStructuralGateRecord
	return appendGuardTag(json.Marshal(wire(r)))(tag)
}

// guardNamedStateGateRecord opens the same gate only when a hand-named scalar
// state and a structural shape COINCIDE.
//
// Kind is set to a value the generic patterns cannot produce — they seed 0
// through 8 — so no reading of the generic states opens it, and only a named
// reading that also builds the structural modes can (#3655 item 5).
type guardNamedStateGateRecord struct {
	Kind     int     `json:"kind"`
	Optional *string `json:"optional"`
	hidden   string
}

// guardNamedKind is that value.
const guardNamedKind = 42

func (r guardNamedStateGateRecord) MarshalJSON() ([]byte, error) {
	tag := "constant"
	if r.Kind == guardNamedKind && r.Optional == nil {
		tag = fmt.Sprintf("hidden:%d", len(r.hidden))
	}
	type wire guardNamedStateGateRecord
	return appendGuardTag(json.Marshal(wire(r)))(tag)
}

// guardHiddenScalarGateRecord is gated on a hidden BOOL and nothing else.
//
// Nothing plants a hidden scalar — the unwalked pass has no marker for one — so
// the scalar seeder is the only thing that ever sets it, and seeding it on both
// sides of the comparison left the gate in the same position in both records
// (#3655 item 9).
type guardHiddenScalarGateRecord struct {
	Name string `json:"name"`
	gate bool
}

func (r guardHiddenScalarGateRecord) MarshalJSON() ([]byte, error) {
	tag := "shut"
	if r.gate {
		tag = "open"
	}
	type wire guardHiddenScalarGateRecord
	return appendGuardTag(json.Marshal(wire(r)))(tag)
}

// appendGuardTag splices a `tag` member onto a marshalled object, which is how
// each probe above adds the DECLARED extra whose value the reading has to watch.
// Curried so a marshaler can pass its error through in one line.
func appendGuardTag(out []byte, err error) func(string) ([]byte, error) {
	return func(tag string) ([]byte, error) {
		if err != nil {
			return nil, err
		}
		return append(out[:len(out)-1], []byte(fmt.Sprintf(`,"tag":%q}`, tag))...), nil
	}
}

// guardTagEntry is the exemption each of those probes is read against: `tag` is
// declared, so only the independence readings can object to it.
var guardTagEntry = reviewedMarshaler{
	why:   "synthetic: declares a constant tag beside the fields, derived from nothing",
	extra: map[string]string{"tag": `"constant"`},
}

// TestGuardHiddenStateIndependenceReadsEveryStructuralMode holds the with/without
// comparison to every shape the fixture builds.
//
// It used to be built for the populated and sparse shapes only, so a marshaler
// that reached for unwalked state exactly when an optional pointer was absent —
// or when a collection was allocated-but-empty — kept that dependence in two of
// the four modes the contract already constructs (#3655 item 2).
//
// The two shapes that do NOT open the gate are the control: the same marshaler,
// read in the two modes that always had the comparison, must stay silent. A
// probe that reported everywhere would pass for the wrong reason.
func TestGuardHiddenStateIndependenceReadsEveryStructuralMode(t *testing.T) {
	typ := reflect.TypeOf(guardStructuralGateRecord{})
	reviewAs(t, typ, guardTagEntry)
	report := &marshalerReport{}
	readStructuralModes(t, report, typ, guardTagEntry, "probe",
		fixtureSpec{seq: 100, pattern: guardScalarPatterns[1], state: 3, withUnwalked: true})
	assertModesReported(t, report, []string{" (nil pointers)", " (empty, not nil)"}, []string{" (sparse)"})
}

// TestGuardNamedStateReadingCoversEveryStructuralMode is the same property for a
// reading that names its own state.
//
// The archived reading built the fully populated shape and nothing else, so a
// marshaler acting on a legacy archived row whose Tabs are nil — the shape a
// pre-#1195 row actually has — was never read at all (#3655 item 5). The gate
// here needs BOTH the named state and the structural shape, so nothing but a
// named reading that builds the modes can open it.
func TestGuardNamedStateReadingCoversEveryStructuralMode(t *testing.T) {
	typ := reflect.TypeOf(guardNamedStateGateRecord{})
	reviewAs(t, typ, guardTagEntry)
	report := &marshalerReport{}
	readStructuralModes(t, report, typ, guardTagEntry, "probe", fixtureSpec{
		seq: 200, pattern: guardEveryScalarSet, state: 1, withUnwalked: true,
		override: func(value reflect.Value) {
			value.Addr().Interface().(*guardNamedStateGateRecord).Kind = guardNamedKind
		},
	})
	assertModesReported(t, report, []string{" (nil pointers)"}, []string{" (sparse)", " (empty, not nil)"})

	// Without the override the named state never occurs, so the generic
	// readings cannot be what caught it.
	generic := &marshalerReport{}
	readStructuralModes(t, generic, typ, guardTagEntry, "probe",
		fixtureSpec{seq: 300, pattern: guardScalarPatterns[1], state: 3, withUnwalked: true})
	if found := allFindings(generic); len(found) > 0 {
		t.Errorf("the generic states reported %v, and the probe's gate needs Kind = %d, which "+
			"they never seed — so the named reading is not what this test is measuring",
			found, guardNamedKind)
	}
}

// TestGuardSeedsHiddenScalarsOnlyWithHiddenState holds the scalar seeder to the
// fixture's own withUnwalked.
//
// Seeding hidden scalars unconditionally put the same value on both sides of the
// comparison whose entire job is to differ, so a marshaler gated on a hidden
// bool or numeric emitted identical bytes with hidden state populated and with
// it empty (#3655 item 9). Read across every state, because the gate is open
// only where the pattern makes it so.
func TestGuardSeedsHiddenScalarsOnlyWithHiddenState(t *testing.T) {
	typ := reflect.TypeOf(guardHiddenScalarGateRecord{})
	reviewAs(t, typ, guardTagEntry)
	var reported int
	seq := 400
	for _, spread := range guardScalarPatterns {
		for _, state := range guardMarshalerStates {
			report := &marshalerReport{}
			readStructuralModes(t, report, typ, guardTagEntry, "probe",
				fixtureSpec{seq: seq, pattern: spread, state: state, withUnwalked: true})
			reported += len(allFindings(report))
			seq += 100
		}
	}
	if reported == 0 {
		t.Errorf("no reading objected to a marshaler gated on a HIDDEN bool, across %d states.\n\n"+
			"Nothing but the seeder ever sets one, so seeding it in the control record too "+
			"leaves the two records identical in the only state the comparison exists to read.",
			len(guardScalarPatterns)*len(guardMarshalerStates))
	}
}

// guardWalkedTimestampRecord transforms an ordinary member when a walked
// timestamp is set — the exported one, and the optional pointer beside it.
type guardWalkedTimestampRecord struct {
	Name      string     `json:"name"`
	CreatedAt time.Time  `json:"created_at"`
	Optional  *time.Time `json:"optional"`
}

func (r guardWalkedTimestampRecord) MarshalJSON() ([]byte, error) {
	type wire guardWalkedTimestampRecord
	out := wire(r)
	if !r.CreatedAt.IsZero() {
		out.Name = "created:" + out.Name
	}
	if r.Optional != nil {
		out.Name = "optional:" + out.Name
	}
	return json.Marshal(out)
}

// TestGuardSeedsWalkedTimestamps pins both halves of #3655 item 13.
//
// fill exempts time.Time as text-free and returns at one BEFORE its pointer arm
// allocates, and the seeder used to step over it — so every fixture carried its
// exported timestamps at the zero instant and its optional ones absent. A
// marshaler that adds or transforms text only when a timestamp is SET had that
// branch shut in every reading. InstanceData alone has 26 such leaves.
func TestGuardSeedsWalkedTimestamps(t *testing.T) {
	typ := reflect.TypeOf(guardWalkedTimestampRecord{})
	entry := reviewedMarshaler{why: "synthetic: claims to emit exactly its fields"}
	reviewAs(t, typ, entry)
	report := &marshalerReport{}
	readStructuralModes(t, report, typ, entry, "probe",
		fixtureSpec{seq: 900, pattern: guardScalarPatterns[1], state: 3, withUnwalked: true})

	found := allFindings(report)
	for _, want := range []string{"created:", "optional:"} {
		if !containsAny(found, want) {
			t.Errorf("no reading objected to a member transformed only while a timestamp is set "+
				"(%q missing from %v).\n\n"+
				"An exported timestamp seeded to a non-zero instant is what opens that branch, and "+
				"an optional one has to be ALLOCATED before it can be read as present.", want, found)
		}
	}
	// The nil-pointers mode is the control: it clears the optional again, so the
	// absent reading is not lost to the allocation.
	if !containsAny(found, "(nil pointers): name emits \"created:") {
		t.Errorf("the nil-pointers mode did not read the optional timestamp as ABSENT: %v\n\n"+
			"Allocating it in the seeder must not take the absent branch away — the structural "+
			"mode is what keeps both readings.", found)
	}
}

// guardWipingText is a named string whose POINTER-receiver marshaler renders the
// value and then clears it: the wipe-after-read shape a secret-holding type
// plausibly has.
type guardWipingText string

func (t *guardWipingText) MarshalJSON() ([]byte, error) {
	out, err := json.Marshal(string(*t))
	*t = ""
	return out, err
}

// guardSharedStorageRecord holds one through a pointer, which a plain twin
// SHARES — the twin copies the pointer, not the string behind it.
type guardSharedStorageRecord struct {
	Name string           `json:"name"`
	Wipe *guardWipingText `json:"wipe"`
}

func (r guardSharedStorageRecord) MarshalJSON() ([]byte, error) {
	type wire guardSharedStorageRecord
	return json.Marshal(wire(r))
}

// TestGuardFixtureBuildsIndependentRecords pins the property #3655 item 4 says
// the named reading did not have: the twin and the custom marshal must be built
// from two records, not from two readings of one.
//
// The baseline is captured first, which protects it from a marshaler that
// rewrites what it renders — but only until the NEXT call. json takes the
// address of an addressable value when it invokes a pointer-receiver marshaler,
// and a twin shares every pointer, slice and map with the record it was built
// from, so marshalling the twin reaches through to the original. The custom call
// then reads what the baseline call destroyed and the contract blames the
// marshaler for a difference the fixture made.
//
// Both constructions are run here, so the defect is exhibited rather than
// described: one record reports, two records do not.
func TestGuardFixtureBuildsIndependentRecords(t *testing.T) {
	typ := reflect.TypeOf(guardSharedStorageRecord{})
	entry := reviewedMarshaler{why: "synthetic: claims to emit exactly its fields"}
	reviewAs(t, typ, entry)
	spec := fixtureSpec{seq: 800, pattern: guardScalarPatterns[0], state: 2, override: func(value reflect.Value) {
		text := guardWipingText("PLANTED")
		value.Addr().Interface().(*guardSharedStorageRecord).Wipe = &text
	}}

	// One record, read twice — what archivedFixture used to do.
	shared, _ := plantedRecord(t, typ, spec)
	single := marshalerFixture{
		baseline: marshalOrFail(t, "the plain twin of "+typ.String(), plainTwinOf(t, shared)),
		custom:   marshalReviewed(t, typ, shared),
	}
	report := &marshalerReport{}
	diffFixture(t, report, typ, entry, "one record", single, false)
	if found := allFindings(report); len(found) == 0 {
		t.Fatalf("the probe no longer exhibits the defect: reading ONE record twice reported "+
			"nothing.\n\nbaseline %s\ncustom   %s\n\nMarshalling the twin has to reach the "+
			"original through the shared pointer, or there is nothing for two records to fix.",
			single.baseline, single.custom)
	}

	report = &marshalerReport{}
	diffFixture(t, report, typ, entry, "two records", newMarshalerFixture(t, typ, spec), false)
	if found := allFindings(report); len(found) > 0 {
		t.Errorf("two independent records still reported %v.\n\nEach marshal must read a record "+
			"the other has not touched.", found)
	}
}

// assertModesReported holds a reading to the modes it was supposed to object in,
// and to silence in the rest.
func assertModesReported(t *testing.T, report *marshalerReport, want, quiet []string) {
	t.Helper()
	found := allFindings(report)
	for _, mode := range want {
		if !containsAny(found, mode) {
			t.Errorf("no reading objected in the%s mode: %v\n\n"+
				"The probe's marshaler moves a DECLARED member with hidden state in exactly that "+
				"shape, so only the with/without comparison built for it can see the movement.",
				mode, found)
		}
	}
	for _, mode := range quiet {
		if containsAny(found, mode) {
			t.Errorf("the%s mode reported, and the probe's gate is shut there: %v\n\n"+
				"A probe that objects everywhere passes for the wrong reason.", mode, found)
		}
	}
}

func allFindings(report *marshalerReport) []string {
	return append(append(append([]string(nil), report.added...), report.changed...), report.dropped...)
}

func containsAny(found []string, want string) bool {
	for _, f := range found {
		if strings.Contains(f, want) {
			return true
		}
	}
	return false
}
