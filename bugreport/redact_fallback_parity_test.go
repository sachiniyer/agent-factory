package bugreport

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRedactInstancesFallbackRedactsHostPlacementArchivedWorkspace is the
// #5571 fallback guard for the off-box workstream's locators. A legacy/corrupt
// record that fails the typed decode (here `status` is a string) must still have
// host, placement, and archived_workspace redacted on the generic path — each
// can carry an internal hostname or a remote workspace locator the typed path
// digests or drops wholesale. Before the fix these keys were absent from
// sensitiveJSONKeys, so the operator's internal host names and remote workspace
// layout reached a publicly shared bundle verbatim.
func TestRedactInstancesFallbackRedactsHostPlacementArchivedWorkspace(t *testing.T) {
	r := &redactor{}
	raw := json.RawMessage(`[{
		"id": "leg-1",
		"status": "legacy-string-status",
		"host": "ZZHOSTZZ",
		"placement": {
			"host_alias": "ZZALIASZZ",
			"workspace": {"location": "ZZLOCZZ"}
		},
		"archived_workspace": {
			"host": "ZZARCHWSZZ",
			"dir": "/srv/ZZARCHDIRZZ",
			"branch": "feature/ZZARCHBRANCHZZ"
		}
	}]`)
	out := string(r.redactInstancesJSON(raw))
	for _, leak := range []string{
		"ZZHOSTZZ", "ZZALIASZZ", "ZZLOCZZ",
		"ZZARCHWSZZ", "ZZARCHDIRZZ", "ZZARCHBRANCHZZ",
	} {
		if strings.Contains(out, leak) {
			t.Errorf("fallback path leaked %q:\n%s", leak, out)
		}
	}
	// The keys survive (with redacted values) so triage still sees the row had
	// a host, a placement, and an archived workspace.
	for _, key := range []string{`"host"`, `"placement"`, `"archived_workspace"`} {
		if !strings.Contains(out, key) {
			t.Errorf("fallback dropped the key %s — the marker should keep it for triage:\n%s", key, out)
		}
	}
	// Safe structural fields still survive.
	if !strings.Contains(out, "leg-1") || !strings.Contains(out, "legacy-string-status") {
		t.Errorf("fallback dropped safe structural fields:\n%s", out)
	}
}

