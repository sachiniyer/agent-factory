package bugreport

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
)

// guardSentinel is the stem of the unique marker planted in each string-bearing
// field before redaction runs. It is deliberately not a path, a title, or
// anything the text scrub knows how to collapse: this guard asks only "did
// redactInstanceData touch this field", never "would the scrub have saved it".
const guardSentinel = "AF-REDACTION-FIELD-GUARD-SENTINEL"

// guardMaxDepth bounds the reflective walk. Nothing in InstanceData is
// self-referential today; the bound is here so that a future field which is
// cannot hang the suite instead of failing it.
const guardMaxDepth = 12

// guardSliceSeed is how many elements every reflected slice and map is filled
// with. TWO, not one: a redaction that touches only the first entry — the
// natural shape of a hand-written x[0].Field = ... or a loop with a stray
// break — passes a one-element probe while every later element still reaches
// the bundle verbatim.
const guardSliceSeed = 2

// guardMinContainer is the shortest fixed array that can hold a marker
// distinctive enough to trust. Below it, a truncated marker is too short to
// tell a planted value from incidental data, so the field is reported rather
// than silently probed with something meaningless.
const guardMinContainer = 8

var verbatimInstanceFields = map[string]string{
	"ID":     "minted instance id, never derived from user text",
	"TaskID": "minted task id (#1892), never derived from user text",

	"BackendType":  "bounded backend discriminator (\"local\", \"remote\", \"\")",
	"CurrentAgent": "agent enum name (tmux.SupportedPrograms), not user text",

	"Branch":                 "the username segment collapses via scrub; the branch SUFFIX is deliberately retained — see TestScrubRedactsUsernameEndingInNonWordChar, which asserts \"[user]/fix-login-bug\"",
	"Worktree.BranchName":    "same branch-name policy as Branch above",
	"Worktree.BaseCommitSHA": "git SHA, machine-minted",

	"IdleReason":               "bounded IdleReason enum (#3168)",
	"LastPromptDeliveryStatus": "bounded PromptDeliveryStatus enum (#3162)",
	"LifecycleAction":          "bounded LifecycleAction enum (\"archive\"/\"restore\")",
	"RootRecreateContext":      "bounded RootRecreateContext enum (#2629)",

	"ModelChange.Before": "model identifier reported by the agent, not user text",
	"ModelChange.After":  "model identifier reported by the agent, not user text",

	"PRInfo.Branch": "same branch-name policy as Branch above",
	"PRInfo.State":  "bounded PR state enum",

	"AgentConversation.Agent":       "agent enum name; the resumable ID beside it is cleared",
	"AgentConversation.CaptureKind": "bounded capture-kind enum",

	"PendingTabCleanup[].TabID": "minted tab id (#1738) — never derived from user text, and kept for triage; the TmuxName beside it is redacted",

	"Tabs[].ID":                          "minted tab id (#1738)",
	"Tabs[].Conversation.Agent":          "agent enum name; the resumable ID beside it is cleared",
	"Tabs[].Conversation.CaptureKind":    "bounded capture-kind enum",
	"Tabs[].Handoffs[].To":               "incoming agent name (tmux.SupportedPrograms)",
	"Tabs[].Handoffs[].HeadSHA":          "git SHA, machine-minted",
	"Tabs[].Handoffs[].Reason":           "bounded HandoffReason* constant",
	"Tabs[].Handoffs[].From.Agent":       "outgoing agent enum name; the resumable ID beside it is cleared (#3405)",
	"Tabs[].Handoffs[].From.CaptureKind": "bounded capture-kind enum",

	"PendingTabs[].ID":                          "minted tab id (#1738)",
	"PendingTabs[].Conversation.Agent":          "agent enum name; the resumable ID beside it is cleared",
	"PendingTabs[].Conversation.CaptureKind":    "bounded capture-kind enum",
	"PendingTabs[].Handoffs[].To":               "incoming agent name (tmux.SupportedPrograms)",
	"PendingTabs[].Handoffs[].HeadSHA":          "git SHA, machine-minted",
	"PendingTabs[].Handoffs[].Reason":           "bounded HandoffReason* constant",
	"PendingTabs[].Handoffs[].From.Agent":       "outgoing agent enum name; the resumable ID beside it is cleared (#3405)",
	"PendingTabs[].Handoffs[].From.CaptureKind": "bounded capture-kind enum",

	"TabKinds[].Kind":   "bounded tab-kind enum",
	"TabKinds[].Reason": "the daemon's OWN refusal text (#3060), not user input",

	"ArchiveReport.RetainedTrees[].Skipped[].Reason":                           "af-authored skip diagnostic (\"permission denied\"), not a user-chosen name; redact.go's stated policy keeps it for triage while blanking the Path beside it",
	"ArchiveReport.RollbackFence.OriginalRelocationRecovery.CleanupGeneration": "minted cleanup-generation token",
	"ArchiveReport.RollbackFence.OriginalRelocationRecovery.CleanupLifecycle":  "bounded RelocationRecoveryState enum",
	"ArchiveReport.RollbackFence.OriginalRelocationRecovery.State":             "bounded RelocationRecoveryState enum",
	"Worktree.RelocationRecovery.CleanupGeneration":                            "minted cleanup-generation token",
	"Worktree.RelocationRecovery.CleanupLifecycle":                             "bounded RelocationRecoveryState enum",
	"Worktree.RelocationRecovery.State":                                        "bounded RelocationRecoveryState enum",
}

