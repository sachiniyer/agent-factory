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
// policy always drops (session titles, task prompts, tab commands);
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
// a prefix (github_token, x-api-key, client_secret) and an optional opening
// quote; the value half is any run of 6+ non-space, non-quote characters.
var keyValueSecret = regexp.MustCompile(
	`(?i)("?[a-z0-9_-]*(?:api[_-]?key|secret|token|password|passwd|pwd|auth|access[_-]?token|refresh[_-]?token|client[_-]?secret|bearer|credential|private[_-]?key)s?"?\s*[:=]\s*"?)([^\s"',}\]]{6,})`)

// privateKeyBlock matches a PEM private-key block in its entirety.
var privateKeyBlock = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)

// redactor holds the per-run redaction context — the home directory to
// collapse to "~" and the username token(s) to blank to "[user]" — resolved
// once so every section scrubs against the same values. Constructed with
// newRedactor() in production; tests build one directly with fixed values for
// deterministic assertions.
type redactor struct {
	home  string
	users []string
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
	s = keyValueSecret.ReplaceAllString(s, "${1}"+secretMarker)
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

// redactInstancesJSON parses one repo's instances.json, applies the
// structural field-redaction policy to every record, re-marshals, and scrubs
// the result. The typed decode is intentional and fail-closed: any field the
// current InstanceData does not know about is dropped rather than passed
// through, so a future secret-bearing field cannot leak before the redactor is
// taught about it. If the payload does not decode as []InstanceData (corrupt
// or legacy shape), the raw text is scrubbed and returned so triage still gets
// something without risking a structural leak.
func (r *redactor) redactInstancesJSON(raw json.RawMessage) json.RawMessage {
	var datas []session.InstanceData
	if err := json.Unmarshal(raw, &datas); err != nil {
		return json.RawMessage(r.scrub(string(raw)))
	}
	for i := range datas {
		redactInstanceData(&datas[i])
	}
	out, err := json.MarshalIndent(datas, "", "  ")
	if err != nil {
		return json.RawMessage(r.scrub(string(raw)))
	}
	return json.RawMessage(r.scrub(string(out)))
}

// redactInstanceData blanks the free-text and arbitrary-payload fields of a
// single session record while leaving the structural triage fields (ids,
// liveness/status, program, timestamps, git SHAs, counts, flags) intact.
// Paths are left for the text scrub to collapse ($HOME→~, username→[user]).
func redactInstanceData(d *session.InstanceData) {
	if d.Title != "" {
		d.Title = redactedMarker
	}
	for i := range d.Tabs {
		if d.Tabs[i].Command != "" {
			d.Tabs[i].Command = redactedMarker
		}
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
