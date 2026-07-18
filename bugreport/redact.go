package bugreport

import (
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// Redaction markers. `redactedMarker` replaces a whole free-text field the
// policy always drops (session titles, session prompts, task prompts, tab commands);
// `secretMarker` replaces a substring a best-effort pattern flagged as a
// credential inside otherwise-kept text (log lines, config values).
const (
	redactedMarker = "[redacted]"
	secretMarker   = "[redacted-secret]"
	userMarker     = "[user]"
)

// secretPatterns are targeted, high-confidence credential shapes scrubbed
// wherever they appear (log tail, config, instance/task text). They are
// deliberately specific — a broad "any long hex string" rule would also nuke
// the git SHAs and IDs a triager needs, so those are left intact. Best-effort
// by construction; the bundle always warns the user to review before sharing.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),                                     // OpenAI / Anthropic-style keys (incl. sk-ant-…)
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),                                // GitHub PAT / OAuth / server / refresh tokens
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),                              // GitHub fine-grained PAT
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),                              // Slack tokens
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),                                          // AWS access key id
	regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`),                                     // Google API key
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]+`), // JWT (header.payload.signature)
}

// keyValueSecret matches a `<credential-key> = <value>` / `<key>: <value>`
// assignment and redacts only the value, preserving the key so triage can see
// *that* a credential is configured without leaking it. The key half tolerates
// a prefix (github_token, x-api-key, client_secret) and optional quotes. The
// value half recognizes TOML/JSON-style double-quoted strings, TOML literal
// single-quoted strings, and bare token-like values.
//
// THE BARE CLASS MUST NOT EXCLUDE `]`. It used to, which meant a bare value
// stopped BEFORE a `]` instead of at a real terminator — so the captured text
// was not the value, only a prefix of it. Everything downstream inherited that
// lie: `api_key=[redacted-secret]actualcredential` captured just
// `[redacted-secret`, which looks exactly like a marker this redactor wrote, and
// the credential rode out untouched behind it. The bug was never in the
// comparison, so no guard on top of the capture could fix it.
//
// The value now ends only at a genuine terminator — whitespace, a quote, `,`,
// `}`, or end of text — so what the regex hands back IS the whole bare value,
// and comparing it to a marker is a real comparison. Values carrying structural
// characters are covered by the quoted alternatives, which consume their own
// delimiters. Dropping `]` also errs toward MORE redaction (a `]` adjacent to a
// bare value is absorbed rather than left behind), which is the safe direction.
var keyValueSecret = regexp.MustCompile(
	`(?i)(["']?[a-z0-9_-]*(?:api[_-]?key|secret|token|password|passwd|pwd|auth|access[_-]?token|refresh[_-]?token|client[_-]?secret|bearer|credential|private[_-]?key)s?["']?\s*[:=]\s*)(?:"(?:\\.|[^"\\\r\n])*"|'[^'\r\n]*'|[^\s"',}]{6,})`)

// privateKeyBlock matches a PEM private-key block in its entirety.
var privateKeyBlock = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)

// afTmuxSessionName matches a repo-scoped af tmux session name
// (af_<8 hex>_<title>, incl. any __tab / _paste suffix). The <title> segment is
// the sanitized, whitespace-stripped session title, so it leaks the same
// free-text name the structured redactor already drops from InstanceData.TmuxName
// — but the daemon log tail is bundled verbatim and prints these names on nearly
// every line (e.g. "af_0f8fc14c_fix-1436"), reintroducing the #1533 leak class
// through the log blob (#1584). The title segment is a run of non-whitespace,
// non-':' characters: titles never contain whitespace (stripped at
// sanitization) and never contain ':' (rewritten to '_'), so ':' — a tmux
// window/pane ref or log delimiter — cleanly bounds the name without ever
// truncating a real title mid-way and leaving a fragment behind. Keys on the
// name *shape*, so it scrubs archived/killed sessions no live set still knows.
var afTmuxSessionName = regexp.MustCompile(`af_[0-9a-f]{8}_[^\s:]+`)