// knownUnredactedFields are paths that DO reach a shared bundle verbatim and
// are NOT judged safe — each tracked by an issue. They are separated from
// verbatimInstanceFields on purpose: folding a known leak into the "safe" list
// with a soothing sentence is how an allowlist stops being a guard. This map is
// a debt register, and every entry names where the debt is filed.
//
// Do not add an entry here to make a failing build green. Add one only for a
// leak that predates the guard and has an issue.
var knownUnredactedFields = map[string]string{
	// Absolute paths are NOT guaranteed to be reachable by the scrub. scrub
	// replaces r.home and the username tokens and nothing else, so a repo or
	// worktree outside $HOME — /srv/ConfidentialClient/repo, a sibling checkout,
	// anything reached via --repo — ships its directory names verbatim. Claiming
	// these as "scrub collapses $HOME" asserted a guarantee the pipeline does not
	// make, which is the same false intuition that produced #3541 (#3592 review).
	"Path":                                      "#3588 — absolute session path, only collapsed when it happens to sit under $HOME",
	"Worktree.RepoPath":                         "#3588 — the USER's repo path; routinely outside $HOME and never collapsed there",
	"Worktree.WorktreePath":                     "#3588 — absolute worktree path, only collapsed when it happens to sit under $HOME",
	"ArchiveReport.RetainedTrees[].Path":        "#3588 — retained tree root; same unguaranteed-$HOME assumption",
	"Worktree.RelocationRecovery.AlternatePath": "#3588 — alternate worktree path; same unguaranteed-$HOME assumption",
	"ArchiveReport.RollbackFence.OriginalRelocationRecovery.AlternatePath": "#3588 — same unguaranteed-$HOME assumption",

	"Tabs[].Name":              "#3588 — user-chosen tab name; not a title (scrubSessionTitles cannot know it) and not a path",
	"PendingTabs[].Name":       "#3588 — same user-chosen tab name under the staging roster",
	"Account":                  "#3588 — user-chosen credential-account label (--account work); nothing in the pipeline touches it",
	"LostRestoreFailure.Error": "#3588 — af-authored diagnostic; embedded paths collapse via scrub, but an embedded session title does not",
	"Program":                  "#3588 — may be an arbitrary command line; redactTabData redacts the TabData.Command analogue wholesale, so leaving this verbatim is an inconsistency, not a decision",

	"ArchiveWarning": "#3588 — the bounded warning projection embeds the user-chosen skipped file names. #3554 (74e3b06f) closed the LOG path for exactly this text, but scrubArchiveWarningPaths is called only from scrubLog; redactInstancesJSON applies plain scrub, so the FIELD still carries them into the JSON section",
}

