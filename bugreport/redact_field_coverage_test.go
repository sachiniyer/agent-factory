package bugreport

import (
	"bytes"
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

// guardSentinel is planted in every string-bearing field of InstanceData before
// redaction runs. It is deliberately not a path, a title, or anything the text
// scrub knows how to collapse: this guard asks only "did redactInstanceData
// touch this field", never "would the scrub have saved it".
const guardSentinel = "AF-REDACTION-FIELD-GUARD-SENTINEL"

// guardMaxDepth bounds the reflective walk. Nothing in InstanceData is
// self-referential today; the bound is here so that a future field which is
// cannot hang the suite instead of failing it.
const guardMaxDepth = 12

// verbatimInstanceFields are the InstanceData paths redactInstanceData
// deliberately leaves alone, each with the reason it is safe to publish. The
// bar for an entry here is that the value is machine-minted (a stable id, a
// bounded enum, a git SHA) or an absolute system path the text scrub
// demonstrably collapses via $HOME/username.
//
// A value whose TEXT THE USER CHOSE does not belong here, even when it looks
// harmless. That is the judgement both #2419 and #3541 got wrong.
//
// Pipeline fact these justifications rest on: redactInstancesJSON runs
// redactInstanceData, marshals, then applies r.scrub — credscrub, $HOME to "~",
// username to "[user]". scrub does NOT remove session titles (scrubUnstructured
// does, kept separate so a short title like "id" cannot rewrite JSON keys), and
// it cannot reach bytes JSON has base64-encoded.
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

// TestRedactInstanceDataCoversEveryStringField is the #3548 guard. Two fields
// have already reached publicly shared bundles verbatim by being added to
// InstanceData while redactInstanceData was never taught about them —
// PendingHandoffMission (#2419) and ArchiveReport.RetainedTrees[].Skipped[].Path
// (#3541). Both were fixed by hand-adding the field, which fixes the instance
// and not the class.
//
// This walks InstanceData reflectively, plants a sentinel in every string-bearing
// field, runs the real redactInstanceData, and reports every field where the
// sentinel survived. A survivor must be named in verbatimInstanceFields with a
// reason, so a newly added field forces a deliberate redact-or-justify decision
// instead of defaulting to leak.
//
// Deliberately NOT a test of the text scrub. The scrub collapses $HOME to "~"
// and the username to "[user]"; a relative path like "private-work.txt" contains
// neither, which is precisely why #3541 shipped. Judging coverage by "the scrub
// would catch it" is the false intuition that produced both instances, so this
// guard never consults it.
func TestRedactInstanceDataCoversEveryStringField(t *testing.T) {
	var data session.InstanceData
	v := reflect.ValueOf(&data).Elem()
	filler := &sentinelFiller{}
	filler.fill(v, "", 0)

	// A subtree the walk could not reach is a subtree this guard cannot speak
	// for. Fail rather than quietly cover less than the name promises.
	if len(filler.tooDeep) > 0 {
		sort.Strings(filler.tooDeep)
		t.Errorf("the reflective walk hit its depth limit (%d) and skipped %d subtree(s):\n  %s\n\n"+
			"Those fields were never sentinel-planted, so this guard says nothing about them. "+
			"Raise guardMaxDepth, or flatten the shape.",
			guardMaxDepth, len(filler.tooDeep), strings.Join(filler.tooDeep, "\n  "))
	}
	if len(filler.unsupported) > 0 {
		sort.Strings(filler.unsupported)
		t.Errorf("the reflective walk met %d field(s) of a kind it cannot plant a sentinel in:\n  %s\n\n"+
			"encoding/json CAN serialize an interface's concrete value, so these are holes in the "+
			"guard, not fields it may ignore. Extend sentinelFiller.fill to populate a concrete "+
			"value for them.",
			len(filler.unsupported), strings.Join(filler.unsupported, "\n  "))
	}

	// Prove the fill actually planted something, or an empty walk would make
	// this test vacuously green forever.
	planted := collectSentinelPaths(v, "", 0)
	if len(planted) == 0 {
		t.Fatal("the reflective fill planted no sentinel: the walk is broken, " +
			"and this guard would pass no matter what redactInstanceData did")
	}

	redactInstanceData(&data)
	leaked := collectSentinelPaths(reflect.ValueOf(&data).Elem(), "", 0)

	leakedSet := make(map[string]struct{}, len(leaked))
	for _, path := range leaked {
		leakedSet[path] = struct{}{}
	}

	var unjustified []string
	seen := make(map[string]struct{}, len(leaked))
	for _, path := range leaked {
		_, safe := verbatimInstanceFields[path]
		_, known := knownUnredactedFields[path]
		if safe || known {
			continue
		}
		// Seeded slices yield the same path once per element; report it once.
		if _, dup := seen[path]; dup {
			continue
		}
		seen[path] = struct{}{}
		unjustified = append(unjustified, path)
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
			"     machine-minted id/enum/SHA, or an absolute path the scrub collapses via $HOME.\n"+
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

// sentinelFiller plants guardSentinel in every settable string-bearing field
// reachable from a value, and records the places it could NOT reach. Those
// records are failures, not diagnostics: a subtree this walk silently skipped is
// a subtree the guard cannot speak for, and a guard that stays green over a
// field it never visited is worse than no guard (#3592 review).
// jsonMarshalerType and textMarshalerType detect a type that renders itself.
// Such a type can emit text this reflective walk never sees — including text
// held in UNEXPORTED fields, which the walk deliberately skips — so its output
// cannot be reasoned about from Go fields alone (#3592 review).
var (
	jsonMarshalerType = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshalerType = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
)

// reviewedMarshalerTypes are the self-rendering types whose MarshalJSON has been
// read and shown to emit only the exported fields this walk already covers.
// Anything NOT listed here fails the guard rather than being trusted, because
// "it probably just marshals its fields" is the assumption that has to be
// checked once per type, not assumed forever.
var reviewedMarshalerTypes = map[reflect.Type]string{
	reflect.TypeOf(time.Time{}):               "timestamp; carries no user text and is skipped by the walk",
	reflect.TypeOf(git.ArchiveRetainedTree{}): "MarshalJSON marshals a `type wireTree ArchiveRetainedTree` alias of the same exported fields, normalized via clone()",
	reflect.TypeOf(git.ArchiveSkippedEntry{}): "MarshalJSON marshals a `type wireEntry ArchiveSkippedEntry` alias of the same exported fields, normalized via clone()",
}

// jsonSkipped reports whether encoding/json will omit this field entirely. A
// field tagged `json:"-"` can never reach a bundle, so planting a sentinel in it
// would fail the guard and push the author toward an allowlist entry for a field
// that is not even serialized — a misleading entry in a map whose whole value is
// that its entries are true (#3592 review).
func jsonSkipped(f reflect.StructField) bool {
	return f.Tag.Get("json") == "-"
}

// mustVisit reports whether a struct field can carry text into a bundle and so
// must be walked.
//
// The anonymous clause is the subtle half. encoding/json PROMOTES the exported
// members of an anonymously embedded struct, even when the embedded TYPE is
// unexported, so `struct{ inner }` with `inner{ Secret string }` serializes as
// {"secret": …}. A plain IsExported check skips that whole subtree while json
// happily publishes it (#3592 review).
//
// reflect agrees this is reachable: the embedded field itself reports
// CanSet=false, but its promoted exported members report CanSet=true — measured,
// not assumed — which is why fill can descend and plant in them.
func mustVisit(f reflect.StructField) bool {
	if jsonSkipped(f) {
		return false
	}
	if f.IsExported() {
		return true
	}
	return f.Anonymous && f.Type.Kind() == reflect.Struct
}

type sentinelFiller struct {
	tooDeep     []string
	unsupported []string
}

// guardSliceSeed is how many elements every reflected slice is filled with.
// TWO, not one: a redaction that touches only index 0 — the natural shape of a
// hand-written `x[0].Field = ...` or a loop with a stray break — passes a
// one-element probe while every later element still reaches the bundle
// verbatim. Two elements make the collection-wide contract observable.
const guardSliceSeed = 2

// fill plants guardSentinel throughout v, allocating pointers, slices and maps
// on the way so nested shapes are actually visited.
//
// Unexported fields are skipped: reflect cannot set them, they carry no json
// tag, and they never reach a bundle. time.Time is skipped for the mirror
// reason — opaque, no user text, and recursing into its unexported internals
// would panic.
func (f *sentinelFiller) fill(v reflect.Value, path string, depth int) {
	if depth > guardMaxDepth {
		f.tooDeep = append(f.tooDeep, path)
		return
	}
	if ty := v.Type(); ty.Implements(jsonMarshalerType) || reflect.PointerTo(ty).Implements(jsonMarshalerType) ||
		ty.Implements(textMarshalerType) || reflect.PointerTo(ty).Implements(textMarshalerType) {
		if _, reviewed := reviewedMarshalerTypes[ty]; !reviewed {
			f.unsupported = append(f.unsupported, path+" ("+ty.String()+" renders itself)")
			return
		}
	}
	// Structs are descended into even when the value itself is not settable:
	// that is exactly an anonymous embedded unexported struct, whose promoted
	// members ARE settable. Every other kind needs to be written to, so an
	// unsettable one is a subtree this guard cannot speak for.
	if !v.CanSet() && v.Kind() != reflect.Struct {
		f.unsupported = append(f.unsupported, path+" ("+v.Kind().String()+" is not settable)")
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(guardSentinel)
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
		if v.Type().Elem().Kind() == reflect.Uint8 {
			v.SetBytes([]byte(guardSentinel))
			return
		}
		v.Set(reflect.MakeSlice(v.Type(), guardSliceSeed, guardSliceSeed))
		for i := 0; i < v.Len(); i++ {
			f.fill(v.Index(i), path+"[]", depth+1)
		}
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			f.fill(v.Index(i), path+"[]", depth+1)
		}
	case reflect.Map:
		// guardSliceSeed entries under DISTINCT keys, for the slice reason: a
		// redaction that processes one arbitrary entry and returns would scrub
		// every planted sentinel from a single-entry fixture and pass.
		m := reflect.MakeMap(v.Type())
		for i := 0; i < guardSliceSeed; i++ {
			key := reflect.New(v.Type().Key()).Elem()
			f.fill(key, path+"[key]", depth+1)
			switch key.Kind() {
			case reflect.String:
				key.SetString(fmt.Sprintf("%s-%d", guardSentinel, i))
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				key.SetInt(int64(i))
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
				key.SetUint(uint64(i))
			default:
				// Cannot mint distinct keys of this kind, so the second entry
				// would overwrite the first and quietly halve the coverage.
				if i == 0 {
					f.unsupported = append(f.unsupported, path+" (map key kind "+key.Kind().String()+" cannot be seeded distinctly)")
				}
			}
			val := reflect.New(v.Type().Elem()).Elem()
			f.fill(val, path+"[]", depth+1)
			m.SetMapIndex(key, val)
		}
		v.Set(m)
	case reflect.Interface:
		// encoding/json happily serializes whatever concrete string, map or
		// slice an interface field holds, so an unhandled interface is a hole
		// in the guard rather than a field it may ignore. There is no correct
		// sentinel to plant without knowing the concrete type, so this fails
		// loudly and asks the author to extend the walk (#3592 review).
		f.unsupported = append(f.unsupported, path+" (interface)")
	case reflect.Chan, reflect.Func, reflect.UnsafePointer:
		// Not serializable by encoding/json — it errors rather than emitting
		// them — so they cannot reach a bundle and need no sentinel.
	}
}

// collectSentinelPaths returns the field paths under v whose value still
// contains guardSentinel, named the way a Go author would write them
// ("PendingTabCleanup[].TmuxName") so a failure points straight at the field.
func collectSentinelPaths(v reflect.Value, path string, depth int) []string {
	if depth > guardMaxDepth || !v.IsValid() {
		return nil
	}
	var found []string
	switch v.Kind() {
	case reflect.String:
		if strings.Contains(v.String(), guardSentinel) {
			found = append(found, path)
		}
	case reflect.Ptr:
		if !v.IsNil() {
			found = append(found, collectSentinelPaths(v.Elem(), path, depth+1)...)
		}
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			return nil
		}
		for i := 0; i < v.NumField(); i++ {
			field := v.Type().Field(i)
			if !mustVisit(field) {
				continue
			}
			found = append(found, collectSentinelPaths(v.Field(i), join(path, field.Name), depth+1)...)
		}
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			if bytes.Contains(v.Bytes(), []byte(guardSentinel)) {
				found = append(found, path)
			}
			return found
		}
		for i := 0; i < v.Len(); i++ {
			found = append(found, collectSentinelPaths(v.Index(i), path+"[]", depth+1)...)
		}
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			found = append(found, collectSentinelPaths(v.Index(i), path+"[]", depth+1)...)
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			found = append(found, collectSentinelPaths(key, path+"[key]", depth+1)...)
			found = append(found, collectSentinelPaths(v.MapIndex(key), path+"[]", depth+1)...)
		}
	}
	return found
}

func join(path, field string) string {
	if path == "" {
		return field
	}
	return fmt.Sprintf("%s.%s", path, field)
}