// redactor holds the per-run redaction context — the home directory to
// collapse to "~" and the username token(s) to blank to "[user]" — resolved
// once so every section scrubs against the same values. Constructed with
// newRedactor() in production; tests build one directly with fixed values for
// deterministic assertions.
type redactor struct {
	home  string
	users []string
	// tmuxNames and titles are the known session tmux names and raw session
	// titles gathered while redacting instances (see noteSession). scrubLog uses
	// them to redact bare titles and non-repo-scoped names (af_<title>, no hash,
	// which the afTmuxSessionName shape can't match) out of the verbatim log
	// tail — closing the #1584 leak the structured sections don't reach.
	tmuxNames map[string]struct{}
	titles    map[string]struct{}
}

// newRedactor resolves the redaction context from the environment: the OS
// home directory and the current username (plus the home directory's base
// name, which is the username on a conventional layout).
func newRedactor() *redactor {
	home, _ := os.UserHomeDir()
	var users []string
	if u, err := user.Current(); err == nil {
		users = appendUserToken(users, u.Username)
	}
	if home != "" {
		users = appendUserToken(users, filepath.Base(home))
	}
	return &redactor{home: home, users: users}
}

// appendUserToken adds a username token to the scrub list, skipping empties,
// path-ish junk, and tokens under 3 chars (too short to replace safely without
// mangling unrelated substrings).
func appendUserToken(users []string, name string) []string {
	name = strings.TrimSpace(name)
	if len(name) < 3 || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return users
	}
	for _, existing := range users {
		if existing == name {
			return users
		}
	}
	return append(users, name)
}