// ---------------------------------------------------------------------------
// Which fields reach a bundle is decided by encoding/json, not by this file.
//
// Earlier revisions of this guard modelled json's field selection directly —
// json:"-", ambiguous promoted names, tagged dominance — and review found a bug
// in that model three rounds running (#3592). Each fix added another rule and
// another corner to get wrong, and a partially-correct reimplementation of
// encoding/json's typeFields is a worse liability than none: when it is wrong in
// the skip direction it silently stops covering a field json really does emit.
//
// So the model is gone. The walk plants a UNIQUE marker per field and is
// deliberately PERMISSIVE — over-planting is free, because a field json drops
// simply never appears in the document. Redaction then runs, the record is
// marshalled, and a field counts as leaked if and only if its marker survives
// into that JSON. encoding/json answers the question it is the authority on, and
// every rule above falls out for free: a json:"-" field is absent, an ambiguous
// pair is absent, a tagged winner is present and its shadowed loser is not, a
// tagged anonymous struct nests instead of promoting, an invalid tag falls back
// to the Go field name.
//
// What the walk still owns is the part json cannot answer: whether a value could
// be planted at all. Those cases are failures, never silent skips.
// ---------------------------------------------------------------------------

// plantedField records one planted marker and the exact JSON fragment it
// produces, captured at plant time by marshalling the value itself. Searching
// the finished document for that fragment is what makes []byte (base64),
// [N]byte and []rune (integer lists) detectable without this file knowing how
// json encodes any of them.
type plantedField struct {
	path  string
	forms []string
}

// mustVisit reports whether a struct field can carry text into a bundle and so
// must be walked.
//
// The anonymous clause is the subtle half. encoding/json PROMOTES the exported
// members of an anonymously embedded struct, even when the embedded TYPE is
// unexported, so `struct{ inner }` with `inner{ Secret string }` serializes as
// {"secret": ...}. A plain IsExported check skips that whole subtree while json
// happily publishes it. A pointer embed promotes the same way.
//
// reflect agrees this is reachable: the embedded field itself reports
// CanSet=false, but its promoted exported members report CanSet=true — measured,
// not assumed — which is why fill can descend and plant in them.
func mustVisit(f reflect.StructField) bool {
	// `json:"-"` is an explicit never-serialize marker, not a selection rule
	// among competing fields, so honouring it is not the modelling this guard
	// gave up. It must be honoured BEFORE the walk, because an unplantable
	// shape behind that tag (any, []int, an unreviewed marshaler) would
	// otherwise be reported as a hole in a field json cannot emit (#3592 review).
	// Compare the PARSED tag name, not the whole tag. encoding/json omits the
	// field for every "-" spelling — `json:"-"`, `json:"-,"` and
	// `json:"-,omitempty"` all marshal to nothing (measured) — so an exact-string
	// match on `-` alone reported a harmless non-serializable field as a hole
	// (#3592 review).
	if strings.Split(f.Tag.Get("json"), ",")[0] == "-" {
		return false
	}
	if f.IsExported() {
		return true
	}
	if !f.Anonymous {
		return false
	}
	t := f.Type
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t.Kind() == reflect.Struct
}

// jsonMarshalerType and textMarshalerType detect a type that renders itself.
// Such a type can emit text this walk never sees — including text held in
// UNEXPORTED fields, which the walk cannot plant into — so its output cannot be
// reasoned about from Go fields alone.
var (
	jsonMarshalerType = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshalerType = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
)

// reviewedMarshalerTypes are the self-rendering types whose MarshalJSON has been
// read and shown to emit only the exported fields this walk already covers.
// Anything NOT listed here fails the guard rather than being trusted, because
// "it probably just marshals its fields" is a claim to check once per type, not
// to assume forever.
var reviewedMarshalerTypes = map[reflect.Type]string{
	reflect.TypeOf(time.Time{}):               "timestamp; carries no user text and is skipped by the walk",
	reflect.TypeOf(git.ArchiveRetainedTree{}): "MarshalJSON marshals a `type wireTree ArchiveRetainedTree` alias of the same exported fields, normalized via clone()",
	reflect.TypeOf(git.ArchiveSkippedEntry{}): "MarshalJSON marshals a `type wireEntry ArchiveSkippedEntry` alias of the same exported fields, normalized via clone()",
}

