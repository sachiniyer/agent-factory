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
var unpopulatedMarshalerState = map[string]string{}

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
		switch {
		case !isField:
			declared, ok := entry.extra[name]
			if !ok {
				note(&report.added, "%s: %s = %s", where, name, got)
			} else if pinExtras && string(got) != declared {
				note(&report.changed, "%s: %s emits %s, the entry declares %s", where, name, got, declared)
			}
		case string(got) != string(want):
			if normalizedEmpty(entry, name, got, want) {
				continue
			}
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
		seedScalars(value, spec.pattern, spec.state, "")
	}
	if spec.nilPointers {
		nilOptionalPointers(value, 0)
	}
	if spec.sparse {
		sparseWalkedState(value, spec.emptyNotNil, 0)
	}
	return value, unwalked
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
// Three things it must NOT touch, each a way of getting the comparison wrong:
//
//   - `json:"-"` fields and unexported ones. They are the hidden state the
//     independence reading compares, and clearing them in both fixtures leaves
//     nothing to compare (#3592 review).
//   - an anonymous unexported EMBEDDING is descended into rather than skipped:
//     json promotes its exported members and fill plants them, so they are part
//     of the walked side even though the embedding itself is unexported — the
//     same exception mustVisit makes (#3592 review).
//   - nil is not the only empty. `reflect.Zero` gives a NIL slice or map, and a
//     marshaler can distinguish that from an allocated empty one, so emptyNotNil
//     reads the other shape.
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
			field := value.Type().Field(i)
			if field.Tag.Get("json") == "-" {
				continue
			}
			if field.PkgPath != "" {
				if !field.Anonymous || baseType(field.Type).Kind() != reflect.Struct {
					continue
				}
				held := value.Field(i)
				sparseWalkedState(reflect.NewAt(held.Type(), held.Addr().UnsafePointer()).Elem(),
					emptyNotNil, depth+1)
				continue
			}
			sparseWalkedState(value.Field(i), emptyNotNil, depth+1)
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
			note := func(into *[]string, format string, args ...any) {
				*into = append(*into, fmt.Sprintf(format, args...))
			}
			diff := func(where string, fixture marshalerFixture, pinExtras bool) {
				diffFixture(t, report, typ, entry, where, fixture, pinExtras)
			}

			// The state the walk leaves behind is the one the declared extra
			// VALUES describe, so it is the only place they are pinned.
			diff("unseeded", newMarshalerFixture(t, typ, fixtureSpec{withUnwalked: true}), true)

			seq := 1000
			for pattern, spread := range guardScalarPatterns {
				for _, state := range guardMarshalerStates {
					where := fmt.Sprintf("pattern %d state %d", pattern, state)
					base := fixtureSpec{seq: seq, pattern: spread, state: state, withUnwalked: true}
					first := newMarshalerFixture(t, typ, base)
					diff(where, first, false)

					// The other ordinary production shapes, each read in full:
					// the optionals absent, and the strings and collections
					// empty. fill populates both, so neither branch was ever
					// taken (#3592 review).
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
						diff(where+label, newMarshalerFixture(t, typ, mode), false)
					}

					// Same state, DIFFERENT planted text: an extra derived from
					// any of it differs, however it was encoded on the way out.
					// Read in EVERY mode — an extra that reaches for user text
					// only while an optional is absent is invisible to a
					// comparison run on the populated shape alone.
					for label, mode := range modes {
						mine := decodeMembers(t, typ.String()+".MarshalJSON",
							newMarshalerFixture(t, typ, mode).custom)
						other := mode
						other.seq = mode.seq + 500
						theirs := decodeMembers(t, typ.String()+".MarshalJSON",
							newMarshalerFixture(t, typ, other).custom)
						for name := range entry.extra {
							if string(mine[name]) != string(theirs[name]) {
								note(&report.changed, "%s%s: %s emits %s for one record and %s for another "+
									"whose only difference is the text planted in it — the entry claims it "+
									"is derived from the enum beside it, never from user text",
									where, label, name, mine[name], theirs[name])
							}
						}
					}

					// Same state, same planted text, unwalked state EMPTY: the
					// output must not move at all.
					emptySpec := base
					emptySpec.withUnwalked = false
					empty := newMarshalerFixture(t, typ, emptySpec)
					sparseEmpty := emptySpec
					sparseEmpty.sparse = true
					sparsePopulated := base
					sparsePopulated.sparse = true
					if a, b := newMarshalerFixture(t, typ, sparsePopulated), newMarshalerFixture(t, typ, sparseEmpty); !bytes.Equal(a.custom, b.custom) {
						note(&report.added, "%s (sparse): the output depends on state encoding/json cannot "+
							"reach — with the unexported and json:\"-\" fields populated it emits %s, with "+
							"them empty %s", where, a.custom, b.custom)
					}
					// Both meaningful channel states, against the same empty
					// reading: an OPEN channel opens a `!= nil` gate, a CLOSED
					// one opens a completed-work gate, and neither is the other.
					closedSpec := base
					closedSpec.closeChans = true
					for label, populated := range map[string]marshalerFixture{
						"":         first,
						", closed": newMarshalerFixture(t, typ, closedSpec),
					} {
						if !bytes.Equal(populated.custom, empty.custom) {
							note(&report.added, "%s%s: the output depends on state encoding/json cannot "+
								"reach — with the unexported and json:\"-\" fields populated it emits "+
								"%s, with them empty %s", where, label, populated.custom, empty.custom)
						}
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
			switch {
			case field.PkgPath != "":
				held := value.Field(i)
				settable := reflect.NewAt(held.Type(), held.Addr().UnsafePointer()).Elem()
				if field.Anonymous && baseType(field.Type).Kind() == reflect.Struct &&
					field.Tag.Get("json") != "-" {
					// NOT unwalked state: json PROMOTES an anonymous unexported
					// struct's exported members and mustVisit already plants
					// them, so planting them again here makes the two fixtures
					// differ in walked text and the independence check blames
					// the marshaler for it. Only what is hidden DEEPER inside
					// is unwalked (#3592 review).
					//
					// The tag check is the whole condition, not decoration:
					// mustVisit rejects `json:"-"` BEFORE it ever reaches its
					// anonymous clause, so an ignored embedding is not walked at
					// all and its members are as unplanted as any other hidden
					// field (#3592 review).
					plantUnwalkedInto(filler, settable, at, depth+1, hidden, closeChans)
					continue
				}
				fillUnwalked(filler, settable, at)
				plantUnwalkedInto(filler, settable, at, depth+1, true, closeChans)
			case field.Tag.Get("json") == "-":
				// Same treatment as an unexported field, opaque state included:
				// the tag hides it from the DEFAULT encoder and from nothing
				// else, so an ignored channel or function is a gate a marshaler
				// can still read (#3592 review). Both branches hand the value to
				// the recursion with hidden=true, and its default arm is the ONE
				// place opaque state is populated — an explicit call here as
				// well was redundant, and a probe that removed it stayed red
				// because the recursion had already done the work.
				fillUnwalked(filler, value.Field(i), at)
				plantUnwalkedInto(filler, value.Field(i), at, depth+1, true, closeChans)
			default:
				plantUnwalkedInto(filler, value.Field(i), at, depth+1, hidden, closeChans)
			}
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
		// cover (#3592 review). A context satisfies context.Context and any, so
		// it opens both shapes; anything narrower is recorded rather than
		// guessed at.
		if !value.CanSet() || !value.IsNil() {
			return
		}
		ctx := reflect.ValueOf(context.Background())
		if ctx.Type().Implements(value.Type()) {
			value.Set(ctx)
			return
		}
		filler.unsupported = append(filler.unsupported,
			path+" ("+value.Type().String()+" has no representative value to populate)")
	case reflect.UnsafePointer:
		filler.unsupported = append(filler.unsupported,
			path+" (hidden unsafe.Pointer cannot be populated)")
	}
}

