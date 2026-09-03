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

	"github.com/sachiniyer/agent-factory/session"
)

// The contract that holds reviewedMarshalerTypes to its word.
//
// Split out of redact_field_coverage_guard_test.go, which tests the WALK: this
// file tests the exemption the walk grants a self-rendering type, and the two
// grew far enough apart that one file could no longer hold both under the
// file-length limit (#1145). The seam is real — nothing here is about planting
// markers, and nothing there is about what a marshaler emits.

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
	// atomic.Pointer[string]'s payload lives behind an unsafe.Pointer the filler
	// cannot plant. What it holds — the session's hook scope prefix (#3650) — is
	// not invisible to the report as a result: ForStorage copies it onto the
	// exported Worktree.HookScopeUnitPrefix, where the field-coverage guard reaches
	// it and classifies it. archiveReportSource itself is a process-local handle
	// retained only so the projection can read the archive report, and it is
	// unexported, so no marshaler can emit anything under it either way.
	"session.InstanceData: archiveReportSource.hookScopeUnitPrefix.v (hidden unsafe.Pointer cannot be populated)": "atomic payload behind an unsafe.Pointer; the value it holds is emitted and classified via the exported Worktree.HookScopeUnitPrefix, and archiveReportSource is an unexported process-local handle no marshaler can reach (#3650)",
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
		plantUnwalkedState(filler, value, false)
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

// marshalerReport collects everything one reviewed type's contract found, so
// the per-state readings and the named archived readings report through the
// same path and with the same wording.
type marshalerReport struct {
	added   []string
	changed []string
	dropped []string
}

// diffFixture is the contract itself, applied to one fixture: the member diff
// against the plain twin, the declared extras, and the search for text planted
// where encoding/json could never reach it.
func diffFixture(t *testing.T, report *marshalerReport, typ reflect.Type, entry reviewedMarshaler, where string, fixture marshalerFixture, pinExtras bool) {
	t.Helper()
	note := func(into *[]string, format string, args ...any) {
		*into = append(*into, fmt.Sprintf(format, args...))
	}
	customMembers := decodeMembers(t, typ.String()+".MarshalJSON", fixture.custom)
	baselineMembers := decodeMembers(t, "the plain twin of "+typ.String(), fixture.baseline)
	for name, got := range customMembers {
		want, isField := baselineMembers[name]
		switch declared, normalizes := entry.normalizesEmpty[name]; {
		case !isField:
			declared, ok := entry.extra[name]
			if !ok {
				note(&report.added, "%s: %s = %s", where, name, got)
			} else if pinExtras && string(got) != declared {
				note(&report.changed, "%s: %s emits %s, the entry declares %s", where, name, got, declared)
			}
		case normalizes && string(want) == "null":
			// The declared normalization, read in the ONE state that exercises
			// it — the member absent, so the marshaler's own choice of empty
			// form is what is on show. It has its own arm because two of its
			// three outcomes are invisible to a diff keyed on the two sides
			// DIFFERING (#3686 review).
			if finding := normalizedEmptyFinding(declared, name, got); finding != "" {
				note(&report.changed, "%s: %s", where, finding)
			}
		case string(got) != string(want):
			note(&report.changed, "%s: %s emits %s, the field holds %s", where, name, got, want)
		}
	}
	for name := range baselineMembers {
		if _, ok := customMembers[name]; !ok {
			note(&report.dropped, "%s: %s", where, name)
		}
	}
	for name := range entry.extra {
		if _, ok := customMembers[name]; !ok {
			note(&report.dropped, "%s: %s (declared as an extra, not emitted)", where, name)
		}
	}
	for _, form := range fixture.unwalked {
		if strings.Contains(string(fixture.custom), form) {
			note(&report.added, "%s: text planted where encoding/json would never reach it "+
				"(an unexported or json:\"-\" field): %s", where, form)
		}
	}
}