// isUnplantableScalar reports an element kind that json serializes but fill has
// no way to plant readable text in. Refusing is the point: a numeric container
// holding user text would otherwise sit in a zero-valued fixture no detection
// pass can see.
func isScalarNumeric(k reflect.Kind) bool {
	switch k {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Uintptr, reflect.Float32, reflect.Float64,
		reflect.Complex64, reflect.Complex128:
		return true
	}
	return false
}

// isUnplantableScalar reports a SEQUENCE element kind that json serializes but
// fill has no way to plant readable text in. uint8 and int32 are excluded
// because a sequence of them is a text container this guard does plant —
// []byte and []rune. Every other numeric kind, Uintptr included, must be
// refused: `[]uintptr` marshals to plain integers that reconstruct code points
// or packed bytes just as well (#3592 review).
func isUnplantableScalar(k reflect.Kind) bool {
	return isScalarNumeric(k) && k != reflect.Uint8 && k != reflect.Int32
}

// sentinelFiller plants a unique marker in every plantable field and records the
// places it could NOT reach. Those records are failures, not diagnostics: a
// subtree this walk skipped is a subtree the guard cannot speak for, and a guard
// that stays green over a field it never visited is worse than no guard.
type sentinelFiller struct {
	tooDeep     []string
	unsupported []string
	planted     []plantedField
	seq         int
}

// nextMarker puts the unique sequence FIRST. A fixed array is filled by
// truncation, so a marker whose distinguishing suffix falls off the end gives
// every short array identical contents — two [8]byte fields would both read
// "AF-REDAC", and one field's surviving fragment would mark the other leaked
// too, wrecking attribution and the stale-entry check (#3592 review).
// guardMinContainer is sized so the sequence always survives.
func (f *sentinelFiller) nextMarker() string {
	f.seq++
	return fmt.Sprintf("%04d-%s", f.seq, guardSentinel)
}

// record captures the JSON fragment the just-planted value produces.
// record captures every JSON fragment the just-planted value can produce.
//
// BOTH the raw marker and the marshalled form are kept, and a match on either
// counts. The raw marker is what survives an encoding option that re-quotes the
// value: a field tagged `json:",string"` serializes a string as "\"marker\"",
// so the marshalled form ("marker", with its own quotes) is NOT a substring of
// the document while the bare marker still is (#3592 review). The marshalled
// form is what catches the containers whose encoding hides the text entirely —
// []byte as base64, [N]byte and []rune as integer lists. Markers are
// alphanumeric-and-dash, so json never escapes them and the raw form is exact.
func (f *sentinelFiller) record(v reflect.Value, path, marker string) {
	forms := []string{marker}
	if v.CanInterface() {
		if form, err := json.Marshal(v.Interface()); err == nil {
			forms = append(forms, string(form))
		}
	}
	f.planted = append(f.planted, plantedField{path: path, forms: forms})
}