// normalizedEmpty reports the one difference an entry may declare: a member the
// marshaler renders as an empty collection where the default encoder renders the
// absent one as null. Anything else — including the reverse — still fails.
func normalizedEmpty(entry reviewedMarshaler, name string, got, want json.RawMessage) bool {
	if string(want) != "null" || (string(got) != "[]" && string(got) != "{}") {
		return false
	}
	for _, declared := range entry.normalizesEmpty {
		if declared == name {
			return true
		}
	}
	return false
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
			seedScalars(elem, pattern, state, path+"[]")
			rebuilt.SetMapIndex(key, elem)
		}
		value.Set(rebuilt)
	case reflect.Bool:
		value.SetBool(pattern(pathIndex(path), state)%2 == 1)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value.SetInt(int64(pattern(pathIndex(path), state)))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		value.SetUint(uint64(pattern(pathIndex(path), state)))
	case reflect.Float32, reflect.Float64:
		value.SetFloat(float64(pattern(pathIndex(path), state)))
	case reflect.Complex64, reflect.Complex128:
		// isScalarNumeric counts both complex kinds, so fill exempts them as
		// fixed-position scalars — and a gate on one would then read zero in
		// every varied fixture (#3592 review).
		value.SetComplex(complex(float64(pattern(pathIndex(path), state)), 0))
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
//
// The gate itself is ARMED. Production sets snapshotTabsProjected alongside the
// roster, and the walk leaves a bool at its zero value — so a marshaler that
// exposed the roster only while that flag is true would have sailed through a
// fixture that never set it (#3592 review). And these fixtures go through the
// whole contract, not just the hidden-state comparison: a member replaced or
// added only in an archived row is caught here too.
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
			archived := func(value reflect.Value) {
				// Every bool on, so any flag a marshaler might gate on is open,
				// and the two enums then set to the archived pair under test.
				seedScalars(value, func(int, int) int { return 1 }, 1, "")
				record := value.Addr().Interface().(*session.InstanceData)
				record.Status = tc.status
				record.Liveness = tc.liveness
			}
			report := &marshalerReport{}
			withHidden := archivedFixture(t, typ, 7000, true, archived)
			without := archivedFixture(t, typ, 7000, false, archived)
			diffFixture(t, report, typ, entry, "archived "+tc.name, withHidden, false)

			// The same text-independence reading the generic states get, which
			// they cannot be relied on to give this pair: a declared extra
			// derived from planted text only in the archived state matches
			// itself in both fixtures above, since those differ in hidden state
			// alone (#3592 review).
			other := archivedFixture(t, typ, 7500, true, archived)
			mine := decodeMembers(t, typ.String()+".MarshalJSON", withHidden.custom)
			theirs := decodeMembers(t, typ.String()+".MarshalJSON", other.custom)
			for name := range entry.extra {
				if string(mine[name]) != string(theirs[name]) {
					report.changed = append(report.changed, fmt.Sprintf(
						"archived %s: %s emits %s for one record and %s for another whose only "+
							"difference is the text planted in it — the entry claims it is derived "+
							"from the enum beside it, never from user text",
						tc.name, name, mine[name], theirs[name]))
				}
			}

			if !bytes.Equal(withHidden.custom, without.custom) {
				report.added = append(report.added, fmt.Sprintf(
					"archived %s: the output depends on state encoding/json cannot reach — "+
						"with it: %s\n  without: %s", tc.name, withHidden.custom, without.custom))
			}
			report.reportTo(t, typ, entry)
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
		unwalked = plantUnwalkedState(filler, value, false)
	}
	set(value)
	return marshalerFixture{
		baseline: marshalOrFail(t, "the plain twin of "+typ.String(), plainTwinOf(t, value)),
		custom:   marshalReviewed(t, typ, value),
		unwalked: unwalked,
	}
}