// TestRedactInstancesFallbackRedactsCallbackAndArchiveNonce is the #5571 W3.1
// and #5189 fallback guard for the callback refusal and archive nonce. Both are
// redacted on the typed path — callback_unavailable can embed the operator's
// configured callback endpoint, and archive_nonce is an unguessable occupancy
// proof — so the fallback must not ship them verbatim either.
func TestRedactInstancesFallbackRedactsCallbackAndArchiveNonce(t *testing.T) {
	r := &redactor{}
	raw := json.RawMessage(`[{
		"id": "leg-1",
		"status": "legacy-string-status",
		"callback_unavailable": "ZZCBUNAVAILZZ",
		"archive_nonce": "ZZARCHNONCEZZ"
	}]`)
	out := string(r.redactInstancesJSON(raw))
	for _, leak := range []string{"ZZCBUNAVAILZZ", "ZZARCHNONCEZZ"} {
		if strings.Contains(out, leak) {
			t.Errorf("fallback path leaked %q:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, "leg-1") {
		t.Errorf("fallback dropped safe structural fields:\n%s", out)
	}
}

// TestRedactInstancesFallbackRedactsPruneIntent is the #5189 fallback guard for
// the prune intent's locators and nonces. The typed path collapses
// tombstone_path and worktree_path via collapsePathField and replaces
// leaf_name, leaf_nonce, and nonce with the marker. The fallback drops the
// whole prune_intent object wholesale, and mirrors the individual keys too so a
// legacy record that carries them at a different level is still covered.
func TestRedactInstancesFallbackRedactsPruneIntent(t *testing.T) {
	r := &redactor{}
	raw := json.RawMessage(`[{
		"id": "leg-1",
		"status": "legacy-string-status",
		"prune_intent": {
			"worktree_path": "/srv/ZZWTZZ",
			"tombstone_path": "/srv/ZZTOMBZZ",
			"leaf_name": "ZZLEAFNAMEZZ",
			"leaf_nonce": "ZZLEAFNONCEZZ",
			"nonce": "ZZNONCEZZ",
			"branch": "feature/keep-branch",
			"head": "abc123def456"
		}
	}]`)
	out := string(r.redactInstancesJSON(raw))
	for _, leak := range []string{
		"ZZWTZZ", "ZZTOMBZZ", "ZZLEAFNAMEZZ", "ZZLEAFNONCEZZ", "ZZNONCEZZ",
	} {
		if strings.Contains(out, leak) {
			t.Errorf("fallback path leaked %q:\n%s", leak, out)
		}
	}
	// The prune_intent key survives (with a redacted value) so triage still
	// sees the row had a prune intent.
	if !strings.Contains(out, `"prune_intent"`) {
		t.Errorf("fallback dropped the prune_intent key — the marker should keep it for triage:\n%s", out)
	}
	if !strings.Contains(out, "leg-1") {
		t.Errorf("fallback dropped safe structural fields:\n%s", out)
	}
}

// TestRedactInstancesFallbackRedactsPruneIntentKeysAtTopLevel verifies the
// individual key entries (tombstone_path, leaf_name, leaf_nonce, nonce) are
// redacted even when they appear OUTSIDE prune_intent — a legacy or hand-edited
// record may carry them at a different level, and the map is deliberately broad
// and fail-safe.
func TestRedactInstancesFallbackRedactsPruneIntentKeysAtTopLevel(t *testing.T) {
	r := &redactor{}
	raw := json.RawMessage(`[{
		"id": "leg-1",
		"status": "legacy-string-status",
		"tombstone_path": "/srv/ZZTOMBZZ",
		"leaf_name": "ZZLEAFNAMEZZ",
		"leaf_nonce": "ZZLEAFNONCEZZ",
		"nonce": "ZZNONCEZZ"
	}]`)
	out := string(r.redactInstancesJSON(raw))
	for _, leak := range []string{"ZZTOMBZZ", "ZZLEAFNAMEZZ", "ZZLEAFNONCEZZ", "ZZNONCEZZ"} {
		if strings.Contains(out, leak) {
			t.Errorf("fallback path leaked %q at the top level:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, "leg-1") {
		t.Errorf("fallback dropped safe structural fields:\n%s", out)
	}
}

// TestRedactInstancesFallbackRedactsWorktreeMissingReason is the #5102 fallback
// guard for worktree.missing_reason. The typed path rebuilds it from af's own
// forms (with the path collapsed) or collapses it whole; the fallback blanks it
// — the reason embeds the worktree path, a user-meaningful repo location.
func TestRedactInstancesFallbackRedactsWorktreeMissingReason(t *testing.T) {
	r := &redactor{}
	raw := json.RawMessage(`[{
		"id": "leg-1",
		"status": "legacy-string-status",
		"worktree": {
			"missing_reason": "tracked worktree path /srv/ConfidentialClient/ZZMISSREASONZZ does not exist",
			"branch": "feature/test"
		}
	}]`)
	out := string(r.redactInstancesJSON(raw))
	if strings.Contains(out, "ZZMISSREASONZZ") {
		t.Errorf("fallback path leaked the missing_reason text:\n%s", out)
	}
	if strings.Contains(out, "ConfidentialClient") {
		t.Errorf("fallback path leaked a path embedded in missing_reason:\n%s", out)
	}
	if !strings.Contains(out, "leg-1") || !strings.Contains(out, "feature/test") {
		t.Errorf("fallback dropped safe structural fields:\n%s", out)
	}
}

// TestRedactInstancesFallbackRedactsPushMode is the #5459 fallback guard for a
// docs tab's push_mode. The typed path keeps only "auto"/"manual"; the fallback
// blanks it wholesale — on a shape it could not parse it cannot distinguish the
// two enum words from a hand-edited string.
func TestRedactInstancesFallbackRedactsPushMode(t *testing.T) {
	r := &redactor{}
	raw := json.RawMessage(`[{
		"id": "leg-1",
		"status": "legacy-string-status",
		"tabs": [
			{"id": "tab-1", "push_mode": "ZZPUSHMODEZZ"},
			{"id": "tab-2", "push_mode": "auto"}
		]
	}]`)
	out := string(r.redactInstancesJSON(raw))
	if strings.Contains(out, "ZZPUSHMODEZZ") {
		t.Errorf("fallback path leaked a hand-edited push_mode:\n%s", out)
	}
	// "auto" is also blanked on the fallback path: this path cannot distinguish
	// the enum word from a hand-edited value, so it blanks both. The typed path
	// is where the enum survives; the fallback is the broader net by design.
	if strings.Contains(out, `"push_mode": "auto"`) {
		t.Errorf("fallback path kept an enum push_mode — on a shape it could not parse, the value is blanked:\n%s", out)
	}
	// Tab ids survive for triage.
	if !strings.Contains(out, "tab-1") || !strings.Contains(out, "tab-2") {
		t.Errorf("fallback dropped safe structural tab ids:\n%s", out)
	}
}

// TestRedactInstancesFallbackRedactsAllNewFields is the comprehensive guard: a
// single record that fails the typed decode and carries every field the #5571
// workstream, #5189, #5105, and #5459 added typed redactions for. Each field
// uses a distinct sentinel so a surviving value pinpoints exactly which key
// leaked. Negative controls (worktree_path, title, path) are redacted too,
// proving the fallback arm itself ran and the leak was the missing-key set.
func TestRedactInstancesFallbackRedactsAllNewFields(t *testing.T) {
	r := &redactor{}
	raw := json.RawMessage(`[{
		"id": "leg-1",
		"status": "running",
		"title": "ZZTITLEZZ",
		"path": "/srv/ZZPATHZZ",
		"host": "ZZHOSTZZ",
		"callback_unavailable": "ZZCBUNAVAILZZ",
		"archive_nonce": "ZZARCHNONCEZZ",
		"placement": {"host_alias": "ZZALIASZZ", "workspace": {"location": "ZZLOCZZ"}},
		"archived_workspace": {"host": "ZZARCHWSZZ", "dir": "/srv/ZZARCHDIRZZ", "branch": "feature/ZZARCHBRANCHZZ"},
		"prune_intent": {
			"worktree_path": "/srv/ZZWTZZ",
			"tombstone_path": "/srv/ZZTOMBZZ",
			"leaf_name": "ZZLEAFNAMEZZ",
			"leaf_nonce": "ZZLEAFNONCEZZ",
			"nonce": "ZZNONCEZZ",
			"branch": "feature/keep-branch",
			"head": "abc123def456"
		},
		"worktree": {
			"missing_reason": "ZZMISSREASONZZ",
			"repo_path": "/srv/ZZREPOZZ",
			"branch": "feature/test"
		},
		"tabs": [
			{"id": "tab-1", "push_mode": "ZZPUSHMODEZZ"}
		]
	}]`)
	out := string(r.redactInstancesJSON(raw))
	// Every sentinel that should be redacted — the new fields plus the
	// negative-control fields the fallback already covered.
	for _, leak := range []string{
		"ZZHOSTZZ", "ZZALIASZZ", "ZZLOCZZ",
		"ZZARCHWSZZ", "ZZARCHDIRZZ", "ZZARCHBRANCHZZ",
		"ZZCBUNAVAILZZ", "ZZARCHNONCEZZ",
		"ZZTOMBZZ", "ZZLEAFNAMEZZ", "ZZLEAFNONCEZZ", "ZZNONCEZZ",
		"ZZWTZZ", // worktree_path, a negative control already in the map
		"ZZMISSREASONZZ",
		"ZZPUSHMODEZZ",
		"ZZTITLEZZ", // title, a negative control already in the map
		"ZZPATHZZ",  // path, a negative control already in the map
		"ZZREPOZZ",  // repo_path, a negative control already in the map
	} {
		if strings.Contains(out, leak) {
			t.Errorf("fallback path leaked %q:\n%s", leak, out)
		}
	}
	// Structural fields outside the dropped objects survive the fallback walk.
	// branch/head inside prune_intent do NOT survive: prune_intent is dropped
	// wholesale (the same trade runtime_cleanup and attention make), so the
	// per-field layout inside it is not relied on on a shape this path could not
	// parse.
	for _, keep := range []string{"leg-1", "feature/test", "tab-1"} {
		if !strings.Contains(out, keep) {
			t.Errorf("fallback dropped safe structural field %q:\n%s", keep, out)
		}
	}
	// The redaction marker is present.
	if !strings.Contains(out, redactedMarker) {
		t.Errorf("expected redaction markers on the fallback path:\n%s", out)
	}
}