// fill plants markers throughout v, allocating pointers, slices and maps on the
// way so nested shapes are actually visited.
//
// Unexported non-anonymous fields are skipped: reflect cannot set them, and they
// reach a bundle only through a custom marshaler, which is refused above.
// time.Time is skipped as opaque and text-free.
func (f *sentinelFiller) fill(v reflect.Value, path string, depth int) {
	if depth > guardMaxDepth {
		f.tooDeep = append(f.tooDeep, path)
		return
	}
	// Normalize through pointers before the reviewed lookup. *time.Time
	// implements json.Marshaler via the promoted value method, so keying on the
	// POINTER type missed the exemption and failed the suite on an ordinary
	// optional timestamp (#3592 review).
	if ty := v.Type(); ty.Implements(jsonMarshalerType) || reflect.PointerTo(ty).Implements(jsonMarshalerType) ||
		ty.Implements(textMarshalerType) || reflect.PointerTo(ty).Implements(textMarshalerType) {
		base := ty
		for base.Kind() == reflect.Ptr {
			base = base.Elem()
		}
		if _, reviewed := reviewedMarshalerTypes[base]; !reviewed {
			f.unsupported = append(f.unsupported, path+" ("+ty.String()+" renders itself)")
			return
		}
		if base == reflect.TypeOf(time.Time{}) {
			return
		}
	}
	// Structs are descended into even when the value itself is not settable:
	// that is exactly an anonymous embedded unexported struct, whose promoted
	// members ARE settable. Every other kind must be written to, so an
	// unsettable one is a subtree this guard cannot speak for.
	if !v.CanSet() && v.Kind() != reflect.Struct {
		f.unsupported = append(f.unsupported, path+" ("+v.Kind().String()+" is not settable)")
		return
	}
	switch v.Kind() {
	case reflect.String:
		marker := f.nextMarker()
		v.SetString(marker)
		f.record(v, path, marker)
	case reflect.Ptr:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		f.fill(v.Elem(), path, depth+1)
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			return
		}
		for i := 0; i < v.NumField(); i++ {
			field := v.Type().Field(i)
			if !mustVisit(field) {
				continue
			}
			f.fill(v.Field(i), join(path, field.Name), depth+1)
		}
	case reflect.Slice:
		f.fillSequence(v, path, depth, true)
	case reflect.Array:
		f.fillSequence(v, path, depth, false)
	case reflect.Map:
		f.fillMap(v, path, depth)
	case reflect.Interface:
		// encoding/json serializes whatever concrete string, map or slice an
		// interface field holds, so an unhandled interface is a hole in the
		// guard rather than a field it may ignore. There is no correct marker
		// to plant without knowing the concrete type.
		f.unsupported = append(f.unsupported, path+" (interface)")
	case reflect.Chan, reflect.Func, reflect.UnsafePointer:
		// Not serializable by encoding/json — it errors rather than emitting
		// them — so they cannot reach a bundle and need no marker.
	}
}

// fillSequence handles slices and arrays, including the byte and rune forms
// whose JSON encodings differ from each other and from a plain list.
func (f *sentinelFiller) fillSequence(v reflect.Value, path string, depth int, isSlice bool) {
	elem := v.Type().Elem().Kind()
	switch {
	case elem == reflect.Uint8:
		// Written element by element rather than with SetBytes/reflect.Copy:
		// both demand EXACTLY uint8, so a named byte type (`type DigestByte
		// uint8`) — which this kind-based branch admits — panicked and crashed
		// the suite instead of being classified (#3592 review).
		marker := f.nextMarker()
		if isSlice {
			raw := []byte(marker)
			sv := reflect.MakeSlice(v.Type(), len(raw), len(raw))
			for i, b := range raw {
				sv.Index(i).SetUint(uint64(b))
			}
			v.Set(sv)
		} else {
			if v.Len() < guardMinContainer {
				f.unsupported = append(f.unsupported,
					fmt.Sprintf("%s ([%d]byte is too short to hold a distinctive marker)", path, v.Len()))
				return
			}
			for i, b := range []byte(marker) {
				if i >= v.Len() {
					break
				}
				v.Index(i).SetUint(uint64(b))
			}
		}
		f.record(v, path, marker)
	case elem == reflect.Int32:
		// []rune is []int32, and json emits it as code points from which the
		// exact text reconstructs.
		marker := f.nextMarker()
		runes := []rune(marker)
		if isSlice {
			rv := reflect.MakeSlice(v.Type(), len(runes), len(runes))
			for i, r := range runes {
				rv.Index(i).SetInt(int64(r))
			}
			v.Set(rv)
		} else {
			if v.Len() < guardMinContainer {
				f.unsupported = append(f.unsupported,
					fmt.Sprintf("%s ([%d]rune is too short to hold a distinctive marker)", path, v.Len()))
				return
			}
			for i, r := range runes {
				if i >= v.Len() {
					break
				}
				v.Index(i).SetInt(int64(r))
			}
		}
		f.record(v, path, marker)
	case isUnplantableScalar(elem):
		f.unsupported = append(f.unsupported,
			fmt.Sprintf("%s (%s of %s cannot carry a text marker)", path, v.Kind(), elem))
	default:
		if isSlice {
			v.Set(reflect.MakeSlice(v.Type(), guardSliceSeed, guardSliceSeed))
		}
		for i := 0; i < v.Len(); i++ {
			f.fill(v.Index(i), path+"[]", depth+1)
		}
	}
}