// scrub is the catch-all text pass applied to every section: it removes PEM
// blocks and pattern-matched credentials, collapses the home directory to "~",
// and blanks bare username tokens to "[user]". It runs last over already
// field-redacted content, so it is defense-in-depth, not the only line of
// defense.
func (r *redactor) scrub(s string) string {
	s = privateKeyBlock.ReplaceAllString(s, secretMarker)
	s = keyValueSecret.ReplaceAllStringFunc(s, redactKeyValueSecret)
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, secretMarker)
	}
	if r.home != "" && r.home != "/" {
		s = strings.ReplaceAll(s, r.home, "~")
	}
	for _, name := range r.users {
		re := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`)
		s = re.ReplaceAllString(s, userMarker)
	}
	return s
}

// scrubLog scrubs the daemon log tail. On top of the standard scrub() pass it
// redacts the free-text <title> in every af_<hash>_<title> tmux session name and
// any bare session title the log prints, so the verbatim log blob can't leak the
// session titles the structured sections already drop (#1584 — the exact #1533
// class, reintroduced through the bundled log). Call this instead of scrub() for
// the log section; it ends by delegating to scrub() for the usual
// $HOME/username/secret pass.
func (r *redactor) scrubLog(s string) string {
	// Redact the title in every af_<hash>_<title> name. Keys on the name shape,
	// so it catches current AND historical (archived/killed) sessions the live
	// instance set no longer references.
	s = afTmuxSessionName.ReplaceAllStringFunc(s, redactAFTmuxTitle)
	// Non-repo-scoped names (af_<title>, no hash) don't match the shape above;
	// redact those known names exactly.
	for name := range r.tmuxNames {
		if !afTmuxSessionName.MatchString(name) {
			s = strings.ReplaceAll(s, name, tmuxPrefixMarker)
		}
	}
	// Bare raw titles the log prints verbatim (e.g. via a %q-formatted Title).
	// Best-effort: only titles long enough to redact without mangling unrelated
	// words, matched on word boundaries.
	for title := range r.titles {
		if re := bareTitleRegexp(title); re != nil {
			s = re.ReplaceAllString(s, redactedMarker)
		}
	}
	return r.scrub(s)
}

// tmuxPrefixMarker is the redaction of an af tmux session name whose title
// segment is removed but whose "af_" prefix is kept so the line still reads as
// referring to an af session.
const tmuxPrefixMarker = "af_" + redactedMarker

// redactAFTmuxTitle redacts the <title> of a matched af_<8 hex>_<title> name,
// keeping the fixed, user-text-free "af_<hash>_" prefix (3 + 8 + 1 = 12 chars).
func redactAFTmuxTitle(match string) string {
	return match[:12] + redactedMarker
}

// bareTitleRegexp compiles a boundary-anchored matcher for a bare session title,
// or nil when the title is too short (< 4 chars) to redact without risking
// mangling unrelated log text. Best-effort by design — the tmux-name redaction
// above is the primary defense; this catches raw titles the log prints outside a
// name.
//
// A `\b` anchor is only emitted on the edge where the title's own boundary
// character is a word char ([A-Za-z0-9_]); `\b` matches only at a word↔non-word
// transition, so anchoring an edge whose title char is itself non-word (e.g. the
// trailing `]` of "client[prod]") never matches and the title leaks (#1639). The
// per-edge anchor still guards word-char edges against partial-word mangling
// (title "test" won't match inside "testing") while a non-word edge is already
// self-delimiting and needs no anchor.
func bareTitleRegexp(title string) *regexp.Regexp {
	title = strings.TrimSpace(title)
	if len(title) < 4 {
		return nil
	}
	var left, right string
	if isWordByte(title[0]) {
		left = `\b`
	}
	if isWordByte(title[len(title)-1]) {
		right = `\b`
	}
	re, err := regexp.Compile(left + regexp.QuoteMeta(title) + right)
	if err != nil {
		return nil
	}
	return re
}

// isWordByte reports whether b is a regex word character ([A-Za-z0-9_]), i.e. a
// byte across which `\b` forms a boundary. ASCII-only, matching RE2's default
// `\b` semantics.
func isWordByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}

// noteSession records a session's tmux name(s) and raw title(s) before they are
// redacted, so scrubLog can strip them from the log tail. Called on each record
// while collecting instances, i.e. before collectLog runs.
func (r *redactor) noteSession(d *session.InstanceData) {
	r.noteTmuxName(d.TmuxName)
	r.noteTitle(d.Title)
	r.noteTitle(d.Worktree.SessionName)
	for _, tab := range d.Tabs {
		r.noteTmuxName(tab.TmuxName)
	}
}

// noteTitle records one raw session title for scrubLog, skipping blanks.
func (r *redactor) noteTitle(title string) {
	if strings.TrimSpace(title) == "" {
		return
	}
	if r.titles == nil {
		r.titles = make(map[string]struct{})
	}
	r.titles[title] = struct{}{}
}

// noteTmuxName records one raw tmux session name for scrubLog, skipping blanks.
func (r *redactor) noteTmuxName(name string) {
	if name == "" {
		return
	}
	if r.tmuxNames == nil {
		r.tmuxNames = make(map[string]struct{})
	}
	r.tmuxNames[name] = struct{}{}
}

// titleJSONKeys are the object keys whose string value is a raw session title on
// the generic fallback path, mirroring the fields noteSession reads off a typed
// record (InstanceData.Title and Worktree.SessionName). `tmux_name` is handled
// separately — it is a name derived from the title, not the title itself.
var titleJSONKeys = map[string]bool{"title": true, "session_name": true}

// noteUnknownJSON walks a decoded-but-unparseable instances.json payload and
// records every title/tmux-name it carries, so scrubLog can strip them from the
// log tail exactly as it does for records that decoded typed (#1790). It must
// run BEFORE redactUnknownJSON blanks those same values.
//
// The walk is key-driven and shape-agnostic, so it reaches nested title-bearing
// locations (worktree.session_name, tabs[].tmux_name) without assuming the
// record layout the typed decode already rejected.
func (r *redactor) noteUnknownJSON(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			s, isString := val.(string)
			key := strings.ToLower(k)
			switch {
			case !isString:
				r.noteUnknownJSON(val)
			case titleJSONKeys[key]:
				r.noteTitle(s)
			case key == "tmux_name":
				r.noteTmuxName(s)
			}
		}
	case []any:
		for _, e := range t {
			r.noteUnknownJSON(e)
		}
	}
}

func redactKeyValueSecret(match string) string {
	idx := keyValueSecret.FindStringSubmatchIndex(match)
	if len(idx) < 4 || idx[2] < 0 {
		return secretMarker
	}
	prefix := match[idx[2]:idx[3]]
	value := match[idx[3]:]
	// A value an earlier pass already redacted must survive untouched. scrub is
	// applied more than once to the same text by design — per section, again over
	// the assembled text/JSON, and again on each component the issue draft inlines
	// — so it has to be idempotent. It was not: re-scrubbing a marker re-wrapped
	// it and grew a bracket per pass, and a real bundle shipped 28
	// `[redacted-secret]]`.
	//
	// This skip is only safe because `value` is the COMPLETE value; see
	// redactionMarkerValues for why, and keyValueSecret for the boundary that
	// makes it true.
	if isRedactionMarker(value) {
		return match
	}
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		return prefix + `"` + secretMarker + `"`
	}
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		return prefix + `'` + secretMarker + `'`
	}
	return prefix + secretMarker
}

// redactionMarkerValues are the EXACT, COMPLETE value forms this redactor emits,
// and nothing else. isRedactionMarker is a fast-path AROUND the scrub, and a
// fast-path around a redactor is sound only if it recognizes precisely what that
// redactor produces — anything looser is a way for a real credential to reach a
// public bundle unscrubbed.
//
// Every entry is a whole value, which is what makes the comparison sound. That
// is a property of keyValueSecret, not of this map: each alternative in its value
// half now ends at a genuine terminator (see the regex comment), so the captured
// text is the entire value —
//
//	bare      `[redacted-secret]`   ends at whitespace/quote/`,`/`}`/EOS
//	bare      `[redacted]`          ditto
//	quoted    `"[redacted-secret]"` the alternative consumes both quotes
//	quoted    `'[redacted-secret]'` ditto
//	quoted    `"[redacted]"`        ditto
//	quoted    `'[redacted]'`        ditto
//
// — so a value that merely BEGINS with a marker (`[redacted-secret]hunter2`,
// `"[redacted-secret]hunter2"`) is captured in full, matches no entry here, and
// takes the normal redacting path. It cannot reach the unchanged path.
//
// Derived from the marker constants so they cannot drift if a marker is reworded.
var redactionMarkerValues = map[string]bool{
	secretMarker:               true,
	redactedMarker:             true,
	`"` + secretMarker + `"`:   true,
	`'` + secretMarker + `'`:   true,
	`"` + redactedMarker + `"`: true,
	`'` + redactedMarker + `'`: true,
}

// isRedactionMarker reports whether value is EXACTLY a marker an earlier scrub
// pass wrote, so re-scrubbing it would only re-wrap it. Exact match against a
// COMPLETE value — never a prefix, never a substring, and never a truncated
// capture: a value this redactor did not write must take the normal path.
func isRedactionMarker(value string) bool {
	return redactionMarkerValues[value]
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
// before the text scrub runs. If it is not even valid JSON, the contents are
// omitted entirely with a note. The fallback is never raw-with-regex-only —
// under-including beats leaking.
func (r *redactor) redactInstancesJSON(raw json.RawMessage) json.RawMessage {
	var datas []session.InstanceData
	if err := json.Unmarshal(raw, &datas); err == nil {
		for i := range datas {
			r.noteSession(&datas[i])
			redactInstanceData(&datas[i])
		}
		if out, marshalErr := json.MarshalIndent(datas, "", "  "); marshalErr == nil {
			return json.RawMessage(r.scrub(string(out)))
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
	return json.RawMessage(r.scrub(string(out)))
}

// sensitiveJSONKeys are object keys whose values are dropped wholesale on the
// generic fallback path, where the typed field-level policy cannot apply. It is
// deliberately broad and fail-safe: on a shape we could not parse, a key that
// *might* hold free text, a secret, a path, or arbitrary metadata is redacted
// rather than trusted. Structural keys (id, status, program, timestamps, git
// SHAs, counts, flags) are absent here and so survive the walk (then get the
// text scrub for any residual $HOME/username/credential).
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
	"remote_meta": true,
	// tmux_name and session_name mirror the typed-path redaction
	// (redactInstanceData drops TmuxName, Worktree.SessionName, and
	// Tabs[].TmuxName): each carries the free-text session title. Without
	// them the fallback path leaked titles the typed path already scrubs,
	// including the nested tabs[].tmux_name and worktree.session_name the
	// recursive walk below reaches (#1680).
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

// redactInstanceData blanks the free-text and arbitrary-payload fields of a
// single session record while leaving the structural triage fields (ids,
// liveness/status, program, timestamps, git SHAs, counts, flags) intact.
// Paths are left for the text scrub to collapse ($HOME→~, username→[user]).
func redactInstanceData(d *session.InstanceData) {
	if d.Title != "" {
		d.Title = redactedMarker
	}
	if d.Prompt != "" {
		d.Prompt = redactedMarker
	}
	if d.Worktree.SessionName != "" {
		d.Worktree.SessionName = redactedMarker
	}
	// TmuxName is derived from the session title (e.g. "af_ConfidentialDeal"),
	// so it leaks the same free-text name Title carries and must be redacted too.
	if d.TmuxName != "" {
		d.TmuxName = redactedMarker
	}
	for i := range d.Tabs {
		if d.Tabs[i].Command != "" {
			d.Tabs[i].Command = redactedMarker
		}
		if d.Tabs[i].TmuxName != "" {
			d.Tabs[i].TmuxName = redactedMarker
		}
		// A web tab's URL is user-supplied (any http/https target passes
		// NormalizeWebTabURL) and can name internal infrastructure or a private
		// repo — the same class of sensitive URL PRInfo.URL is redacted for
		// below (#1954). Redact non-loopback targets; keep loopback ones (the
		// proxied dev-server case), which are safe and useful for triage,
		// mirroring the loopback/non-loopback split the daemon proxy already
		// draws (session.IsLoopbackWebTarget).
		if d.Tabs[i].URL != "" && !session.IsLoopbackWebTarget(d.Tabs[i].URL) {
			d.Tabs[i].URL = redactedMarker
		}
		if d.Tabs[i].Conversation != nil {
			d.Tabs[i].Conversation.ID = ""
		}
	}
	if d.AgentConversation != nil {
		d.AgentConversation.ID = ""
	}
	if d.PRInfo.Title != "" {
		d.PRInfo.Title = redactedMarker
	}
	if d.PRInfo.URL != "" {
		d.PRInfo.URL = redactedMarker
	}
}

// redactedTask is the structural, secret-free projection of a task.Task. The
// prompt and watch command — both free-text that can carry secrets — collapse
// to a marker (and a boolean recording that one was present); everything else
// is scheduling metadata safe to keep. ProjectPath survives here and is
// scrubbed for $HOME/username by the text pass.
type redactedTask struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	HasPrompt     bool   `json:"has_prompt"`
	Prompt        string `json:"prompt,omitempty"`
	CronExpr      string `json:"cron_expr,omitempty"`
	HasWatchCmd   bool   `json:"has_watch_cmd"`
	WatchCmd      string `json:"watch_cmd,omitempty"`
	TargetSession string `json:"target_session,omitempty"`
	ProjectPath   string `json:"project_path,omitempty"`
	Program       string `json:"program,omitempty"`
	Enabled       bool   `json:"enabled"`
	LastRunStatus string `json:"last_run_status,omitempty"`
}

// redactTask maps a task.Task to its redacted projection.
func redactTask(t task.Task) redactedTask {
	rt := redactedTask{
		ID:            t.ID,
		Name:          t.Name,
		CronExpr:      t.CronExpr,
		TargetSession: t.TargetSession,
		ProjectPath:   t.ProjectPath,
		Program:       t.Program,
		Enabled:       t.Enabled,
		LastRunStatus: t.LastRunStatus,
	}
	if strings.TrimSpace(t.Prompt) != "" {
		rt.HasPrompt = true
		rt.Prompt = redactedMarker
	}
	if strings.TrimSpace(t.WatchCmd) != "" {
		rt.HasWatchCmd = true
		rt.WatchCmd = redactedMarker
	}
	return rt
}
