package bugreport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// The contract that holds reviewedMarshalerTypes to its word: what a reviewed
// marshaler's output is COMPARED against, once a record has been built for it.
//
// Split out of redact_field_coverage_guard_test.go, which tests the WALK: this
// file tests the exemption the walk grants a self-rendering type, and the two
// grew far enough apart that one file could no longer hold both under the
// file-length limit (#1145). The seam is real — nothing here is about planting
// markers, and nothing there is about what a marshaler emits.
//
// Split again at #3655, along the seam that same limit found a second time:
// redact_marshaler_record_test.go BUILDS the fixture records — the states they
// reach, the shapes they take, the hidden state planted in them — and this file
// decides what their output has to match. Every reading below rests on the first
// of those being right, which is what redact_marshaler_fixture_test.go probes.

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

					// The other meaningful CHANNEL readings, against the same
					// empty control. An open channel opens a `!= nil` gate, a
					// QUEUED one delivers a payload to a non-blocking receive,
					// and a CLOSED one opens a completed-work gate — no two of
					// them are the same reading (#3655 item 12). Read on the
					// populated shape only: the structural modes vary the WALKED
					// side, and crossing the two would multiply the fixtures
					// without opening a new gate.
					control := base
					control.withUnwalked = false
					empty := newMarshalerFixture(t, typ, control)
					for label, mode := range map[string]guardChanMode{
						", queued channels": guardChanQueued,
						", closed channels": guardChanClosed,
					} {
						spec := base
						spec.chans = mode
						if a := newMarshalerFixture(t, typ, spec); !bytes.Equal(a.custom, empty.custom) {
							report.added = append(report.added, fmt.Sprintf(
								"%s%s: the output depends on state encoding/json cannot reach — "+
									"with the unexported and json:\"-\" fields populated it emits %s, "+
									"with them empty %s", where, label, a.custom, empty.custom))
						}
					}
					seq += 1000
				}
			}

			report.reportTo(t, typ, entry)
		})
	}
}

// twinField is one member of a plain twin: the field encoding/json will see,
// where to read its value from, and whether it was PROMOTED out of an anonymous
// embedding — which is what makes a name collision unrepresentable.
type twinField struct {
	field reflect.StructField
	// source reads the member out of the original record, and reports false when
	// a nil POINTER embedding broke the chain to it — see twinValues.
	source   func(reflect.Value) (reflect.Value, bool)
	promoted bool
}

// twinFieldsOf collects the fields encoding/json emits for typ, lifting the
// exported members of an anonymously embedded unexported STRUCT out of it.
//
// Skipping such an embedding was a false positive waiting to happen: json
// PROMOTES its exported members, so a twin without them makes a faithful alias
// marshaler look like it adds every one of them (#3592 review). They are lifted
// instead, carrying their own tags — which is what json does with them, and the
// same shape mustVisit already handles in the main walk.
//
// RECURSIVELY, which is #3655 item 8. Lifting one level and stopping loses the
// members json promotes out of an embedding INSIDE an embedding — measured:
// `struct{ inner1; Top string }` where inner1 holds `inner2` and a Mid renders
// as {"deep":…,"mid":…,"top":…}, all three at the top level. A twin that stopped
// at the first level was missing "deep" and blamed the marshaler for adding it.
// Same class as the collision refusal: a twin that answers differently from
// encoding/json is no baseline.
//
// holder reaches the struct value these fields live in, and reports false when a
// nil POINTER embedding broke the chain. That is not a value the twin can carry:
// see twinValues.
func twinFieldsOf(t *testing.T, typ reflect.Type, holder func(reflect.Value) (reflect.Value, bool), promoted bool, depth int) []twinField {
	t.Helper()
	if depth > guardMaxDepth {
		// Only a POINTER embedding can nest this far — a value one would be an
		// infinitely sized type — and stopping early silently drops members.
		t.Fatalf("the plain twin of %s cannot represent it: its anonymous embeddings nest deeper "+
			"than %d levels.\n\nA twin that stops early is missing members json promotes, which "+
			"makes a faithful marshaler look like it added them.", typ, guardMaxDepth)
	}
	var out []twinField
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		at, ftype := i, typ.Field(i).Type
		if field.PkgPath == "" {
			// Anonymity is carried through. A lifted field that is ITSELF an
			// exported anonymous embedding is promoted by json in the original,
			// so a twin that made it an ordinary named member would nest what
			// json flattens.
			out = append(out, twinField{
				field: reflect.StructField{
					Name: field.Name, Type: ftype, Tag: field.Tag, Anonymous: field.Anonymous,
				},
				source: func(root reflect.Value) (reflect.Value, bool) {
					held, ok := holder(root)
					if !ok {
						return reflect.Value{}, false
					}
					return held.Field(at), true
				},
				promoted: promoted,
			})
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
				"its children.\n\nTeach twinFieldsOf to keep the nesting, or drop the exemption "+
				"for this type — a twin that answers differently from encoding/json is no "+
				"baseline.", typ, field.Name, name)
		}
		inner := func(root reflect.Value) (reflect.Value, bool) {
			held, ok := holder(root)
			if !ok {
				return reflect.Value{}, false
			}
			held = held.Field(at)
			held = reflect.NewAt(held.Type(), held.Addr().UnsafePointer()).Elem()
			for held.Kind() == reflect.Pointer {
				if held.IsNil() {
					return reflect.Value{}, false
				}
				held = held.Elem()
			}
			return held, true
		}
		out = append(out, twinFieldsOf(t, embedded, inner, true, depth+1)...)
	}
	return out
}