// fillMap seeds guardSliceSeed entries under distinct keys, for the slice
// reason: a redaction that processes one arbitrary entry and returns would
// scrub every marker from a single-entry fixture and pass.
func (f *sentinelFiller) fillMap(v reflect.Value, path string, depth int) {
	// A SCALAR map value cannot hold a marker — unlike a sequence, there is one
	// number per key, not a container of them — while json still emits the whole
	// map, so `map[int]int32` of code points would reconstruct text with nothing
	// planted anywhere. uint8 and int32 are refused here too, for that reason
	// (#3592 review).
	// Note the hole but DO NOT return: the keys of such a map are still
	// plantable and still reach the bundle, so returning here would trade a
	// value-shaped blind spot for a key-shaped one.
	valueScalar := isScalarNumeric(v.Type().Elem().Kind())
	if valueScalar {
		f.unsupported = append(f.unsupported,
			fmt.Sprintf("%s (map value kind %s cannot carry a text marker)", path, v.Type().Elem().Kind()))
	}
	m := reflect.MakeMap(v.Type())
	for i := 0; i < guardSliceSeed; i++ {
		key := reflect.New(v.Type().Key()).Elem()
		switch key.Kind() {
		case reflect.String:
			// The key must be planted and recorded in ONE step. Filling it and
			// then overwriting it for distinctness recorded a marker that no
			// longer existed, so a map with user-controlled KEYS contributed
			// nothing detectable and passed unconditionally (#3592 review).
			marker := f.nextMarker()
			key.SetString(marker)
			f.record(key, path+"[key]", marker)
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			key.SetInt(int64(i))
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			key.SetUint(uint64(i))
		default:
			if i == 0 {
				f.unsupported = append(f.unsupported,
					path+" (map key kind "+key.Kind().String()+" cannot be seeded distinctly)")
			}
		}
		val := reflect.New(v.Type().Elem()).Elem()
		if !valueScalar {
			f.fill(val, path+"[]", depth+1)
		}
		m.SetMapIndex(key, val)
	}
	v.Set(m)
}

