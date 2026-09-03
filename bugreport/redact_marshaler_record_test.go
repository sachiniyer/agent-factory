package bugreport

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"
)

// How a contract fixture RECORD is built: the states it reaches, the structural
// shapes it takes, and the state encoding/json can never reach that is planted
// in it.
//
// Split out of redact_marshaler_contract_test.go at #3655, at the seam the
// file-length limit found for the second time (#1145). That file decides what a
// reviewed marshaler's output must MATCH; this one decides what it is asked to
// render. The two separate cleanly: nothing here reads a marshaler's output, and
// nothing there plants a value.
//
// Everything here is deterministic in fixtureSpec — two records built to the
// same spec are identical, byte for byte — which is what makes the independence
// comparisons next door possible at all, since each compares two records that
// differ in exactly one thing.
//
// redact_marshaler_fixture_test.go probes THIS file: whether the state a record
// claims to reach is the state a marshaler would actually find there.

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
		filler.fill(value, "", 0, guardAddressable)
		filler.unsupported, filler.tooDeep = nil, nil
		plantUnwalkedState(filler, value, guardChanOpen)
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

// fixtureSpec is one reading of a reviewed type: which markers, which scalar
// state, and which of the structural conditions a marshaler might gate on.
type fixtureSpec struct {
	seq     int
	pattern func(index, state int) int
	state   int
	// withUnwalked populates the state encoding/json cannot reach.
	withUnwalked bool
	// chans is which of a hidden channel's readings this record builds.
	chans guardChanMode
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
	filler.fill(value, "", 0, guardAddressable)
	var unwalked []string
	if spec.withUnwalked {
		unwalked = plantUnwalkedState(filler, value, spec.chans)
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

// nilOptionalPointers clears every pointer the walk allocated on the WALKED side
// of a record, so it reads the way an ordinary one with nothing optional set
// does.
//
// fill allocates unconditionally — it has to, or it could not plant inside an
// optional subtree — which means the absent case was never read at all, and a
// marshaler that exposes hidden state only while a pointer is nil would keep
// that branch shut in every fixture (#3592 review).
//
// Which side a field is on is guardHidesSubtree's answer, and this traversal
// used to give its own (#3655 item 3). It skipped every unexported field, so an
// optional pointer PROMOTED out of an anonymous unexported embedding — a member
// json publishes, and fill allocates — was never cleared, and the absent branch
// stayed shut for exactly the members mustVisit and sparseWalkedState both make
// the exception for. The same answer also keeps it off `json:"-"` pointers,
// which are hidden state: clearing those emptied, in this mode, the very state
// the with/without comparison is there to compare.
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
			field := value.Type().Field(i)
			if guardHidesSubtree(field) {
				continue
			}
			held := guardSettableField(value, i)
			if field.PkgPath != "" {
				// Everything unexported still here is an anonymous struct
				// embedding, and the twin FLATTENS one — so a nil pointer at
				// this position is a shape the baseline cannot express at all
				// (see twinValues), rather than one member going absent.
				// Descended through, so the optionals INSIDE are still cleared,
				// which is the whole of item 3. An EXPORTED anonymous embedding
				// is left to the ordinary pointer arm: the twin keeps it
				// anonymous and json applies the same promotion rules to both
				// sides, so clearing it reads a real state and blames nobody.
				for held.Kind() == reflect.Pointer && !held.IsNil() {
					held = held.Elem()
				}
				if held.Kind() != reflect.Struct {
					continue
				}
			}
			nilOptionalPointers(held, depth+1)
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
func plantUnwalkedState(filler *sentinelFiller, value reflect.Value, chans guardChanMode) []string {
	first := len(filler.planted)
	plantUnwalkedInto(filler, value, "", 0, false, chans)
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
func plantUnwalkedInto(filler *sentinelFiller, value reflect.Value, path string, depth int, hidden bool, chans guardChanMode) {
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
			plantUnwalkedInto(filler, value.Elem(), path, depth+1, hidden, chans)
		}
	case reflect.Slice, reflect.Array:
		if elem := value.Type().Elem().Kind(); elem == reflect.Uint8 || elem == reflect.Int32 {
			return
		}
		for i := 0; i < value.Len(); i++ {
			plantUnwalkedInto(filler, value.Index(i), path+"[]", depth+1, hidden, chans)
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
			plantUnwalkedInto(filler, elem, path+"[]", depth+1, hidden, chans)
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
				plantUnwalkedInto(filler, settable, at, depth+1, hidden, chans)
				continue
			}
			// The recursion's default arm is the ONE place opaque state is
			// populated — an explicit call here as well was redundant, and a
			// probe that removed it stayed red because the recursion had already
			// done the work.
			fillUnwalked(filler, settable, at)
			plantUnwalkedInto(filler, settable, at, depth+1, true, chans)
		}
	default:
		if hidden {
			populateOpaque(filler, value, path, depth, chans)
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
	filler.fill(value, path, 0, guardAddressable)
}

// guardHiddenInstant is the value a hidden timestamp is set to: fixed, so two
// fixtures of the same shape stay comparable, and unmistakably non-zero.
var guardHiddenInstant = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

// guardContextInterface and guardErrorInterface are the interface types the
// fixture has a representative for — see populateOpaque's Interface arm for why
// they are exact type matches rather than Implements checks.
var (
	guardContextInterface = reflect.TypeOf((*context.Context)(nil)).Elem()
	guardErrorInterface   = reflect.TypeOf((*error)(nil)).Elem()
)

// guardChanMode is which reading of a hidden channel a record is built with.
//
// encoding/json can serialize none of them, so the main walk rightly ignores a
// channel — but a MARSHALER can read one, and `GitWorktree.hooksDone` is exactly
// that shape: production sets it and HooksDone() exposes it (#3592 review).
// Three readings, because a non-blocking receive answers each of them
// differently, and a `!= nil` gate cannot tell any of them apart:
type guardChanMode int

const (
	// guardChanOpen is an open, EMPTY channel: the gate is open, and a
	// non-blocking receive takes the DEFAULT branch — "not ready yet".
	guardChanOpen guardChanMode = iota
	// guardChanQueued is an open channel with elements buffered in it, so a
	// non-blocking receive DELIVERS one (#3655 item 12).
	//
	// Every mode used to be empty, so both readings handed back the element
	// type's zero value and a marshaler that non-blockingly receives queued text
	// was silent in all of them. The elements are planted like any other hidden
	// state, so text taken out of one and emitted is found by the same search
	// that covers every other unwalked field. A `chan struct{}` carries no
	// payload to plant, and the mode still moves it: the receive SUCCEEDS where
	// the empty channel's would have fallen through.
	guardChanQueued
	// guardChanClosed is the completed-work reading — hooksDone is closed when
	// hooks finish, and a marshaler acting "once hooks are done" is silent for
	// an open channel and for a nil one alike (#3592 review).
	//
	// Closed and EMPTY, deliberately: a closed channel still delivers what is
	// queued in it, so closing a queued one would take away the ok=false
	// reading that is the whole point of this mode.
	guardChanClosed
)

// guardChanCapacity is how many elements the queued mode buffers. Two, the same
// as a seeded collection, so a marshaler that drains gets more than one.
const guardChanCapacity = guardSliceSeed

// populateOpaque gives a hidden channel, function or interface a value a
// marshaler can actually read something out of.
//
// Nothing json can serialize passes through here — it errors on all three — so
// the main walk ignores them, and every one of them is a gate a marshaler can
// still read. What is planted is REPRESENTATIVE, not exhaustive: the bar each
// arm below is held to is that the value opens the gate AND carries whatever
// payload the shape's own interface can deliver, so a marshaler that reads
// through it finds planted text rather than a zero.
//
// A directional channel is made bidirectionally and converted, since MakeChan
// refuses a receive-only type. Anything that cannot be populated is recorded as
// a fixture gap rather than left silently zero.
func populateOpaque(filler *sentinelFiller, value reflect.Value, path string, depth int, chans guardChanMode) {
	switch value.Kind() {
	case reflect.Chan:
		if !value.CanSet() || !value.IsNil() {
			return
		}
		// Made bidirectionally, and CLOSED BEFORE the conversion. MakeChan
		// refuses a receive-only type, and the conversion is precisely what
		// takes away the ability to close it — hooksDone is declared
		// `<-chan struct{}`, so a close attempted after converting is not
		// allowed and a guard that skipped it left the closed mode inert.
		// Measured: the probe for it passed until this order was fixed
		// (#3592 review).
		elem := value.Type().Elem()
		capacity := 0
		if chans == guardChanQueued {
			capacity = guardChanCapacity
		}
		made := reflect.MakeChan(reflect.ChanOf(reflect.BothDir, elem), capacity)
		switch chans {
		case guardChanQueued:
			for i := 0; i < capacity; i++ {
				held := reflect.New(elem).Elem()
				plantHiddenPayload(filler, held, elementPath(path+"<-", i), depth, chans)
				made.Send(held)
			}
		case guardChanClosed:
			made.Close()
		}
		value.Set(made.Convert(value.Type()))
	case reflect.Func:
		if !value.CanSet() || !value.IsNil() {
			return
		}
		// The RESULTS are populated, not just the function (#3655 item 11).
		// Returning the zero of every result made "populated" true of the field
		// and false of anything it returns, so a marshaler calling the function
		// and reading what came back found an empty string, a false, a nil
		// error — in every fixture, with nothing written down about it.
		//
		// Built ONCE and captured, so the function is deterministic: two records
		// built to the same spec must return the same values, or the
		// independence comparisons would differ for reasons of their own.
		typ := value.Type()
		out := make([]reflect.Value, typ.NumOut())
		for i := range out {
			out[i] = reflect.New(typ.Out(i)).Elem()
			plantHiddenPayload(filler, out[i], path+"() result "+strconv.Itoa(i), depth, chans)
		}
		value.Set(reflect.MakeFunc(typ, func([]reflect.Value) []reflect.Value { return out }))
	case reflect.Interface:
		// A hidden interface can be a pure GATE — GitWorktree's constructors
		// assign a non-nil hooksCtx, and a marshaler can branch on its presence
		// without ever reading it, which the register's old rationale did not
		// cover (#3592 review).
		//
		// Only a shape with a representative is populated (#3655 item 1). The
		// test used to be "does a context satisfy this interface", which `any`
		// answers yes to along with every other type in the language — and a
		// context is no representative of `any`: a marshaler that asserts the
		// field to a string and emits it finds a context in every fixture, emits
		// nothing, and passes while the fixture records the gate as OPEN.
		// Populated and reported are the only two answers, so a shape not named
		// here is written down and unclassifiedFixtureGaps then demands the
		// reason the contract holds without it.
		//
		// TWO shapes qualify, by the same test: the value has to be readable
		// through the interface's OWN method set, not merely assignable to it.
		//
		//   - context.Context, a pure gate. Its methods carry no user text, and
		//     the field production has (hooksCtx) is read as presence.
		//   - error, which #3655 item 11 made reachable: `func() error` sits in
		//     the reviewed graph, and a nil result shuts a `!= nil` gate in
		//     every fixture. A non-nil error whose Error() returns a PLANTED
		//     marker opens the gate and carries a payload, so a marshaler that
		//     emits the message is caught by the same search as every other
		//     unwalked field rather than passing on an empty one.
		//
		// The residual is the same for both and is why `any` is refused: a
		// marshaler that type-asserts past the method set to a concrete type
		// sees something else. An interface with no methods offers nothing BUT
		// that, which is the line.
		if !value.CanSet() || !value.IsNil() {
			return
		}
		switch value.Type() {
		case guardContextInterface:
			value.Set(reflect.ValueOf(context.Background()))
		case guardErrorInterface:
			value.Set(reflect.ValueOf(errors.New(plantHiddenText(filler, path))))
		default:
			filler.unsupported = append(filler.unsupported,
				path+" ("+value.Type().String()+" has no representative value that opens a gate "+
					"and carries a payload its own method set can deliver)")
		}
	case reflect.UnsafePointer:
		filler.unsupported = append(filler.unsupported,
			path+" (hidden unsafe.Pointer cannot be populated)")
	}
}

// plantHiddenPayload fills one value a marshaler can only reach by CALLING
// something — a function's result, a channel's queued element — with the same
// planted state any other hidden field gets.
//
// The ordinary hidden path in one call: fillUnwalked for the shapes reflect can
// write text into, then the recursion for everything below, which is also what
// reaches populateOpaque again for an opaque leaf. A shape neither can populate
// lands in the gap register under a path naming how it is reached, so a result
// or an element the fixture cannot fill is written down rather than left zero.
func plantHiddenPayload(filler *sentinelFiller, value reflect.Value, path string, depth int, chans guardChanMode) {
	fillUnwalked(filler, value, path)
	plantUnwalkedInto(filler, value, path, depth+1, true, chans)
}

// plantHiddenText mints one marker and records it as planted, for a payload that
// is not reached through a settable reflect.Value — an error's message.
//
// Recorded through the same path a planted string takes, so the forms searched
// for in a marshaler's output are the same ones: the raw marker, its JSON
// encoding, and the unit that survives a partial edit.
func plantHiddenText(filler *sentinelFiller, path string) string {
	held := reflect.New(reflect.TypeOf("")).Elem()
	marker := filler.nextMarker()
	held.SetString(marker)
	filler.record(held, path, marker)
	return marker
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
			s.seed(value.Index(i), elementPath(path, i), hidden)
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
		// Ordered by KEY, and seeded by that order's rank rather than by the key
		// itself. Two records built to the same spec carry the same keys, so
		// either would be deterministic — but a record built with a different
		// SEQ carries different keys, and seeding from them would make a scalar
		// gate move with the planted text. The text-independence reading would
		// then blame the marshaler for the fixture's own doing. Ranks are 0 and
		// 1 whatever the keys are, and json emits map members in this same
		// order, so the two records line up member for member.
		keys := value.MapKeys()
		sort.Slice(keys, func(a, b int) bool {
			return fmt.Sprint(keys[a].Interface()) < fmt.Sprint(keys[b].Interface())
		})
		for i, key := range keys {
			elem := reflect.New(value.Type().Elem()).Elem()
			elem.Set(value.MapIndex(key))
			s.seed(elem, elementPath(path, i), hidden)
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

// elementPath names one element of a repeated aggregate by its INDEX.
//
// Every element used to seed from a path ending "[]", so all of them received
// identical scalars and no fixture could satisfy a gate over two of them —
// `Tabs[0].Kind != Tabs[1].Kind`, which production rosters routinely have
// (#3655 item 7). A marshaler that acts only on a heterogeneous collection was
// therefore read on a homogeneous one, always.
//
// The unwalked pass keeps "[]" deliberately: its paths are REPORTS, and the gap
// register is keyed on them, so an index there would make one entry per element
// out of one fact about a field.
func elementPath(path string, index int) string {
	return path + "[" + strconv.Itoa(index) + "]"
}

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
