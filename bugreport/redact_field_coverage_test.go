package bugreport

import (
	"bytes"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
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

	"Path":                   "absolute session path; scrub collapses $HOME and the username",
	"Branch":                 "the username segment collapses via scrub; the branch SUFFIX is deliberately retained — see TestScrubRedactsUsernameEndingInNonWordChar, which asserts \"[user]/fix-login-bug\"",
	"Worktree.RepoPath":      "absolute repo path; scrub collapses $HOME and the username",
	"Worktree.WorktreePath":  "absolute worktree path; scrub collapses $HOME and the username",
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
	"ArchiveReport.RetainedTrees[].Path":                                       "the retained tree's own path is the system worktree path, which scrub collapses via $HOME — the policy redact.go states for this field",
	"ArchiveReport.RollbackFence.OriginalRelocationRecovery.AlternatePath":     "absolute alternate worktree path; scrub collapses $HOME and the username",
	"ArchiveReport.RollbackFence.OriginalRelocationRecovery.CleanupGeneration": "minted cleanup-generation token",
	"ArchiveReport.RollbackFence.OriginalRelocationRecovery.CleanupLifecycle":  "bounded RelocationRecoveryState enum",
	"ArchiveReport.RollbackFence.OriginalRelocationRecovery.State":             "bounded RelocationRecoveryState enum",
	"Worktree.RelocationRecovery.AlternatePath":                                "absolute alternate worktree path; scrub collapses $HOME and the username",
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
	fillWithSentinel(t, v, 0)

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

// fillWithSentinel plants guardSentinel in every settable string-bearing field
// reachable from v, allocating pointers, one-element slices and one-entry maps
// on the way so nested shapes are actually visited.
//
// Unexported fields are skipped: reflect cannot set them, and they carry no
// json tag, so they never reach a bundle. time.Time is skipped for the same
// reason in the other direction — it is opaque, holds no user text, and
// recursing into its unexported internals would panic.
func fillWithSentinel(t *testing.T, v reflect.Value, depth int) {
	t.Helper()
	if depth > guardMaxDepth || !v.CanSet() {
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(guardSentinel)
	case reflect.Ptr:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		fillWithSentinel(t, v.Elem(), depth+1)
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if !v.Type().Field(i).IsExported() {
				continue
			}
			fillWithSentinel(t, v.Field(i), depth+1)
		}
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			v.SetBytes([]byte(guardSentinel))
			return
		}
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fillWithSentinel(t, v.Index(0), depth+1)
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			fillWithSentinel(t, v.Index(i), depth+1)
		}
	case reflect.Map:
		key := reflect.New(v.Type().Key()).Elem()
		fillWithSentinel(t, key, depth+1)
		val := reflect.New(v.Type().Elem()).Elem()
		fillWithSentinel(t, val, depth+1)
		m := reflect.MakeMap(v.Type())
		m.SetMapIndex(key, val)
		v.Set(m)
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
			if !field.IsExported() {
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
