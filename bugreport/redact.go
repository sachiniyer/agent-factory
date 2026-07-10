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
var keyValueSecret = regexp.MustCompile(
	`(?i)(["']?[a-z0-9_-]*(?:api[_-]?key|secret|token|password|passwd|pwd|auth|access[_-]?token|refresh[_-]?token|client[_-]?secret|bearer|credential|private[_-]?key)s?["']?\s*[:=]\s*)(?:"(?:\\.|[^"\\\r\n])*"|'[^'\r\n]*'|[^\s"',}\]]{6,})`)

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

// RedactPath collapses $HOME to ~ and the username to [user] in a single path,
// using the same rules as the bundle redactor. The command layer runs the
// written bundle's path through it before inlining the path into the GitHub
// issue-draft body, so the (public) draft can't leak $HOME/username even though
// the bundle itself is redacted.
func RedactPath(p string) string {
	return newRedactor().scrub(p)
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

// bareTitleRegexp compiles a word-boundary matcher for a bare session title, or
// nil when the title is too short (< 4 chars) to redact without risking mangling
// unrelated log text. Best-effort by design — the tmux-name redaction above is
// the primary defense; this catches raw titles the log prints outside a name.
func bareTitleRegexp(title string) *regexp.Regexp {
	title = strings.TrimSpace(title)
	if len(title) < 4 {
		return nil
	}
	re, err := regexp.Compile(`\b` + regexp.QuoteMeta(title) + `\b`)
	if err != nil {
		return nil
	}
	return re
}

// noteSession records a session's tmux name(s) and raw title(s) before they are
// redacted, so scrubLog can strip them from the log tail. Called on each record
// while collecting instances, i.e. before collectLog runs.
func (r *redactor) noteSession(d *session.InstanceData) {
	if r.tmuxNames == nil {
		r.tmuxNames = make(map[string]struct{})
	}
	if r.titles == nil {
		r.titles = make(map[string]struct{})
	}
	if d.TmuxName != "" {
		r.tmuxNames[d.TmuxName] = struct{}{}
	}
	if strings.TrimSpace(d.Title) != "" {
		r.titles[d.Title] = struct{}{}
	}
	if strings.TrimSpace(d.Worktree.SessionName) != "" {
		r.titles[d.Worktree.SessionName] = struct{}{}
	}
	for _, tab := range d.Tabs {
		if tab.TmuxName != "" {
			r.tmuxNames[tab.TmuxName] = struct{}{}
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
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		return prefix + `"` + secretMarker + `"`
	}
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		return prefix + `'` + secretMarker + `'`
	}
	return prefix + secretMarker
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
	if len(d.RemoteMeta) > 0 {
		// Preserve the *signal* that remote metadata existed without emitting
		// any of its (arbitrary, possibly secret) contents.
		d.RemoteMeta = map[string]interface{}{"_redacted": true}
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