// dedupeSorted removes repeats and orders the result. Seeded containers visit
// the same path once per element, so a single unplantable field would otherwise
// be listed guardSliceSeed times and counted as that many fields — a report that
// misstates how much of the record is uncovered.
func dedupeSorted(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func join(path, field string) string {
	if path == "" {
		return field
	}
	return fmt.Sprintf("%s.%s", path, field)
}

// TestRedactInstanceDataCoversEveryStringField is the #3548 guard. Two fields
// have already reached publicly shared bundles verbatim by being added to
// InstanceData while redactInstanceData was never taught about them —
// PendingHandoffMission (#2419) and ArchiveReport.RetainedTrees[].Skipped[].Path
// (#3541). Both were fixed by hand-adding the field, which fixes the instance
// and not the class.
//
// This plants a unique marker in every plantable field, runs the real
// redactInstanceData, marshals the result, and reports every field whose marker
// survived into the JSON. A survivor must be classified — redacted, allowlisted
// as safe with a reason, or recorded as a tracked leak — so a newly added field
// forces a deliberate redact-or-justify decision instead of defaulting to leak.
//
// Deliberately NOT a test of the text scrub. The scrub collapses $HOME to "~"
// and the username to "[user]"; a relative path like "private-work.txt" contains
// neither, which is precisely why #3541 shipped. Judging coverage by "the scrub
// would catch it" is the false intuition that produced both instances, so this
// guard never consults it.
func TestRedactInstanceDataCoversEveryStringField(t *testing.T) {
	var data session.InstanceData
	filler := &sentinelFiller{}
	filler.fill(reflect.ValueOf(&data).Elem(), "", 0)

	// A subtree the walk could not reach is a subtree this guard cannot speak
	// for. Fail rather than quietly cover less than the name promises.
	if tooDeep := dedupeSorted(filler.tooDeep); len(tooDeep) > 0 {
		t.Errorf("the reflective walk hit its depth limit (%d) and skipped %d subtree(s):\n  %s\n\n"+
			"Those fields were never marker-planted, so this guard says nothing about them. "+
			"Raise guardMaxDepth, or flatten the shape.",
			guardMaxDepth, len(tooDeep), strings.Join(tooDeep, "\n  "))
	}
	if unsupported := dedupeSorted(filler.unsupported); len(unsupported) > 0 {
		t.Errorf("the reflective walk met %d field(s) it cannot plant a marker in:\n  %s\n\n"+
			"encoding/json can still serialize these, so they are holes in the guard, not "+
			"fields it may ignore. Extend sentinelFiller to populate them.",
			len(unsupported), strings.Join(unsupported, "\n  "))
	}
	if len(filler.planted) == 0 {
		t.Fatal("the reflective fill planted no marker: the walk is broken, " +
			"and this guard would pass no matter what redactInstanceData did")
	}

	redactInstanceData(&data)

	// encoding/json decides what reaches a bundle. Marshalling the redacted
	// record and looking for each marker's own JSON fragment means this test
	// never has to model json's field-selection rules — see the note above.
	doc, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshalling the redacted record failed: %v", err)
	}
	rendered := string(doc)

	leakedSet := make(map[string]struct{})
	var leaked []string
	for _, p := range filler.planted {
		escaped := false
		for _, form := range p.forms {
			if strings.Contains(rendered, form) {
				escaped = true
				break
			}
		}
		if !escaped {
			continue
		}
		if _, dup := leakedSet[p.path]; dup {
			continue
		}
		leakedSet[p.path] = struct{}{}
		leaked = append(leaked, p.path)
	}

	var unjustified []string
	for _, path := range leaked {
		_, safe := verbatimInstanceFields[path]
		_, known := knownUnredactedFields[path]
		if !safe && !known {
			unjustified = append(unjustified, path)
		}
	}
	if len(unjustified) > 0 {
		sort.Strings(unjustified)
		t.Errorf("%d InstanceData field(s) reach a shared bug report verbatim and are "+
			"neither redacted by redactInstanceData nor classified:\n  %s\n\n"+
			"This is the #3548 class: a field was added to InstanceData and the redaction "+
			"policy was never taught about it (#2419, #3541).\n\n"+
			"Pick one, deliberately:\n"+
			"  1. Redact it in bugreport/redact.go — the default for anything whose text a USER chose.\n"+
			"  2. Add it to verbatimInstanceFields with the reason it is safe — only for a\n"+
			"     machine-minted id/enum/SHA, or a value covered by a written, test-asserted policy.\n"+
			"  3. Add it to knownUnredactedFields with a tracking issue, if it leaks and you are\n"+
			"     not fixing it here.\n\n"+
			"Do NOT reach for 2 to make this green. The scrub does not remove session titles and\n"+
			"cannot see inside base64, so \"the scrub will catch it\" is not a justification.",
			len(unjustified), strings.Join(unjustified, "\n  "))
	}

	// Neither map may rot. An entry that no longer leaks is describing code that
	// does not exist any more, which is how an allowlist quietly becomes a place
	// real leaks hide. For knownUnredactedFields a stale entry is good news —
	// the leak got fixed — and the fixer is told to delete the line by name.
	assertNoStaleEntries(t, "verbatimInstanceFields", verbatimInstanceFields, leakedSet,
		"They are now redacted or gone. Remove them so the allowlist keeps describing the code that actually runs.")
	assertNoStaleEntries(t, "knownUnredactedFields", knownUnredactedFields, leakedSet,
		"That leak is fixed — remove the entry (and close out its tracking issue).")
}

// assertNoStaleEntries reports classified paths that no longer leak.
func assertNoStaleEntries(t *testing.T, name string, classified map[string]string, leaked map[string]struct{}, advice string) {
	t.Helper()
	var stale []string
	for path := range classified {
		if _, ok := leaked[path]; !ok {
			stale = append(stale, path)
		}
	}
	if len(stale) == 0 {
		return
	}
	sort.Strings(stale)
	t.Errorf("%d entr(y/ies) in %s no longer reach the bundle verbatim:\n  %s\n\n%s",
		len(stale), name, strings.Join(stale, "\n  "), advice)
}