// twinCollision returns the reason a FLATTENED twin cannot represent this field
// set, or "" when it can.
//
// Promotion is flattened into the twin, which is faithful only while the
// promoted names do not collide with anything else. json resolves a collision by
// DEPTH — a direct field at depth zero dominates a promoted one, and two at the
// same depth cancel — and a flattened twin has no depths left to resolve by, so
// it would answer differently and blame the marshaler. reflect.StructOf would
// panic on the duplicate Go name before it got the chance (#3592 review).
//
// No reviewed type has that shape, and the guard refuses it rather than
// guessing: a twin that cannot represent the type is no baseline for it. Only a
// collision INVOLVING a promoted field is unrepresentable. Two direct fields
// sharing a name sit at the same depth in the original type and in the twin
// alike, and json suppresses both in each — so the twin answers identically and
// is a perfectly good baseline. Refusing that shape too was a false failure on
// something the contract can handle (#3592 review).
//
// A duplicate Go name would still panic reflect.StructOf, so it is refused
// whatever its provenance.
//
// A field json emits NO member for contributes no json name (#3655 item 14): the
// exact tag `json:"-"` drops it from the document, so it cannot collide with
// anything, and naming it by its Go field name invented a collision json would
// never have.
//
// Returned rather than reported, so the rule can be read directly by a probe
// against a shape no reviewed type has.
func twinCollision(fields []twinField) string {
	seen := map[string]int{}
	for i, twin := range fields {
		names := []struct{ kind, name string }{{"Go name", twin.field.Name}}
		if member, emits := jsonMemberName(twin.field); emits {
			names = append(names, struct{ kind, name string }{"json name", member})
		}
		for _, at := range names {
			previous, dup := seen[at.kind+" "+at.name]
			if !dup {
				seen[at.kind+" "+at.name] = i
				continue
			}
			if at.kind == "json name" && !twin.promoted && !fields[previous].promoted {
				continue
			}
			return fmt.Sprintf("%s and %s share the %s %q",
				fields[previous].field.Name, twin.field.Name, at.kind, at.name)
		}
	}
	return ""
}

