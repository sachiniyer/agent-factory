package bugreport

import (
	"encoding/json"
	"strings"

	"github.com/sachiniyer/agent-factory/session"
)

// titleJSONKeys are the object keys whose string value is a raw session title on
// the generic fallback path, mirroring the fields noteSession reads off a typed
// record (InstanceData.Title and Worktree.SessionName). `tmux_name` is handled
// separately — it is a name derived from the title, not the title itself.
var titleJSONKeys = map[string]bool{"title": true, "session_name": true}

// noteUnknownJSON walks a decoded-but-unparseable instances.json payload and
// records every title/tmux-name it carries, so scrubLog can strip them from the
// log tail exactly as it does for records that decoded typed (#1790). It also
// pairs titles with repo_path values solely to derive the title spelling in a
// sibling worktree path (#4099). It must run BEFORE redactUnknownJSON blanks
// those same sensitive values.
//
// The walk is key-driven and shape-agnostic, so it reaches nested title-bearing
// locations (worktree.session_name, tabs[].tmux_name) without assuming the
// record layout the typed decode already rejected.
func (r *redactor) noteUnknownJSON(v any) {
	// The typed shape is a top-level record list. Preserve that one trustworthy
	// ownership boundary even though a field inside made typed decoding fail; a
	// cross-product across records fabricates paths and grows quadratically.
	if records, ok := v.([]any); ok {
		for _, record := range records {
			r.noteUnknownJSONRecord(record)
		}
		return
	}
	r.noteUnknownJSONRecord(v)
}