// reportTo turns what was collected into failures, grouped by what went wrong.
func (r *marshalerReport) reportTo(t *testing.T, typ reflect.Type, entry reviewedMarshaler) {
	t.Helper()
	for label, found := range map[string][]string{
		"emits member(s) the entry does not declare":        r.added,
		"renders member(s) differently from what it claims": r.changed,
		"no longer emits member(s)":                         r.dropped,
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
}

// fixtureSpec is one reading of a reviewed type: which markers, which scalar
// state, and which of the structural conditions a marshaler might gate on.
type fixtureSpec struct {
	seq     int
	pattern func(index, state int) int
	state   int
	// withUnwalked populates the state encoding/json cannot reach.
	withUnwalked bool
	// closeChans reads the other meaningful channel state — production closes
	// hooksDone when hooks finish.
	closeChans bool
	// nilPointers leaves the optional pointers nil. fill allocates every
	// pointer, so without this no fixture ever reads the branch a marshaler
	// takes when an optional field is ABSENT (#3592 review).
	nilPointers bool
	// sparse empties the walked strings and collections, which fill otherwise
	// always populates — the other ordinary production state no fixture read.
	sparse bool
	// emptyNotNil makes those collections allocated-but-empty instead of nil.
	// A marshaler can tell the two apart, and reflect.Zero only gives the nil
	// one (#3592 review).
	emptyNotNil bool
	// override sets the state a reading names by HAND, after the scalar seeding
	// and before the structural modes are applied.
	//
	// It is what lets a named reading — the archived pair production is known to
	// populate — run through the same builder as every generic one, so it gets
	// two independent records (#3655 item 4) and every structural mode
	// (#3655 item 5) without a second, diverging fixture path.
	override func(reflect.Value)
}

// newMarshalerFixture builds a FRESH record and marshals it.
//
// Fresh every time on purpose. A marshaler that mutates what it renders — hidden
// state, or a shared backing array — taints the value for every later reading,
// and the state that would have leaked then sees only what the previous call
// left behind (#3592 review).
//
// spec.seq fixes the marker sequence, so two fixtures built with the same seq
// carry IDENTICAL planted text and differ only in whatever else the spec varies.
// That is what makes the independence checks below possible.
func newMarshalerFixture(t *testing.T, typ reflect.Type, spec fixtureSpec) marshalerFixture {
	t.Helper()
	// TWO records, not one read twice. Capturing the baseline first protects it
	// from a marshaler that rewrites what it renders, but only until the NEXT
	// call: json addresses a slice element when it invokes a pointer-receiver
	// marshaler, so an element that clears itself after rendering leaves the
	// custom call reading what the baseline call destroyed (#3592 review).
	//
	// The build is deterministic — the sequence number fixes every marker, the
	// pattern fixes every scalar — so the two records are identical without a
	// deep copy having to be written and kept correct.
	forBaseline, _ := plantedRecord(t, typ, spec)
	forCustom, unwalked := plantedRecord(t, typ, spec)
	return marshalerFixture{
		baseline: marshalOrFail(t, "the plain twin of "+typ.String(), plainTwinOf(t, forBaseline)),
		custom:   marshalReviewed(t, typ, forCustom),
		unwalked: unwalked,
	}
}

// plantedRecord builds one record to a spec and returns it with the forms
// planted where encoding/json could never reach them.
func plantedRecord(t *testing.T, typ reflect.Type, spec fixtureSpec) (reflect.Value, []string) {
	t.Helper()
	filler := &sentinelFiller{seq: spec.seq}
	value := reflect.New(typ).Elem()
	filler.fill(value, "", 0, false)
	var unwalked []string
	if spec.withUnwalked {
		unwalked = plantUnwalkedState(filler, value, spec.closeChans)
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
	if spec.pattern != nil {
		scalarSeeder{pattern: spec.pattern, state: spec.state, withHidden: spec.withUnwalked}.
			seed(value, "", false)
	}
	if spec.override != nil {
		spec.override(value)
	}
	if spec.nilPointers {
		nilOptionalPointers(value, 0)
	}
	if spec.sparse {
		sparseWalkedState(value, spec.emptyNotNil, 0)
	}
	return value, unwalked
}

// guardHidesSubtree reports whether a struct field puts its whole subtree where
// encoding/json can never reach it — so the unwalked pass owns it, and every
// walked-side transform must leave it alone.
//
// ONE rule, read by all four traversals over a fixture. plantUnwalkedInto,
// sparseWalkedState and the scalar seeder each used to spell it out for
// themselves, and every spelling was a chance to disagree — which is the whole
// of #3655's second cluster: a traversal that does not make the same visibility
// decision as its siblings. It mirrors mustVisit, which is the walk's answer to
// the same question:
//
//   - `json:"-"`, and ONLY the exact tag. `json:"-,"` and `json:"-,omitempty"`
//     serialize under the literal key "-", so they are ordinary members.
//   - an unexported field is hidden, EXCEPT an anonymous struct embedding, whose
//     exported members json PROMOTES into the document. What is hidden deeper
//     inside one is caught by the recursion, not by this answer.
//   - everything else inherits whatever its parent was: an exported field inside
//     a hidden subtree is still hidden.
func guardHidesSubtree(field reflect.StructField) bool {
	if field.Tag.Get("json") == "-" {
		return true
	}
	if field.PkgPath == "" {
		return false
	}
	return !field.Anonymous || baseType(field.Type).Kind() != reflect.Struct
}

// guardSettableField returns field i of an addressable struct value in a form
// reflect will write to, reaching an UNEXPORTED field through its address the
// way the unwalked pass has to.
func guardSettableField(value reflect.Value, i int) reflect.Value {
	held := value.Field(i)
	if value.Type().Field(i).PkgPath == "" {
		return held
	}
	return reflect.NewAt(held.Type(), held.Addr().UnsafePointer()).Elem()
}

// nilOptionalPointers clears every pointer the walk allocated, so the record
// reads the way an ordinary one with nothing optional set does.
//
// fill allocates unconditionally — it has to, or it could not plant inside an
// optional subtree — which means the absent case was never read at all, and a
// marshaler that exposes hidden state only while a pointer is nil would keep
// that branch shut in every fixture (#3592 review).
func nilOptionalPointers(value reflect.Value, depth int) {
	if depth > guardMaxDepth {
		return
	}
	switch value.Kind() {
	case reflect.Pointer:
		if value.CanSet() {
			value.Set(reflect.Zero(value.Type()))
		}
	case reflect.Struct:
		if value.Type() == reflect.TypeOf(time.Time{}) {
			return
		}
		for i := 0; i < value.NumField(); i++ {
			if value.Type().Field(i).PkgPath != "" {
				continue
			}
			nilOptionalPointers(value.Field(i), depth+1)
		}
	case reflect.Slice, reflect.Array:
		if elem := value.Type().Elem().Kind(); elem == reflect.Uint8 || elem == reflect.Int32 {
			return
		}
		for i := 0; i < value.Len(); i++ {
			nilOptionalPointers(value.Index(i), depth+1)
		}
	case reflect.Map:
		// fillMap allocates the pointers inside its elements too, and a map
		// element is not addressable — so without this arm every "nil pointers"
		// fixture still carried non-nil pointers inside its maps, and a
		// marshaler gated on one being absent stayed unread (#3592 review).
		if value.IsNil() || !value.CanSet() {
			return
		}
		rebuilt := reflect.MakeMap(value.Type())
		for _, key := range value.MapKeys() {
			elem := reflect.New(value.Type().Elem()).Elem()
			elem.Set(value.MapIndex(key))
			nilOptionalPointers(elem, depth+1)
			rebuilt.SetMapIndex(key, elem)
		}
		value.Set(rebuilt)
	}
}

// sparseWalkedState empties the walked side of a record: exported strings
// cleared, exported collections emptied, exported byte and rune arrays zeroed.
//
// fill plants a marker in every string and seeds two entries into every
// collection, so a marshaler that acts only when a field is EMPTY — an ordinary
// production state — had its branch shut in every fixture (#3592 review).
//
// Which side of the record a field is on is guardHidesSubtree's answer, not one
// spelled out here: `json:"-"` and ordinary unexported fields are the hidden
// state the independence reading compares, and clearing them in both fixtures
// leaves nothing to compare — while an anonymous unexported EMBEDDING is
// descended into, since json promotes its exported members and fill plants them
// (#3592 review).
//
// The one thing that is this function's own: nil is not the only empty.
// `reflect.Zero` gives a NIL slice or map, and a marshaler can distinguish that
// from an allocated empty one, so emptyNotNil reads the other shape.
func sparseWalkedState(value reflect.Value, emptyNotNil bool, depth int) {
	if depth > guardMaxDepth {
		return
	}
	switch value.Kind() {
	case reflect.Pointer:
		if !value.IsNil() {
			sparseWalkedState(value.Elem(), emptyNotNil, depth+1)
		}
	case reflect.String:
		if value.CanSet() {
			value.SetString("")
		}
	case reflect.Slice:
		if !value.CanSet() {
			return
		}
		if emptyNotNil {
			value.Set(reflect.MakeSlice(value.Type(), 0, 0))
			return
		}
		value.Set(reflect.Zero(value.Type()))
	case reflect.Map:
		if !value.CanSet() {
			return
		}
		if emptyNotNil {
			value.Set(reflect.MakeMap(value.Type()))
			return
		}
		value.Set(reflect.Zero(value.Type()))
	case reflect.Array:
		// A byte or rune array holds planted text, and json emits it whether or
		// not it is zero — so the all-zero array is a state a marshaler can act
		// on, and fill never leaves one (#3592 review).
		if elem := value.Type().Elem().Kind(); elem == reflect.Uint8 || elem == reflect.Int32 {
			if value.CanSet() {
				value.Set(reflect.Zero(value.Type()))
			}
			return
		}
		for i := 0; i < value.Len(); i++ {
			sparseWalkedState(value.Index(i), emptyNotNil, depth+1)
		}
	case reflect.Struct:
		if value.Type() == reflect.TypeOf(time.Time{}) {
			return
		}
		for i := 0; i < value.NumField(); i++ {
			if guardHidesSubtree(value.Type().Field(i)) {
				continue
			}
			sparseWalkedState(guardSettableField(value, i), emptyNotNil, depth+1)
		}
	}
}

// structuralFixtureModes are the shapes an ordinary production record takes
// beside the fully populated one fill leaves behind.
//
// fill allocates every pointer and plants in every string and collection, so
// until these existed no fixture ever read the branch a marshaler takes when an
// optional field is ABSENT or a collection is EMPTY — both of them ordinary
// states (#3592 review). nil and allocated-but-empty are counted separately
// because a marshaler can tell them apart.
func structuralFixtureModes(base fixtureSpec) map[string]fixtureSpec {
	modes := map[string]fixtureSpec{"": base}
	for label, adjust := range map[string]func(*fixtureSpec){
		" (nil pointers)": func(spec *fixtureSpec) { spec.nilPointers = true },
		" (sparse)":       func(spec *fixtureSpec) { spec.sparse = true },
		" (empty, not nil)": func(spec *fixtureSpec) {
			spec.sparse, spec.emptyNotNil = true, true
		},
	} {
		mode := base
		adjust(&mode)
		modes[label] = mode
	}
	return modes
}

// readStructuralModes runs the whole per-record contract over one state, in
// every structural mode: the member diff, the text-independence reading, and the
// hidden-state independence reading.
//
// All three in every mode, which is #3655 item 2. The hidden-state pair used to
// be built for the populated and sparse shapes alone, so a marshaler that
// reached for unwalked state exactly when an optional pointer was absent — or
// when a collection was allocated-but-empty — was never read in the two modes
// that construct those shapes. The member diff DOES run there, so what escaped
// was specifically a member the entry already declares whose VALUE moves with
// hidden state: the diff accepts it by name, and only the byte-identity reading
// can see it move.
//
// Every fixture here is built to the same seq, so the records behind the two
// comparisons differ in exactly one thing each — the planted text for the first,
// the presence of unwalked state for the second.
func readStructuralModes(t *testing.T, report *marshalerReport, typ reflect.Type, entry reviewedMarshaler, where string, base fixtureSpec) {
	t.Helper()
	for label, mode := range structuralFixtureModes(base) {
		populated := newMarshalerFixture(t, typ, mode)
		diffFixture(t, report, typ, entry, where+label, populated, false)

		// Same state, DIFFERENT planted text: an extra derived from any of it
		// differs, however it was encoded on the way out. Read in EVERY mode —
		// an extra that reaches for user text only while an optional is absent
		// is invisible to a comparison run on the populated shape alone.
		other := mode
		other.seq = mode.seq + 500
		mine := decodeMembers(t, typ.String()+".MarshalJSON", populated.custom)
		theirs := decodeMembers(t, typ.String()+".MarshalJSON",
			newMarshalerFixture(t, typ, other).custom)
		for name := range entry.extra {
			if string(mine[name]) != string(theirs[name]) {
				report.changed = append(report.changed, fmt.Sprintf(
					"%s%s: %s emits %s for one record and %s for another whose only difference "+
						"is the text planted in it — the entry claims it is derived from the enum "+
						"beside it, never from user text", where, label, name, mine[name], theirs[name]))
			}
		}

		// Same state, same planted text, unwalked state EMPTY: the output must
		// not move at all.
		control := mode
		control.withUnwalked = false
		if empty := newMarshalerFixture(t, typ, control); !bytes.Equal(populated.custom, empty.custom) {
			report.added = append(report.added, fmt.Sprintf(
				"%s%s: the output depends on state encoding/json cannot reach — with the "+
					"unexported and json:\"-\" fields populated it emits %s, with them empty %s",
				where, label, populated.custom, empty.custom))
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

			report := &marshalerReport{}

			// The state the walk leaves behind is the one the declared extra
			// VALUES describe, so it is the only place they are pinned.
			diffFixture(t, report, typ, entry, "unseeded",
				newMarshalerFixture(t, typ, fixtureSpec{withUnwalked: true}), true)

			seq := 1000
			for pattern, spread := range guardScalarPatterns {
				for _, state := range guardMarshalerStates {
					where := fmt.Sprintf("pattern %d state %d", pattern, state)
					base := fixtureSpec{seq: seq, pattern: spread, state: state, withUnwalked: true}
					readStructuralModes(t, report, typ, entry, where, base)

					// The other meaningful CHANNEL state, against the same empty
					// reading: an open channel opens a `!= nil` gate, a CLOSED
					// one opens a completed-work gate, and neither is the other.
					// Read on the populated shape only — the structural modes
					// vary the WALKED side, and crossing the two would multiply
					// the fixtures without reading a new gate.
					closed := base
					closed.closeChans = true
					control := base
					control.withUnwalked = false
					if a, b := newMarshalerFixture(t, typ, closed), newMarshalerFixture(t, typ, control); !bytes.Equal(a.custom, b.custom) {
						report.added = append(report.added, fmt.Sprintf(
							"%s, closed: the output depends on state encoding/json cannot reach — "+
								"with the unexported and json:\"-\" fields populated it emits %s, with "+
								"them empty %s", where, a.custom, b.custom))
					}
					seq += 1000
				}
			}

			report.reportTo(t, typ, entry)
		})
	}
}

// plainTwinOf returns the same value in a generated struct type with the same
// exported fields and tags and NO methods, so encoding/json renders it by its
// own field rules instead of through the type's MarshalJSON.
//
// Ordinary unexported fields are left out: json never emits them, so their
// absence cannot change the baseline — which is exactly what makes an unexported
// field's text show up as an undeclared member in the diff rather than as a
// matching one.
//
// An anonymously embedded unexported STRUCT is the exception, and skipping it
// was a false positive waiting to happen: json PROMOTES its exported members, so
// a twin without them makes a faithful alias marshaler look like it adds every
// one of those members (#3592 review). They are promoted into the twin instead,
// carrying their own tags — which is what json does with them, and the same
// shape mustVisit already handles in the main walk.
func plainTwinOf(t *testing.T, value reflect.Value) reflect.Value {
	t.Helper()
	typ := value.Type()
	var fields []reflect.StructField
	var sources []func(reflect.Value) reflect.Value
	// promoted marks the twin fields lifted out of an anonymous embedding, which
	// is what makes a name collision unrepresentable.
	var promoted []bool
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		at := i
		if field.PkgPath == "" {
			fields = append(fields, reflect.StructField{
				Name: field.Name, Type: field.Type, Tag: field.Tag, Anonymous: field.Anonymous,
			})
			sources = append(sources, func(v reflect.Value) reflect.Value { return v.Field(at) })
			promoted = append(promoted, false)
			continue
		}
		embedded := baseType(field.Type)
		// `json:"-"` on the embedding removes it from the document entirely, so
		// the twin must not promote its members either — json does not.
		if !field.Anonymous || embedded.Kind() != reflect.Struct || field.Tag.Get("json") == "-" {
			continue
		}
		// A NAMED tag stops it being a promotion at all: json nests the embedded
		// value under that member instead of lifting its children, so flattening
		// them makes a faithful marshaler look like it added the member and
		// dropped the children (#3592 review). The twin cannot express the
		// nesting, so it refuses, as it does for a colliding name.
		if name, _, _ := strings.Cut(field.Tag.Get("json"), ","); name != "" {
			t.Fatalf("the plain twin of %s cannot represent it: %s is an anonymous unexported "+
				"embedding tagged %q, which json NESTS under that member rather than promoting "+
				"its children.\n\nTeach plainTwinOf to keep the nesting, or drop the exemption "+
				"for this type — a twin that answers differently from encoding/json is no "+
				"baseline.", typ, field.Name, name)
		}
		for j := 0; j < embedded.NumField(); j++ {
			lifted := embedded.Field(j)
			if lifted.PkgPath != "" {
				continue
			}
			member := j
			fields = append(fields, reflect.StructField{
				Name: lifted.Name, Type: lifted.Type, Tag: lifted.Tag,
			})
			sources = append(sources, func(v reflect.Value) reflect.Value {
				held := v.Field(at)
				held = reflect.NewAt(held.Type(), held.Addr().UnsafePointer()).Elem()
				for held.Kind() == reflect.Pointer {
					if held.IsNil() {
						return reflect.Zero(lifted.Type)
					}
					held = held.Elem()
				}
				return held.Field(member)
			})
			promoted = append(promoted, true)
		}
	}
	// Promotion is flattened into the twin, which is faithful only while the
	// promoted names do not collide with anything else. json resolves a
	// collision by DEPTH — a direct field at depth zero dominates a promoted one,
	// and two at the same depth cancel — and a flattened twin has no depths left
	// to resolve by, so it would answer differently and blame the marshaler.
	// reflect.StructOf would panic on the duplicate Go name before it got the
	// chance (#3592 review).
	//
	// No reviewed type has that shape, and the guard refuses it rather than
	// guessing: a twin that cannot represent the type is no baseline for it.
	// Only a collision INVOLVING a promoted field is unrepresentable. Two direct
	// fields sharing a name sit at the same depth in the original type and in
	// the twin alike, and json suppresses both in each — so the twin answers
	// identically and is a perfectly good baseline. Refusing that shape too was
	// a false failure on something the contract can handle (#3592 review).
	//
	// A duplicate Go name would still panic reflect.StructOf, so it is refused
	// whatever its provenance.
	seen := map[string]int{}
	for i, field := range fields {
		for kind, name := range map[string]string{
			"Go name":   field.Name,
			"json name": jsonMemberName(field),
		} {
			previous, dup := seen[kind+" "+name]
			if !dup {
				seen[kind+" "+name] = i
				continue
			}
			if kind == "json name" && !promoted[i] && !promoted[previous] {
				continue
			}
			t.Fatalf("the plain twin of %s cannot represent it: %s and %s share the %s %q, and "+
				"json resolves a PROMOTED collision by embedding DEPTH, which a flattened twin "+
				"has thrown away.\n\nTeach plainTwinOf to keep the hierarchy, or drop the "+
				"exemption for this type — a twin that answers differently from encoding/json "+
				"is no baseline.",
				typ, fields[previous].Name, field.Name, kind, name)
		}
	}
	twin := reflect.New(reflect.StructOf(fields)).Elem()
	for i, source := range sources {
		twin.Field(i).Set(source(value))
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
func plantUnwalkedState(filler *sentinelFiller, value reflect.Value, closeChans bool) []string {
	first := len(filler.planted)
	plantUnwalkedInto(filler, value, "", 0, false, closeChans)
	var forms []string
	for _, planted := range filler.planted[first:] {
		forms = append(forms, planted.forms...)
	}
	return forms
}

// hidden says whether this value is already INSIDE unwalked state. It is what
// separates "populate it so a gate opens" from "leave it alone": a timestamp or
// a channel reached through a hidden field is state only a marshaler can see, so
// filling it is free — while the same shapes on the walked side are part of what
// the with/without fixtures must hold identical, and touching one there would
// make them differ and blame the marshaler (#3592 review).
func plantUnwalkedInto(filler *sentinelFiller, value reflect.Value, path string, depth int, hidden, closeChans bool) {
	if depth > guardMaxDepth {
		// Reported, not skipped. A hidden leaf beyond the bound stays empty,
		// and a silent return keeps it out of unclassifiedFixtureGaps too — so
		// a marshaler that exposes it only when populated would pass on a
		// fixture that could never populate it (#3592 review).
		filler.tooDeep = append(filler.tooDeep, path+" (unwalked state below the depth limit)")
		return
	}
	switch value.Kind() {
	case reflect.Pointer:
		if !value.IsNil() {
			plantUnwalkedInto(filler, value.Elem(), path, depth+1, hidden, closeChans)
		}
	case reflect.Slice, reflect.Array:
		if elem := value.Type().Elem().Kind(); elem == reflect.Uint8 || elem == reflect.Int32 {
			return
		}
		for i := 0; i < value.Len(); i++ {
			plantUnwalkedInto(filler, value.Index(i), path+"[]", depth+1, hidden, closeChans)
		}
	case reflect.Map:
		// A map element is not addressable, so it is planted in a copy and
		// written back. Without this arm the hidden fields of a map's element
		// struct stay empty and raise nothing — neither a marker nor a register
		// entry — so a parent that exposes them would pass (#3592 review).
		if value.IsNil() || !value.CanSet() {
			return
		}
		rebuilt := reflect.MakeMap(value.Type())
		for _, key := range value.MapKeys() {
			elem := reflect.New(value.Type().Elem()).Elem()
			elem.Set(value.MapIndex(key))
			plantUnwalkedInto(filler, elem, path+"[]", depth+1, hidden, closeChans)
			rebuilt.SetMapIndex(key, elem)
		}
		value.Set(rebuilt)
	case reflect.Struct:
		if value.Type() == reflect.TypeOf(time.Time{}) {
			// A HIDDEN timestamp is set. fill exempts time.Time as text-free and
			// seedScalars steps over it, which is right for what json emits and
			// wrong for what a marshaler can read: one gated on
			// `hiddenTime.IsZero()` would find it zero in every fixture
			// (#3592 review).
			if hidden && value.CanSet() {
				value.Set(reflect.ValueOf(guardHiddenInstant))
			}
			return
		}
		typ := value.Type()
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			at := join(path, field.Name)
			settable := guardSettableField(value, i)
			// guardHidesSubtree is the shared answer, and both halves of it
			// matter here. An unexported ANONYMOUS struct embedding is not
			// unwalked state — json PROMOTES its exported members and mustVisit
			// already plants them, so planting them again makes the two fixtures
			// differ in walked text and the independence check blames the
			// marshaler for it. And `json:"-"` gets an unexported field's
			// treatment, opaque state included: the tag hides it from the
			// DEFAULT encoder and from nothing else, so an ignored channel or
			// function is a gate a marshaler can still read (#3592 review).
			if !guardHidesSubtree(field) {
				plantUnwalkedInto(filler, settable, at, depth+1, hidden, closeChans)
				continue
			}
			// The recursion's default arm is the ONE place opaque state is
			// populated — an explicit call here as well was redundant, and a
			// probe that removed it stayed red because the recursion had already
			// done the work.
			fillUnwalked(filler, settable, at)
			plantUnwalkedInto(filler, settable, at, depth+1, true, closeChans)
		}
	default:
		if hidden {
			populateOpaque(filler, value, path, closeChans)
		}
	}
}

// fillUnwalked plants into hidden state, leaving the kinds populateOpaque owns
// to it.
//
// fill REPORTS an interface rather than planting one, which is right for the
// walked side — there is no correct marker without knowing the concrete type —
// and wrong here, where the recursion is about to give it a representative
// value. Calling fill anyway left the report standing beside a field that had
// just been populated, and the gap register then demanded an entry for it
// (#3592 review).
func fillUnwalked(filler *sentinelFiller, value reflect.Value, path string) {
	switch value.Kind() {
	case reflect.Interface, reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return
	}
	filler.fill(value, path, 0, false)
}

// guardHiddenInstant is the value a hidden timestamp is set to: fixed, so two
// fixtures of the same shape stay comparable, and unmistakably non-zero.
var guardHiddenInstant = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

// guardContextInterface is the one interface type the fixture has a
// representative for — see populateOpaque's Interface arm for why it is an
// exact type match rather than an Implements check.
var guardContextInterface = reflect.TypeOf((*context.Context)(nil)).Elem()

// populateOpaque gives a hidden channel or function a non-nil value.
//
// encoding/json cannot serialize either, so the main walk rightly ignores them —
// but a MARSHALER can read them, and `GitWorktree.hooksDone` is exactly that
// shape: production sets it and HooksDone() exposes it, so a marshaler gated on
// `HooksDone() != nil` would keep that gate shut in every fixture that left it
// nil (#3592 review). Nothing is planted IN them; they are made non-nil so the
// gate opens.
//
// closeChans reads the OTHER meaningful channel state. hooksDone is closed when
// hooks finish, so a marshaler using a non-blocking receive to act "once hooks
// are done" is silent for an open channel and for a nil one alike — the contract
// reads both modes (#3592 review).
//
// A directional channel is made bidirectionally and converted, since MakeChan
// refuses a receive-only type. Anything that cannot be populated is recorded as
// a fixture gap rather than left silently zero.
func populateOpaque(filler *sentinelFiller, value reflect.Value, path string, closeChans bool) {
	switch value.Kind() {
	case reflect.Chan:
		if !value.CanSet() || !value.IsNil() {
			return
		}
		// Made bidirectionally and CLOSED BEFORE the conversion. MakeChan
		// refuses a receive-only type, and the conversion is precisely what
		// takes away the ability to close it — hooksDone is declared
		// `<-chan struct{}`, so a close attempted after converting is not
		// allowed and a guard that skipped it left the closed mode inert.
		// Measured: the probe for it passed until this order was fixed
		// (#3592 review).
		made := reflect.MakeChan(reflect.ChanOf(reflect.BothDir, value.Type().Elem()), 0)
		if closeChans {
			made.Close()
		}
		value.Set(made.Convert(value.Type()))
	case reflect.Func:
		if !value.CanSet() || !value.IsNil() {
			return
		}
		typ := value.Type()
		value.Set(reflect.MakeFunc(typ, func([]reflect.Value) []reflect.Value {
			out := make([]reflect.Value, typ.NumOut())
			for i := range out {
				out[i] = reflect.Zero(typ.Out(i))
			}
			return out
		}))
	case reflect.Interface:
		// A hidden interface can be a pure GATE — GitWorktree's constructors
		// assign a non-nil hooksCtx, and a marshaler can branch on its presence
		// without ever reading it, which the register's old rationale did not
		// cover (#3592 review).
		//
		// The CONTEXT SHAPE and nothing else gets the representative (#3655
		// item 1). The test used to be "does a context satisfy this interface",
		// which `any` answers yes to along with every other type in the
		// language — and a context is no representative of `any`: a marshaler
		// that asserts the field to a string and emits it finds a context in
		// every fixture, emits nothing, and passes while the fixture records the
		// gate as OPEN. Populated and reported are the only two answers, so a
		// broader interface is written down by name and unclassifiedFixtureGaps
		// then demands the reason the contract holds without it.
		if !value.CanSet() || !value.IsNil() {
			return
		}
		if value.Type() == guardContextInterface {
			value.Set(reflect.ValueOf(context.Background()))
			return
		}
		filler.unsupported = append(filler.unsupported,
			path+" ("+value.Type().String()+" is not the context shape, and no representative "+
				"value opens a gate that reads a concrete payload)")
	case reflect.UnsafePointer:
		filler.unsupported = append(filler.unsupported,
			path+" (hidden unsafe.Pointer cannot be populated)")
	}
}

// normalizedEmptyFinding reads a member the entry declares a normalization for,
// against the twin rendering that member as null. It returns the finding, or ""
// when the marshaler did exactly what the entry says.
//
// THREE outcomes, not two, and only one of them is the permitted difference:
//
//   - the declared form — the one difference an entry may declare.
//   - the OTHER empty collection (#3655 item 6). Accepting `[]` or `{}` for any
//     named member retained nothing about which the member is, so a regression
//     rendering a nil SLICE as an object read as the normalization the entry
//     describes while changing the member's public JSON type. The marshaler
//     picks that form itself, so it is a one-character edit away, and
//     TestGuardNormalizedEmptyFormsMatchTheirFields ties each declared form to
//     the member's Go kind so the declaration cannot drift from the type either.
//   - null — the normalization GONE (#3686 review). This is the outcome a diff
//     keyed on the two sides differing cannot see at all: the member then
//     matches its field exactly, every comparison passes, and the declaration
//     goes on describing a normalization that no longer happens. One word
//     inside ArchiveRetainedTree's clone() — returning the argument instead of
//     the make()d slice — is the whole regression.
func normalizedEmptyFinding(declared, name string, got json.RawMessage) string {
	switch string(got) {
	case declared:
		return ""
	case "null":
		return fmt.Sprintf("%s renders the absent member as null, and the entry declares it "+
			"normalizes to %s — the normalization is gone, so the declaration describes nothing "+
			"and every reading of that member passes vacuously", name, declared)
	}
	return fmt.Sprintf("%s renders the absent member as %s, and the entry declares it normalizes "+
		"to %s — a different public JSON type", name, got, declared)
}

// jsonMemberName is the member name encoding/json gives a field: the tag's name
// when it has a usable one, the Go field name otherwise. Only the name — the
// options after the comma decide nothing here.
func jsonMemberName(field reflect.StructField) string {
	name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
	if name == "" || name == "-" {
		return field.Name
	}
	return name
}

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

// scalarSeeder sets every scalar leaf reachable from a record to a value derived
// from the leaf's PATH and the state, so the record can be read somewhere other
// than the all-zero corner the walk leaves behind.
//
// Keyed by path, not by a running counter. A counter makes the value assigned to
// a field depend on how many scalars came before it, so allocating one hidden
// pointer shifts every later EXPORTED scalar — measured: the independence check
// then reported archive_report, runtime_cleanup and two bools as differing
// between two records that were supposed to differ only in hidden state, all of
// it the fixture's own doing.
type scalarSeeder struct {
	pattern func(index, state int) int
	state   int
	// withHidden seeds the scalars encoding/json cannot reach as well, and it
	// follows the fixture's own withUnwalked.
	//
	// That coupling is the whole of #3655 item 9. Seeding hidden scalars
	// unconditionally put the SAME value on both sides of the comparison whose
	// only job is to differ, so a marshaler gated on a hidden bool or numeric
	// emitted identical bytes with hidden state populated and with it empty, and
	// the reading passed on a gate that never moved. InstanceData carries two
	// such bools — snapshotTabsProjected and archiveReportPending — and dozens
	// more sit under archiveReportSource.
	withHidden bool
}

// seed walks value, seeding what the fixture is allowed to seed. hidden says
// whether this subtree is already where encoding/json cannot reach, which is the
// same question guardHidesSubtree answers for every other traversal here.
//
// Byte and rune containers are stepped over: their elements are the planted text
// itself, and overwriting them would destroy the evidence the rest of the test
// searches for.
func (s scalarSeeder) seed(value reflect.Value, path string, hidden bool) {
	if hidden && !s.withHidden {
		return
	}
	switch value.Kind() {
	case reflect.Pointer:
		if value.IsNil() {
			s.allocateTimestamp(value, path, hidden)
			return
		}
		s.seed(value.Elem(), path, hidden)
	case reflect.Struct:
		if value.Type() == reflect.TypeOf(time.Time{}) {
			s.seedTimestamp(value, path, hidden)
			return
		}
		for i := 0; i < value.NumField(); i++ {
			field := value.Type().Field(i)
			s.seed(guardSettableField(value, i), join(path, field.Name),
				hidden || guardHidesSubtree(field))
		}
	case reflect.Slice, reflect.Array:
		if elem := value.Type().Elem().Kind(); elem == reflect.Uint8 || elem == reflect.Int32 {
			return
		}
		for i := 0; i < value.Len(); i++ {
			s.seed(value.Index(i), path+"[]", hidden)
		}
	case reflect.Map:
		// Same rebuild as plantUnwalkedInto: a map element is not addressable,
		// so it is seeded in a copy and written back. Without this a scalar GATE
		// inside a map element stays at zero in every varied fixture, and a
		// marshaler that transforms a sibling string only when that gate is set
		// matches the twin everywhere (#3592 review).
		if value.IsNil() || !value.CanSet() {
			return
		}
		rebuilt := reflect.MakeMap(value.Type())
		for _, key := range value.MapKeys() {
			elem := reflect.New(value.Type().Elem()).Elem()
			elem.Set(value.MapIndex(key))
			s.seed(elem, path+"[]", hidden)
			rebuilt.SetMapIndex(key, elem)
		}
		value.Set(rebuilt)
	case reflect.Bool:
		value.SetBool(s.at(path)%2 == 1)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value.SetInt(int64(s.at(path)))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		value.SetUint(uint64(s.at(path)))
	case reflect.Float32, reflect.Float64:
		value.SetFloat(float64(s.at(path)))
	case reflect.Complex64, reflect.Complex128:
		// isScalarNumeric counts both complex kinds, so fill exempts them as
		// fixed-position scalars — and a gate on one would then read zero in
		// every varied fixture (#3592 review).
		value.SetComplex(complex(float64(s.at(path)), 0))
	}
}

// at is the value this seeder gives the leaf at path.
func (s scalarSeeder) at(path string) int { return s.pattern(pathIndex(path), s.state) }

// guardWalkedInstant is the base a seeded EXPORTED timestamp is derived from:
// fixed, so two fixtures of the same shape stay comparable, and unmistakably
// non-zero. Distinct from guardHiddenInstant so a marshaler copying one to the
// other is not mistaken for one leaving it alone.
var guardWalkedInstant = time.Date(2026, 3, 14, 9, 26, 53, 0, time.UTC)

// seedTimestamp gives an EXPORTED timestamp a representative non-zero instant
// (#3655 item 13).
//
// fill exempts time.Time as text-free and the seeder used to step over it, so
// every fixture carried CreatedAt and UpdatedAt at the ZERO time — a state
// production records essentially never hold. A marshaler that adds or transforms
// text only when a timestamp is set had that branch shut in all 96 readings.
// InstanceData alone has 26 exported timestamp leaves.
//
// A HIDDEN one is left to plantUnwalkedInto, which sets it on the populated side
// only. Setting it here as well would put the same instant on BOTH sides of the
// independence comparison and take the signal away — item 9's defect wearing a
// different field's clothes.
//
// Derived from the path and the state like every other scalar, so neighbouring
// timestamps differ and a marshaler gated on an ORDER between two of them is
// read both ways across the states — and derived deterministically, so the pair
// of records behind every comparison stays identical.
func (s scalarSeeder) seedTimestamp(value reflect.Value, path string, hidden bool) {
	if hidden || !value.CanSet() {
		return
	}
	value.Set(reflect.ValueOf(guardWalkedInstant.Add(time.Duration(s.at(path)) * time.Hour)))
}

// allocateTimestamp fills in the one optional the walk leaves absent.
//
// fill returns at a time.Time BEFORE its pointer arm allocates, so an optional
// timestamp was nil in every fixture and the branch a marshaler takes when one
// is SET went unread (#3655 item 13). Every other pointer fill reaches is
// already allocated, and the nil-pointers mode clears them all again — so both
// readings exist, and allocating here does not take the absent one away.
func (s scalarSeeder) allocateTimestamp(value reflect.Value, path string, hidden bool) {
	if hidden || !value.CanSet() || value.Type().Elem() != reflect.TypeOf(time.Time{}) {
		return
	}
	value.Set(reflect.New(value.Type().Elem()))
	s.seedTimestamp(value.Elem(), path, hidden)
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
//
// It runs through readStructuralModes, the same reading every generic state
// gets, which settles two of #3655's items at once. The archived record is now
// built by newMarshalerFixture, so the twin and the custom marshal get
// INDEPENDENT records rather than two readings of one — a pointer-receiver
// marshaler reached through the twin's shared backing storage would otherwise
// leave the custom call reading what the baseline call destroyed (item 4). And
// it is read in every structural mode, so a marshaler acting on a legacy
// archived row whose Tabs are nil — the shape a pre-#1195 row actually has — is
// no longer unread (item 5).
func TestGuardInstanceDataMarshalerIgnoresHiddenStateWhenArchived(t *testing.T) {
	typ := reflect.TypeOf(session.InstanceData{})
	entry := reviewedMarshalerTypes[typ]
	for _, tc := range []struct {
		name     string
		status   session.Status
		liveness session.Liveness
	}{
		{"composed", session.Archived, session.LiveArchived},
		{"legacy row", session.Archived, session.LivenessUnset},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := &marshalerReport{}
			readStructuralModes(t, report, typ, entry, "archived "+tc.name, fixtureSpec{
				seq:     7000,
				pattern: guardEveryScalarSet,
				state:   1,
				// The gate itself is ARMED on the populated side. Production
				// sets snapshotTabsProjected alongside the roster, and the walk
				// leaves a bool at its zero value — so a marshaler that exposed
				// the roster only while that flag is true would have sailed
				// through a fixture that never set it (#3592 review). The
				// CONTROL record readStructuralModes compares against leaves it
				// off, because its scalar seeding follows withUnwalked
				// (#3655 item 9) — which is what makes the two records differ in
				// the hidden state the comparison exists to read.
				withUnwalked: true,
				override: func(value reflect.Value) {
					record := value.Addr().Interface().(*session.InstanceData)
					record.Status, record.Liveness = tc.status, tc.liveness
				},
			})
			report.reportTo(t, typ, entry)
		})
	}
}

// guardEveryScalarSet is the pattern the named readings seed with: every bool
// on and every number one, so any flag a marshaler might gate on is open before
// the pair under test is set by hand.
func guardEveryScalarSet(int, int) int { return 1 }