// plainTwinOf returns the same value in a generated struct type with the same
// exported fields and tags and NO methods, so encoding/json renders it by its
// own field rules instead of through the type's MarshalJSON.
//
// Ordinary unexported fields are left out: json never emits them, so their
// absence cannot change the baseline — which is exactly what makes an unexported
// field's text show up as an undeclared member in the diff rather than as a
// matching one.
// twinValues reads each twin member's value out of the original record, or
// returns the reason this VALUE cannot be represented.
//
// A nil anonymous POINTER embedding is that reason, and it is a defect the
// differential oracle found rather than one #3655 named: json OMITS every member
// promoted through a nil embedded pointer, where a flattened twin — which always
// emits its fields — renders each of them as a zero value. Measured on
// `struct{ *middle; Top string }` with the embedding nil: json renders
// {"top":"t"}, the twin rendered {"deep":"","mid":"","top":"t"}, so a faithful
// marshaler looked like it had ADDED deep and mid.
//
// The earlier one-level promotion substituted a zero value for exactly this case
// and said nothing. Refused instead, for the same reason a named tag and a
// promoted collision are: a twin that answers differently from encoding/json is
// no baseline. Nothing reaches it today — fill allocates every pointer, and
// nilOptionalPointers descends THROUGH an embedding rather than clearing it,
// precisely so this stays unreachable.
//
// Returned rather than reported, so a probe can read the rule directly.
func twinValues(fields []twinField, value reflect.Value) ([]reflect.Value, string) {
	out := make([]reflect.Value, len(fields))
	for i, from := range fields {
		held, ok := from.source(value)
		if !ok {
			return nil, fmt.Sprintf("%s is promoted through an anonymous POINTER embedding that "+
				"is nil, and json omits every member behind one rather than rendering it as a "+
				"zero value", from.field.Name)
		}
		out[i] = held
	}
	return out, ""
}

func plainTwinOf(t *testing.T, value reflect.Value) reflect.Value {
	t.Helper()
	typ := value.Type()
	fields := twinFieldsOf(t, typ, func(root reflect.Value) (reflect.Value, bool) {
		return root, true
	}, false, 0)
	if reason := twinCollision(fields); reason != "" {
		t.Fatalf("the plain twin of %s cannot represent it: %s, and json resolves a PROMOTED "+
			"collision by embedding DEPTH, which a flattened twin has thrown away.\n\nTeach "+
			"twinFieldsOf to keep the hierarchy, or drop the exemption for this type — a twin "+
			"that answers differently from encoding/json is no baseline.", typ, reason)
	}
	held, reason := twinValues(fields, value)
	if reason != "" {
		t.Fatalf("the plain twin of %s cannot represent this record: %s.\n\nA flattened twin "+
			"always emits its fields, so it cannot express the absence — teach twinFieldsOf to "+
			"keep the hierarchy, or stop the fixture from building this shape.", typ, reason)
	}
	shape := make([]reflect.StructField, len(fields))
	for i, twin := range fields {
		shape[i] = twin.field
	}
	twin := reflect.New(reflect.StructOf(shape)).Elem()
	for i, from := range held {
		twin.Field(i).Set(from)
	}
	if rendersItself(twin.Type()) {
		t.Fatalf("the plain twin of %s still renders itself, so it is no baseline", typ)
	}
	return twin
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

// jsonMemberName is the member name encoding/json gives a field, and whether it
// emits a member for the field at all.
//
// The dash is THREE cases, not one (#3655 item 14). Only the exact tag `json:"-"`
// removes the field from the document; `json:"-,"` and `json:"-,omitempty"`
// serialize it under the literal key "-" — measured, in isolation:
//
//	struct{ F string `json:"-"` ; N string }           -> {"N":"n"}
//	struct{ F string `json:"-,"` ; N string }          -> {"-":"SECRET","N":"n"}
//	struct{ F string `json:"-,omitempty"`; N string }  -> {"-":"SECRET","N":"n"}
//
// The same three-way reading mustVisit already had written into its comment, and
// this function disagreed with it: it parsed the name before the comma and
// handed back the GO field name for all three, so a member json really does emit
// as "-" was named something else. That is not only a lookup — twinCollision
// reads it, so the twin decided a duplicate on a name json would not use, and
// missed the one it would. Nothing in the tree carries the tag today.
//
// Only the name — the options after the comma decide nothing here.
func jsonMemberName(field reflect.StructField) (string, bool) {
	tag := field.Tag.Get("json")
	if tag == "-" {
		return "", false
	}
	if name, _, _ := strings.Cut(tag, ","); name != "" {
		return name, true
	}
	return field.Name, true
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
