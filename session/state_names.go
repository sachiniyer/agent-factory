package session

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Session state NAMES (#3631).
//
// `status`, `liveness` and `tabs[].kind` serialize as integers and must keep
// doing so: they are public JSON fields that external scripts already decode as
// numbers, so retyping them would break every one of those consumers. The same
// document, however, spells `lifecycle_action`, `backend_type` and
// `tab_kinds[].kind` as strings, and `af sessions list --status` accepts a word
// vocabulary that appeared NOWHERE in the output — so a listed session could not
// be mapped back to the documented filter values without reading Go source, and
// `kind` meant two different things in two adjacent arrays of one payload.
//
// The fix is additive: every integer keeps its meaning and its type, and gains a
// string twin — `status_name`, `liveness_name`, `tabs[].kind_name` — always
// present, never omitempty, because they are always known.
//
// There is exactly ONE table per enum: livenessVocabulary below, and
// tabKindVocabulary in session/tab.go (which lives beside the `--kind` parser it
// feeds). Everything else is derived from them — daemon/snapshot_filter.go
// validates `--status` with ParseLivenessName and renders its "valid: …" list
// with LivenessNameList, and tab.go's `--kind` map is the creatable half of its
// own table — so a name cannot mean one thing in the output and another in the
// flag that filters on it.
//
// The twins are derived at MARSHAL time rather than stored in a field. That is
// what makes them correct for every provenance: a record read off disk by the
// daemonless fallback, or one received from an older daemon that has never heard
// of these fields, is named by the binary printing it instead of carrying an
// empty string it was never told to fill.

// livenessVocabulary is the canonical Liveness ↔ name table, in enum order.
//
// It is simultaneously the `liveness_name` spelling and the vocabulary
// `af sessions list --status` / `Snapshot.statuses` accepts — one table, so the
// value a payload reports and the word that filters for it cannot drift apart.
//
// LivenessUnset is deliberately absent: it is not a state but the "no liveness
// recorded" zero value of a pre-#1195 record, and RecordedLiveness resolves it to
// a real one before anything is named. Naming it would invent a seventh word the
// filter does not accept.
var livenessVocabulary = []struct {
	liveness Liveness
	name     string
}{
	{LiveRunning, "running"},
	{LiveReady, "ready"},
	{LiveLost, "lost"},
	{LiveDead, "dead"},
	{LiveArchived, "archived"},
	{LiveLimitReached, "limit-reached"},
}

var livenessNames = func() map[Liveness]string {
	out := make(map[Liveness]string, len(livenessVocabulary))
	for _, entry := range livenessVocabulary {
		out[entry.liveness] = entry.name
	}
	return out
}()

var livenessByName = func() map[string]Liveness {
	out := make(map[string]Liveness, len(livenessVocabulary))
	for _, entry := range livenessVocabulary {
		out[entry.name] = entry.liveness
	}
	return out
}()

// LivenessName returns the wire name of a Liveness value, or "" for one that has
// none: LivenessUnset, or a value appended by a newer af whose record reached an
// older one. Empty is deliberate — a word it cannot spell honestly is better left
// unsaid than guessed at — and it never reaches `liveness_name`, which resolves
// its argument through RecordedLiveness first.
func LivenessName(l Liveness) string {
	return livenessNames[l]
}

// ParseLivenessName resolves a filter word to its Liveness. ok is false for
// anything outside the vocabulary, INCLUDING the empty string.
func ParseLivenessName(name string) (Liveness, bool) {
	l, ok := livenessByName[name]
	return l, ok
}

// LivenessNameList returns the filter vocabulary in enum order, for help text
// and "valid: …" error messages, so those strings are generated from the
// vocabulary rather than hand-maintained beside it. Mirrors TabKindNameList.
func LivenessNameList() []string {
	names := make([]string, 0, len(livenessVocabulary))
	for _, entry := range livenessVocabulary {
		names = append(names, entry.name)
	}
	return names
}

// StatusName returns the wire name of a legacy composed Status.
//
// The five SETTLED values are liveness values wearing the legacy enum's
// numbering, so their spelling comes from livenessVocabulary through the
// existing LivenessForStatus shim — not from a second list that could drift from
// the filter vocabulary. Loading and Deleting are the exception on purpose: they
// are in-flight OPERATION overlays with no liveness of their own (composeStatus
// produces them from the op axis), so no filter word can name them and they get
// the only honest spelling of the integer beside them.
//
// An unrecognized value names nothing rather than guessing. Routing it through
// LivenessForStatus instead would silently answer "ready" for it — that
// function's default — which is exactly how a value appended tomorrow would
// start reporting a confident wrong word today.
func StatusName(s Status) string {
	switch s {
	case Loading:
		return "loading"
	case Deleting:
		return "deleting"
	}
	if liveness, ok := statusLiveness(s); ok {
		return LivenessName(liveness)
	}
	return ""
}

// statusLiveness resolves the liveness a legacy Status carries, and reports
// ok=false for one that carries NONE — the transient overlays (Loading/Deleting,
// which composeStatus produces from the op axis, not from a state) and any value
// this binary does not know.
//
// LivenessForStatus cannot answer this on its own, and that is the point: its
// default is LiveReady, so it turns both of those into a confident "ready". For
// a caller that must produce SOME state to load an instance into, a default is
// necessary and that one is historical. For a caller NAMING a record, or
// deciding whether a filter word selects it, it is a fabricated answer — a
// record caught mid-kill by a pre-#1195 daemon would report itself, and be
// selected, as `ready`. This is the seam between those two needs; the settled
// set is listed once, here.
func statusLiveness(s Status) (Liveness, bool) {
	switch s {
	case Running, Ready, Dead, Lost, Archived:
		return LivenessForStatus(s), true
	}
	return LivenessUnset, false
}

// RecordedLiveness resolves the liveness a serialized record CARRIES: the
// explicit `liveness` field, falling back to the legacy `status` int for a
// record written before #1195 added the field. It is the base the other two
// record-liveness readers are built on.
//
// It is the derivation the daemon's `--status` filter runs
// (daemon/snapshot_filter.go), and `liveness_name` is built from the SAME call,
// so the two agree by construction: a row the filter selects for a word is a row
// whose `liveness_name` is that word, legacy records included.
//
// It returns LivenessUnset — "this record records no liveness" — when the
// fallback has nothing to work with: a pre-#1195 snapshot caught mid-create or
// mid-kill, or a `status` integer from a newer af. Both then name themselves ""
// and are selected by no filter word, matching what an unrecognized `status` or
// `liveness` integer already does, instead of being labelled `ready` by
// LivenessForStatus's default and swept into `--status ready`.
//
// Deliberately NOT EffectiveLiveness/livenessFromData, which differ twice over.
// They rewrite a persisted Dead to Lost — a LOAD decision, since a dead record
// is recovery-eligible when an Instance is rebuilt from it (#1108) — and naming
// through that would print "lost" beside a `liveness` integer that reads 4 and
// would make `--status dead` select rows it had already renamed. They also must
// answer with a real state for every input, because an Instance has to load as
// something, so they keep the historical `ready` default this one refuses.
func RecordedLiveness(d InstanceData) Liveness {
	if d.Liveness != LivenessUnset {
		return d.Liveness
	}
	liveness, _ := statusLiveness(d.Status)
	return liveness
}

// MarshalJSON emits the stored record plus its derived state names.
//
// Deriving here rather than storing a field is what keeps `status_name` and
// `liveness_name` honest for every provenance — a live daemon projection, a
// record decoded from an older daemon, a legacy row read straight off disk by
// the daemonless fallback — instead of only for the rows this binary happened to
// build. Nothing reads the names back: they are absent from InstanceData's
// fields, so a decode ignores them and no stale copy can survive a round trip.
//
// instances.json gets the names too, since SaveInstances writes through this
// same encoder. That is not the projection leak ForStorage scrubs: that contract
// exists because a persisted FIELD is decoded back and can drive a decision
// after a restart (a stale lifecycle_action, a resurrected in-flight op). These
// are not fields — nothing decodes them, and every encode recomputes them from
// the integers beside them — so there is no stale value on disk for anything to
// act on.
//
// CAUTION for future callers: a struct that EMBEDS InstanceData inherits this
// method and would marshal as a bare session, silently dropping its own fields.
// api.sessionGetResult is the one embedder today and overrides MarshalJSON for
// exactly that reason.
func (d InstanceData) MarshalJSON() ([]byte, error) {
	// The alias drops the method set, so the nested Marshal cannot recurse.
	type alias InstanceData
	return json.Marshal(struct {
		alias
		StatusName   string `json:"status_name"`
		LivenessName string `json:"liveness_name"`
	}{
		alias:        alias(d),
		StatusName:   StatusName(d.Status),
		LivenessName: LivenessName(RecordedLiveness(d)),
	})
}

// MarshalJSON emits the stored tab plus its derived kind name, so `tabs[].kind`
// and `tab_kinds[].kind` finally spell the same concept the same way (#3631).
func (t TabData) MarshalJSON() ([]byte, error) {
	type alias TabData
	return json.Marshal(struct {
		alias
		KindName string `json:"kind_name"`
	}{
		alias:    alias(t),
		KindName: TabKindName(t.Kind),
	})
}

// AppendJSONMember splices one additional member into an already-encoded JSON
// object, preserving the object's existing key order.
//
// It exists for the embedding hazard above: a wrapper around a type with its own
// MarshalJSON cannot reach the wrapped type's encoder through struct embedding
// without losing its own fields, and re-encoding through a map would reorder
// every key of a public payload. name must be a plain member name; value must be
// valid JSON.
func AppendJSONMember(object []byte, name string, value []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(object)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return nil, fmt.Errorf("cannot add %q: encoded value is not a JSON object", name)
	}
	key, err := json.Marshal(name)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.Write(trimmed[:len(trimmed)-1])
	// An object with no members takes no separator. Tested on the INTERIOR rather
	// than on the length, so a pretty-printed "{\n}" is handled as the empty
	// object it is instead of gaining a leading comma that invalidates the result.
	if len(bytes.TrimSpace(trimmed[1:len(trimmed)-1])) > 0 {
		out.WriteByte(',')
	}
	out.Write(key)
	out.WriteByte(':')
	out.Write(value)
	out.WriteByte('}')
	return out.Bytes(), nil
}