func (r *redactor) noteUnknownJSONRecord(v any) {
	titles := make(map[string]struct{})
	repoPaths := make(map[string]struct{})
	var walk func(any)
	walk = func(value any) {
		switch t := value.(type) {
		case map[string]any:
			for k, val := range t {
				s, isString := val.(string)
				key := strings.ToLower(k)
				switch {
				case !isString:
					walk(val)
				case titleJSONKeys[key]:
					titles[s] = struct{}{}
					r.noteTitle(s)
				case key == "tmux_name":
					r.noteTmuxName(s)
				case key == "repo_path":
					repoPaths[s] = struct{}{}
				}
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(v)
	// repo_path is already a sensitive fallback key and is dropped below. Use
	// it only to reproduce the worktree layer's repo-dependent title bound; do
	// not register an untyped value as a path root or give it a structural role.
	for title := range titles {
		r.noteWorktreeSubdirectoryTitle(title)
		for repoPath := range repoPaths {
			r.noteWorktreeTitle(repoPath, title)
		}
	}
}

// unparsedInstancesNote is emitted (as a JSON string) when instances.json is
// not even valid JSON, so nothing sensitive is surfaced from a payload we
// cannot reason about at all.
const unparsedInstancesNote = `"[instances.json could not be parsed; contents omitted for safety]"`

// redactInstancesJSON parses one repo's instances.json, applies the structural
// field-redaction policy to every record, re-marshals, and scrubs the result.
// The typed decode is intentional and fail-closed: any field the current
// InstanceData does not know about is dropped rather than passed through, so a
// future secret-bearing field cannot leak before the redactor is taught about
// it.
//
// When the payload does NOT decode as []InstanceData (a corrupt or legacy
// shape — e.g. a field whose type has since changed), the typed field-level
// policy can't apply, so we redact MORE, not less (fail-safe — this bundle is
// shared publicly): a generic key-aware walk blanks every value under a
// known-sensitive key (prompts, commands, tokens, paths, arbitrary metadata)
// before the JSON-owned scalar scrub runs. If it is not even valid JSON, the
// contents are omitted entirely with a note. The fallback is never
// raw-with-regex-only — under-including beats leaking.
func (r *redactor) redactInstancesJSON(raw json.RawMessage) json.RawMessage {
	var datas []session.InstanceData
	if err := json.Unmarshal(raw, &datas); err == nil {
		// Two passes, exactly as redactTasks runs two: the first registers what
		// this payload knows — titles, tmux names, path roots — and the second
		// redacts against the COMPLETE set. Record order is then irrelevant, which
		// it is not in one pass: a session's path can sit under a root another
		// record introduces, and the root tokens would be numbered by whichever
		// record happened to be redacted first.
		for i := range datas {
			r.noteSession(&datas[i])
		}
		for i := range datas {
			r.redactInstanceData(&datas[i])
		}
		if out, marshalErr := json.MarshalIndent(datas, "", "  "); marshalErr == nil {
			return json.RawMessage(r.scrubJSON(string(out)))
		}
	}

	// Fallback: unknown/corrupt shape. Blank sensitive keys generically, then
	// scrub. Omit entirely if the payload is not valid JSON.
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return json.RawMessage(unparsedInstancesNote)
	}
	// Record the titles this payload carries before blanking them, so scrubLog
	// strips them from the log tail too — the typed path above does this via
	// noteSession, and without it a corrupt instances.json redacted the JSON
	// section while leaving bare titles in the bundled log (#1790).
	r.noteUnknownJSON(generic)
	out, err := json.MarshalIndent(redactUnknownJSON(generic), "", "  ")
	if err != nil {
		return json.RawMessage(unparsedInstancesNote)
	}
	return json.RawMessage(r.scrubJSON(string(out)))
}

// sensitiveJSONKeys are object keys whose values are dropped wholesale on the
// generic fallback path, where the typed field-level policy cannot apply. It is
// deliberately broad and fail-safe: on a shape we could not parse, a key that
// *might* hold free text, a secret, a path, or arbitrary metadata is redacted
// rather than trusted. Structural keys (id, status, timestamps, git SHAs,
// counts, flags) are absent here and so survive the walk (then get the text
// scrub for any residual root/$HOME/username/credential).
var sensitiveJSONKeys = map[string]bool{
	"title": true, "prompt": true, "prompts": true,
	"command": true, "cmd": true, "commands": true,
	"args": true, "argv": true, "arg": true,
	"env": true, "environment": true,
	"token": true, "tokens": true, "secret": true, "secrets": true,
	"password": true, "passwd": true, "pwd": true,
	"credential": true, "credentials": true,
	"api_key": true, "apikey": true, "key": true, "keys": true,
	"auth": true, "authorization": true, "bearer": true,
	"private_key": true, "url": true,
	"path": true, "home": true, "repo_path": true, "worktree_path": true,
	// The rest of #3588's register, mirrored onto this path the way every entry
	// below mirrors its own typed redaction. A record the typed decode REJECTS
	// must never be less private than one it accepts, and each of these is
	// dropped wholesale rather than given the typed treatment because the typed
	// treatment needs a shape this path by definition could not parse:
	//
	//   - alternate_path is the relocation pathname the typed policy collapses to
	//     a root token; there is no record here to take a root from.
	//   - archive_warning embeds the user-chosen skipped file names, and the
	//     grammar that separates them from the prose is the renderer's, not this
	//     payload's.
	//   - name is a tab name, user-chosen. The enum words beside it
	//     (kind_name/status_name/liveness_name) and branch_name are DIFFERENT
	//     keys, matched exactly, so they still survive.
	//   - account is the user-chosen credential-account label.
	//   - account_scope is the per-tab account label redactTabData blanks
	//     (the same fact InstanceData.Account is redacted for, under the #4506
	//     process-tab-restore feature). The registry text sweep only reaches
	//     labels still in r.accounts, so a renamed/retired account label in a
	//     record that fails the typed decode leaks here without this entry.
	//   - program and runtime_program are arbitrary command lines. Program was
	//     listed as structural here until #3588 established it is not;
	//     runtime_program is the override-resolved form of the same value.
	//   - error is af-authored diagnostic text that quotes tmux and git, so it
	//     names titles and worktrees; the typed path scrubs it, and scrubbing
	//     needs the typed record's titles.
	"alternate_path": true, "archive_warning": true,
	"name": true, "account": true, "account_scope": true, "program": true, "runtime_program": true, "error": true,
	// The usage-limit swap's account labels (#3127), mirrored here for the reason
	// every entry above is: a record the typed decode REJECTS must never be less
	// private than one it accepts. "account" already covers the label nested in
	// each account_limit_observations entry, because that key is matched exactly
	// wherever it appears; these two are the spellings it does not reach.
	//
	// pending_account_swap is dropped WHOLESALE rather than per-field, which is
	// this path's rule for an object whose shape it could not parse — the same
	// call remote_meta and the teardown union get. It costs nothing here: the
	// marker replaces the VALUE and leaves the KEY, so "a committed swap was
	// still awaiting delivery" survives for triage while the two labels and the
	// resumable conversation id inside it do not.
	"limit_account": true, "pending_account_swap": true,
	// account_agent is the namespace the account label was selected in (#4430).
	// The typed path keeps it only when it is one of af's agent names and marks
	// anything else; this path cannot make that distinction, so it always
	// masks — a record the typed decode rejected must not publish a value the
	// accepted record would have hidden (#4703's account_scope parity rule).
	"account_agent": true,
	// path_bytes is the durable form of a path that is not valid UTF-8, and JSON
	// carries it BASE64-ENCODED. Blanking "path" alone left the real name in the
	// bundle in a form the closing text scrub cannot recognize as a path, a home
	// directory, or a username — strictly worse than the plain field it stands
	// in for. It is the fallback's copy of the typed clearing in
	// redactInstanceData (#3541).
	"path_bytes":  true,
	"remote_meta": true,
	// A typed record drops this storage-only teardown union in
	// redactInstanceData. Drop the whole object on the generic fallback too: a
	// malformed sibling field must not expose its SSH host/user/key paths, hook
	// command, remote session directory, or container id.
	"runtime_cleanup": true,
	// tmux_name and session_name mirror the typed-path redaction
	// (redactInstanceData drops TmuxName, Worktree.SessionName,
	// Tabs[].TmuxName, and PendingTabCleanup[].TmuxName): each carries the
	// free-text session title. Without them the fallback path leaked titles
	// the typed path already scrubs, including the nested tabs[].tmux_name
	// and worktree.session_name the recursive walk below reaches (#1680).
	// This walk is key-driven and depth-agnostic, which is why it kept
	// covering pending_tab_cleanup[].tmux_name for the whole window the typed
	// path did not (#2776) — the fallback is the broader net by design.
	"tmux_name": true, "session_name": true,
	// conversation and agent_conversation mirror the typed-path redaction
	// (redactInstanceData clears Tabs[].Conversation.ID and
	// AgentConversation.ID): the provider conversation id resumes an agent
	// session and must not ship in a publicly shared bundle. The whole object
	// is dropped rather than just its "id" — on this path the shape is by
	// definition one we could not parse, so a legacy record may carry the id
	// as a bare string, under a differently-named key, or nested deeper, and
	// an id-only rule would miss every such variant. The surviving typed
	// fields (agent, captured_at) are not worth reconstructing a shape
	// contract for here (#1839).
	"conversation": true, "agent_conversation": true,
	// handoffs and from close the same gap on this path that #3405 closed on the
	// typed one: a handoff ledger entry holds the outgoing agent's conversation
	// under the key "from", which neither "conversation" above nor any other key
	// here matched, so an unparseable record leaked exactly the id the typed path
	// had been taught to clear. handoffs drops the ledger wholesale, matching how
	// runtime_cleanup and conversation are handled — on a shape af could not
	// parse, the entry's own layout is not something to rely on. from is the
	// belt: the same object under a legacy or renamed container still goes.
	"handoffs": true, "from": true,
	// pending_handoff_mission mirrors the typed-path redaction (redactInstanceData
	// blanks PendingHandoffMission): the rendered takeover brief embeds the user's
	// free-text prompt/goal verbatim, the same sensitivity class as prompt. A
	// record that fails the typed decode must still drop it (#2419).
	"pending_handoff_mission": true,
}

// redactUnknownJSON recursively rebuilds a decoded JSON value, blanking any
// value whose object key is in sensitiveJSONKeys and recursing everywhere else.
// Non-container leaves are returned unchanged (the caller text-scrubs them).
func redactUnknownJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if sensitiveJSONKeys[strings.ToLower(k)] {
				out[k] = redactedMarker
				continue
			}
			out[k] = redactUnknownJSON(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = redactUnknownJSON(e)
		}
		return out
	default:
		return v
	}
}
