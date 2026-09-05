package bugreport

import (
	"encoding/json"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/credscrub"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
)

// Redaction markers. `redactedMarker` replaces a whole free-text field the
// policy always drops (session titles, session prompts, task prompts, tab commands);
// `secretMarker` replaces a substring a best-effort pattern flagged as a
// credential inside otherwise-kept text (log lines, config values).
const (
	redactedMarker = credscrub.RedactedMarker
	secretMarker   = credscrub.SecretMarker
	userMarker     = "[user]"
)

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

// taskStartedInstanceTitle and taskParkedInstanceTitle recognize the two legacy
// daemon log shapes that rendered a raw session title with %s. New logs use %q,
// but a bundled tail can contain lines written by an older binary. Match the
// legacy field by its fixed surrounding syntax so punctuation-only titles can
// be removed without treating "." or "/" as a global search pattern. The parked
// form keeps its diagnostic suffix; the started form owns the rest of its line.
var (
	taskStartedInstanceTitle = regexp.MustCompile(`(?m)(task \S+ started successfully as instance )[^\r\n]+$`)
	taskParkedInstanceTitle  = regexp.MustCompile(`(?m)(task \S+ parked at a usage limit as instance )(.+)(; waiting for the limit window to reset)$`)
)

// redactor holds the per-run redaction context — the home directory to
// collapse to "~", the username token(s) to blank to "[user]", and the path
// roots to name — resolved once so every section scrubs against the same
// values. Constructed with newRedactor() in production; tests build one
// directly with fixed values for deterministic assertions.
type redactor struct {
	home  string
	users []string
	// tmuxNames and titles are the known session tmux names and raw session
	// titles gathered while redacting instances and tasks. scrubSessionTitles
	// uses titles for both structured task status strings and the verbatim log;
	// scrubLog additionally removes non-repo-scoped names (af_<title>, no hash,
	// which the afTmuxSessionName shape can't match) — closing the #1584 leak the
	// structured sections don't reach.
	tmuxNames map[string]struct{}
	titles    map[string]struct{}
	// accounts are the registered agent-account labels, read from the accounts
	// registry under the AF home once per run (see noteRegisteredAccounts). They
	// are gathered rather than derived from the records being redacted: a label
	// reaches the bundle through text no record owns — the daemon log tail, and
	// the config file that selects a default account — so the record set is not
	// the set that has to come out (#3871).
	accounts map[string]struct{}
	// roots are the absolute directories this run can name: the AF home, plus
	// each session's repo and worktree, gathered by noteSession before any record
	// is redacted. They are registered exactly the way titles are, and for the
	// same reason — the redactor already knows every instance it is redacting, so
	// it can name what those instances point at.
	roots []pathRoot
	// rootTokens dedupes registration by path. First token wins: a path
	// registered twice must not gain a second name.
	rootTokens map[string]string
	// repoRoots and worktreeRoots number the tokens within their kind.
	repoRoots     int
	worktreeRoots int
}

// newRedactor resolves the redaction context from the environment: the OS
// home directory, the current username (plus the home directory's base name,
// which is the username on a conventional layout), and the AF home.
//
// The AF home is resolved through config.GetConfigDir so this agrees with every
// other reader of AGENT_FACTORY_HOME. It matters most when that home is
// deliberately outside $HOME, which is exactly the layout the "$HOME collapses
// it" assumption fails on — but it is registered unconditionally, so a bundle
// names it the same way whatever the layout.
func newRedactor() *redactor {
	home, _ := os.UserHomeDir()
	var users []string
	if u, err := user.Current(); err == nil {
		users = appendUserToken(users, u.Username)
	}
	if home != "" {
		users = appendUserToken(users, filepath.Base(home))
	}
	r := &redactor{home: home, users: users}
	if afHome, err := config.GetConfigDir(); err == nil {
		r.noteAFHome(afHome)
	}
	return r
}

// appendUserToken adds a username token to the scrub list — BOTH the raw form and,
// when they differ, its lowercased form. The username scrub matches byte-exact, but
// the branch stored in instances.json is strings.ToLower(username)
// (config.BranchPrefix), so a mixed-case account ("Sachin.Iyer") would otherwise
// ship its lowercased branch prefix ("sachin.iyer/…") verbatim — the home-to-tilde
// collapse cannot catch it (different case, not a path). Registering both closes
// that leak for the same class as the non-word-boundary one (#2533).
func appendUserToken(users []string, name string) []string {
	name = strings.TrimSpace(name)
	for _, variant := range []string{name, strings.ToLower(name)} {
		users = addUserVariant(users, variant)
		// And the display spelling of each, for the same reason scrub() collapses
		// both spellings of $HOME: a username that is not valid UTF-8 reaches a
		// bundle through JSON, and through the archive warning's rewritten
		// retained root, as replacement characters. addUserVariant dedupes, so on
		// every ordinary account this adds nothing.
		users = addUserVariant(users, strings.ToValidUTF8(variant, "\uFFFD"))
	}
	return users
}

// addUserVariant adds one username spelling to the scrub list, skipping empties,
// path-ish junk, and tokens under 3 chars (too short to replace safely without
// mangling unrelated substrings), and deduping.
func addUserVariant(users []string, name string) []string {
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
// blocks and pattern-matched credentials, collapses every known path root — the
// AF home and each session's repo/worktree to its token, the home directory to
// "~" — and blanks bare username tokens to "[user]". It runs last over already
// field-redacted content, so it is defense-in-depth, not the only line of
// defense.
func (r *redactor) scrub(s string) string {
	s = credscrub.Scrub(s)
	s = r.collapseKnownRoots(s)
	// Account labels are swept HERE, in the catch-all, rather than beside the
	// title pass: a label reaches the bundle through two sections that share only
	// this function. collectLog routes the daemon log tail through scrubLog, which
	// ends by delegating here; collectConfig hands the global config file straight
	// to scrub() and touches no other pass. A sweep added next to the title pass
	// would cover the log and silently miss the config file that NAMES the default
	// account (#3871).
	//
	// It runs before the username pass for the reason that pass runs longest-first
	// within itself: a username that is a token-boundary prefix of a label would
	// otherwise consume the prefix, destroy the only exact match for the label, and
	// strand its suffix in the bundle. The account alphabet makes the reverse
	// impossible — a label never matches inside a longer run of label characters —
	// so this order is safe in both directions rather than a coin flip.
	s = r.scrubAccountLabels(s)
	// Blank bare username tokens with the SAME manual token boundary the title
	// scrub uses, not a `\b<name>\b` regex: a `\b` after the username never matches
	// when the username ends in a non-word rune (an OS username like "test-"), so
	// "test-/fix-login-bug" in a branch leaked the username unredacted — a silent
	// redaction failure in a bundle meant to be safe to share (#2533).
	//
	// Longest-first, exactly as scrubSessionTitles orders titles and for the same
	// reason: a shorter username can be a prefix of a longer one (a raw "jdoe" vs a
	// home basename "jdoe.admin"), and redacting the prefix first destroys the only
	// exact match for the longer token and strands its suffix. The manual boundary
	// makes that prefix-shadowing easier to hit than `\b` did, so the ordering is
	// part of the privacy invariant, not a nicety. Sort a copy so scrub stays a pure
	// read of r.users.
	names := append([]string(nil), r.users...)
	sortLongestFirst(names)
	for _, name := range names {
		s = replaceBareToken(s, name, userMarker)
	}
	return s
}

// scrubUnstructured is the single sanitizer for a free-text scalar or blob
// before it is embedded in any bug-report rendering. In addition to scrub's
// credential/path policy, it removes every known representation of a session
// title. Keeping this separate from scrub is intentional: scrub also runs over
// already-encoded JSON documents, where treating a short title such as "id" as
// bare text would rewrite structural keys. Call this while the value is still a
// value; all later text/JSON renderings then inherit the safe form.
//
// Account labels take the OPPOSITE trade and are swept by scrub() itself, keys
// and all. They have to be: the config file is handed to scrub() whole and shares
// no other pass, so leaving them out of it would leave the label in the section
// that names the default account. What that costs is over-redaction for an
// operator who names an account after a config key — visible in a file they are
// told to read, and the safe direction for an artifact meant to be shared
// (#3871).
func (r *redactor) scrubUnstructured(s string) string {
	return r.scrub(r.scrubKnownLabels(s))
}

// scrubLog scrubs the daemon log tail. On top of the standard scrub() pass it
// redacts the free-text <title> in every af_<hash>_<title> tmux session name and
// any bare session title the log prints, so the verbatim log blob can't leak the
// session titles the structured sections already drop (#1584 — the exact #1533
// class, reintroduced through the bundled log). Call this instead of scrub() for
// the log section; it ends by delegating to scrub() for the usual
// $HOME/username/secret pass.
func (r *redactor) scrubLog(s string) string {
	// The incomplete-archive warning goes first, because it is the one pass here
	// that reads the emitter's LITERAL PROSE, and every pass below rewrites text
	// anywhere it appears. A session titled "paths" is enough to break it: the
	// title pass rewrites the renderer's own "skipped paths:" label to
	// "skipped [redacted]:", the anchor is gone before this pass looks for it,
	// and every user file name in the list ships. It is also the ordering the
	// review on #3554 asked for from the other side — a title INSIDE a skipped path
	// ("secret" in "docs/secret-plan.txt") used to be rewritten first and strand
	// the rest of the name — and matching the whole quoted token makes that
	// impossible in either order.
	s = r.scrubArchiveWarningPaths(s)
	// Remove every known full title representation before any shape-based pass
	// can consume only part of it. In particular, the legacy raw task-start
	// matcher is line-oriented while a legal title may contain newlines; running
	// that matcher first replaced line one and made the original full-title match
	// impossible, leaking the remaining lines (#2249 late review).
	s = r.scrubKnownLabels(s)
	s = r.scrubTmuxNames(s)
	// Retain compatibility with the two legacy raw %s taskrun.go forms. Their
	// syntax is a safer boundary than a global punctuation matcher and also
	// catches historical task-created titles no longer present in instances.json.
	s = taskStartedInstanceTitle.ReplaceAllString(s, `${1}`+redactedMarker)
	s = taskParkedInstanceTitle.ReplaceAllString(s, `${1}`+redactedMarker+`${3}`)
	return r.scrub(s)
}

// scrubTmuxNames removes the free-text <title> from every af tmux session name
// in s. It is shared by scrubLog and scrubDiagnostic rather than inlined in
// either, because "a tmux name carries the title" is one fact: a diagnostic
// string that quotes tmux would otherwise reintroduce, field by field, exactly
// the #1584 leak the log pass closes.
func (r *redactor) scrubTmuxNames(s string) string {
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
	return s
}

// scrubDiagnostic sanitizes an af-AUTHORED diagnostic string that QUOTES an
// error from somewhere else — today, LostRestoreFailure.Error, which carries
// whatever tmux or git returned to the daemon's restore loop.
//
// Such a string is not free text a user typed, and blanking it costs triage the
// one thing it is there for: the reason automatic recovery stopped. But it is
// not af's own prose either. It gets the bundled log tail's treatment instead —
// session titles, the titles hiding inside tmux session names, every known path
// root, $HOME, the username, credential shapes — because the text it quotes
// names the same things the log does, and a second policy for it would drift
// from the first (#3588).
//
// Titles go FIRST, before the shape-based tmux pass, for the reason scrubLog
// orders them that way: a shape matcher that consumes part of a name makes the
// exact full-title match impossible afterwards.
func (r *redactor) scrubDiagnostic(s string) string {
	return r.scrub(r.scrubTmuxNames(r.scrubKnownLabels(s)))
}

// scrubSessionTitles removes exact Go-quoted forms of every known title, then
// applies the conservative word-bearing bare-title matcher. The quoted form is
// the important invariant for task targets: daemon delivery logs and persisted
// delivery errors both format them with %q. Matching strconv.Quote therefore
// covers every legal title byte-for-byte, including short names and punctuation
// that are unsafe to replace globally, plus quotes/backslashes that %q escapes
// (#2238 review). scrubLog handles legacy raw punctuation emitters by their
// fixed field syntax.
func (r *redactor) scrubSessionTitles(s string) string {
	titles := make([]string, 0, len(r.titles))
	for title := range r.titles {
		titles = append(titles, title)
	}
	// A shorter title may be a prefix of a longer one. Redacting the prefix
	// first destroys the only exact match for the longer secret and leaves its
	// suffix behind, so the order is part of the privacy invariant. The lexical
	// tie-break makes output deterministic even though titles are stored in a map.
	sortLongestFirst(titles)
	for _, title := range titles {
		s = strings.ReplaceAll(s, strconv.Quote(title), strconv.Quote(redactedMarker))
		s = replaceBareTitle(s, title)
	}
	return s
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

// noteSession records a session's tmux name(s) and raw title(s) before they are
// redacted, so scrubLog can strip them from the log tail. Called on each record
// while collecting instances, i.e. before collectLog runs.
func (r *redactor) noteSession(d *session.InstanceData) {
	r.noteTmuxName(d.TmuxName)
	r.noteTitle(d.Title)
	r.noteTitle(d.Worktree.SessionName)
	// The two roots this record points at. Registered here, beside the titles,
	// because they are the same kind of fact — something this run knows the name
	// of and every later pass must be able to recognize — and because collecting
	// them before ANY record is redacted is what lets one repo shared by several
	// sessions carry one token (#3588).
	r.noteRepoRoot(d.Worktree.RepoPath)
	r.noteWorktreeRoot(d.Worktree.WorktreePath)
	for _, tab := range d.Tabs {
		r.noteTmuxName(tab.TmuxName)
	}
	// PendingTabs is the durability staging area for metadata-only rows. It uses
	// the same TabData shape as Tabs, so its names need the same log treatment.
	for _, tab := range d.PendingTabs {
		r.noteTmuxName(tab.TmuxName)
	}
	// A pending-teardown handle names a tmux session the same way a live tab
	// does, and the retry logs it on every daemon start until the kill is
	// confirmed — so the log tail prints these names most often for exactly the
	// sessions whose teardown is stuck (#2776).
	for _, cleanup := range d.PendingTabCleanup {
		r.noteTmuxName(cleanup.TmuxName)
	}
}

// noteTitle records one raw session title for structured-string and log
// scrubbing, skipping blanks.
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
	//   - program is an arbitrary command line. It was listed as structural here
	//     until #3588 established it is not.
	//   - error is af-authored diagnostic text that quotes tmux and git, so it
	//     names titles and worktrees; the typed path scrubs it, and scrubbing
	//     needs the typed record's titles.
	"alternate_path": true, "archive_warning": true,
	"name": true, "account": true, "program": true, "error": true,
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

// redactInstanceData blanks the free-text and arbitrary-payload fields of a
// single session record while leaving the structural triage fields (ids,
// liveness/status, timestamps, git SHAs, counts, flags) intact.
//
// PATHS ARE NOT LEFT FOR THE TEXT SCRUB. That is what this function used to say,
// and it was true only for the common layout: scrub replaces r.home and the
// username tokens and nothing else, so a repo or worktree kept outside the home
// directory shipped its directory names verbatim (#3588). Every path field is
// rewritten HERE instead, against the roots noteSession registered — see
// collapsePathField for the three outcomes.
//
// It is a method for that reason: naming a path needs the run's roots, exactly
// as removing a title needs the run's titles. A caller that runs it without
// noteSession first gets the fail-safe end of the policy (an unknown root is a
// marker), never a leak.
func (r *redactor) redactInstanceData(d *session.InstanceData) {
	// A kill tombstone's storage-only cleanup handle can contain a private SSH
	// host/user/key path, a hook command path, or a container id. None is needed
	// to diagnose the session shape, and unlike ordinary snapshots instances.json
	// carries it specifically so teardown can resume after restart.
	d.RuntimeCleanup = nil
	if d.Title != "" {
		d.Title = redactedMarker
	}
	// Program is the session-level analogue of TabData.Command, which
	// redactTabData drops wholesale as user-supplied — and it is user-supplied in
	// exactly the same way: a program_overrides entry or `--program` is an
	// arbitrary command line, path and flags included (the root session on the
	// maintainer's box runs "/home/<user>/.local/bin/claude
	// --dangerously-skip-permissions"). Leaving one verbatim while blanking the
	// other was an inconsistency, not a decision (#3588).
	//
	// It does NOT collapse to the marker outright, because unlike a tab command
	// it carries one fact triage genuinely reads: WHICH AGENT ran. That fact is
	// recoverable from the command without keeping any of it —
	// DetectAgentFromCommand is the same seam every agent-conditional spawn keys
	// off, so what the bundle reports is what af itself decided this session was.
	// A command it cannot resolve to an agent has no safe part to keep, so it
	// takes Command's trade after all.
	d.Program = redactProgram(d.Program)
	// Account is the credential-account label a user picks (`--account work`),
	// free text that may name an employer or a client (#3051). Nothing else in
	// the pipeline touched it. The marker keeps the triage-relevant fact — an
	// account WAS in play, so the session did not run as the ambient identity —
	// and drops the label; an unset account stays unset, so redaction never
	// invents one (#3588).
	if d.Account != "" {
		d.Account = redactedMarker
	}
	if d.Prompt != "" {
		d.Prompt = redactedMarker
	}
	// PendingHandoffMission is a rendered takeover brief that embeds the user's
	// free-text prompt/goal verbatim — the same sensitivity class as Prompt. It
	// was added with transactional handoff (#2286) after this policy was written,
	// so it passed through unredacted into publicly shared bundles (#2419).
	if d.PendingHandoffMission != "" {
		d.PendingHandoffMission = redactedMarker
	}
	if d.Worktree.SessionName != "" {
		d.Worktree.SessionName = redactedMarker
	}
	// TmuxName is derived from the session title (e.g. "af_ConfidentialDeal"),
	// so it leaks the same free-text name Title carries and must be redacted too.
	if d.TmuxName != "" {
		d.TmuxName = redactedMarker
	}
	// A pending-teardown handle is a tmux name and nothing else, so it carries
	// the title exactly as TmuxName above does. It was added with durable tab
	// close (#2669) after this policy was written and inherited none of it, which
	// left the title in pending_tab_cleanup[].tmux_name of every bundle whose
	// instances.json decoded typed — the common case (#2776). The tab id beside
	// it is minted, never derived from user text, and survives for triage.
	for i := range d.PendingTabCleanup {
		if d.PendingTabCleanup[i].TmuxName != "" {
			d.PendingTabCleanup[i].TmuxName = redactedMarker
		}
	}
	for i := range d.Tabs {
		redactTabData(&d.Tabs[i])
	}
	// PendingTabs contains the same TabData shape under a separate ordering and
	// durability contract. Keeping one field redaction function prevents a row
	// from becoming less private merely because restore staged it (#3062).
	for i := range d.PendingTabs {
		redactTabData(&d.PendingTabs[i])
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
	// Path is the session's own absolute path. See collapsePathField.
	d.Path = r.collapsePathField(d.Path)
	d.Worktree.RepoPath = r.collapsePathField(d.Worktree.RepoPath)
	d.Worktree.WorktreePath = r.collapsePathField(d.Worktree.WorktreePath)
	if d.Worktree.RelocationRecovery != nil {
		d.Worktree.RelocationRecovery.AlternatePath =
			r.collapsePathField(d.Worktree.RelocationRecovery.AlternatePath)
	}
	// LostRestoreFailure.Error is af-authored — daemon/lostrestore.go stores the
	// terminal error of the automatic restore loop — but every constructor that
	// reaches it QUOTES an error from tmux or git, and those name the session's
	// tmux session (which is derived from its title) and its worktree. Blanking
	// it would cost triage the reason recovery stopped, so it gets the bundled
	// log tail's treatment instead (#3588). Attempts is a count and survives.
	if d.LostRestoreFailure != nil {
		d.LostRestoreFailure.Error = r.scrubDiagnostic(d.LostRestoreFailure.Error)
	}
	// ArchiveWarning is the bounded projection of ArchiveReport.Warning, and that
	// renderer prints the user-chosen names of the files af could not read.
	// #3554 closed the LOG path for exactly this text, but scrubArchiveWarningPaths
	// is reached only from scrubLog while redactInstancesJSON applies plain
	// scrub — so the same names still rode the JSON section of every bundle
	// (#3588). Routing the field through the same function rather than blanking it
	// keeps one policy for one string, and keeps the warning's SHAPE, which is
	// what triage reads: "af skipped 3 unreadable files", with a reason beside
	// each dropped name.
	d.ArchiveWarning = r.redactArchiveWarning(d.ArchiveWarning)
	// ArchiveReport.Skipped[].Path carries the relative file names a user chose
	// for files af could not read (hence permission_denied). They are relative to
	// the retained worktree root, so the text scrub cannot reach them — there is
	// no $HOME to collapse to "~" and no username to blank to "[user]", and the
	// policy above only covers paths the scrub can collapse. The report arrived
	// with lossless archive storage (#3171) after this policy was written, so a
	// name a user marked private (e.g. "credential", "private-work.txt",
	// "generated/private-019") shipped verbatim in every publicly shared bundle's
	// archive_report.retained_trees[].skipped[].path — the same class as the
	// title leaks in #2419 and #2776, a field added after the policy was written.
	// PathBytes is the durable form when Path is not valid UTF-8 and must be
	// cleared alongside it, or the raw name survives the display redaction. The
	// retained tree's own path is COLLAPSED rather than blanked (it is a system
	// path, and which root it hangs off is triage signal), and the skip reason
	// survives as the diagnostic ("permission denied on N files").
	if d.ArchiveReport != nil {
		for i := range d.ArchiveReport.RetainedTrees {
			// The tree's Path is a root-relative system path, so it takes the same
			// collapse every other path field does — NOT the "$HOME will reach it"
			// assumption this comment used to record, which held only while every
			// tree happened to sit under the home directory (#3588).
			//
			// Its PathBytes is not the same field twice: json emits it
			// base64-encoded, and no text pass can see through base64. A root whose
			// own name is not valid UTF-8 therefore shipped raw.
			//
			// Clearing PathBytes is not enough on its own: ArchiveRetainedTree's
			// MarshalJSON RE-DERIVES it from Path whenever it is empty, so a Path
			// carrying invalid UTF-8 would put the raw bytes straight back on the
			// wire. Reducing Path to its display form is what makes the clearing
			// hold, and it makes that an invariant of this function rather than an
			// inherited property of whoever decoded the record. It runs AFTER the
			// collapse, so an invalid-UTF-8 tail below a known root is normalized
			// too.
			tree := &d.ArchiveReport.RetainedTrees[i]
			tree.Path = strings.ToValidUTF8(r.collapsePathField(tree.Path), "\uFFFD")
			tree.PathBytes = nil
			for j := range tree.Skipped {
				tree.Skipped[j].Path = redactedMarker
				tree.Skipped[j].PathBytes = nil
			}
		}
		// The rollback fence carries the pre-projection relocation state, alternate
		// pathname included. It is the same value under a compatibility name, so it
		// is the same policy.
		if fence := d.ArchiveReport.RollbackFence; fence != nil && fence.OriginalRelocationRecovery != nil {
			fence.OriginalRelocationRecovery.AlternatePath =
				r.collapsePathField(fence.OriginalRelocationRecovery.AlternatePath)
		}
	}
}

// redactProgram reduces a resolved program command line to the agent it runs.
// Shared by the instance record and the task projection because it is one field
// under two owners; see the block in redactInstanceData for why the agent is
// kept and everything around it is not.
func redactProgram(program string) string {
	if program == "" {
		return ""
	}
	if agent := tmux.DetectAgentFromCommand(program); agent != "" {
		return agent
	}
	return redactedMarker
}

func redactTabData(tab *session.TabData) {
	// A tab name is user-chosen (`af sessions tab-create --name <tab>`) and
	// nothing in the pipeline could reach it: it is not a session title, so
	// scrubSessionTitles cannot know it, and it is not a path, so no root or
	// $HOME collapse applies. A tab named after a customer or an internal project
	// shipped verbatim (#3588). It carries no triage value the row does not
	// already have — the minted id names the tab and the kind says what it is —
	// so it takes the same wholesale drop Command does below.
	if tab.Name != "" {
		tab.Name = redactedMarker
	}
	if tab.Command != "" {
		tab.Command = redactedMarker
	}
	if tab.TmuxName != "" {
		tab.TmuxName = redactedMarker
	}
	// A web tab's URL is user-supplied (any http/https target passes
	// NormalizeWebTabURL) and can name internal infrastructure or a private
	// repo — the same class of sensitive URL PRInfo.URL is redacted for
	// below (#1954). External targets are dropped wholesale. For a loopback
	// dev-server, retain only the origin needed for triage: userinfo, paths,
	// queries and fragments can all carry credentials even though the host is
	// safe to name.
	if tab.URL != "" {
		if !session.IsLoopbackWebTarget(tab.URL) {
			tab.URL = redactedMarker
		} else if parsed, err := url.Parse(tab.URL); err != nil || parsed.Scheme == "" || parsed.Host == "" {
			tab.URL = redactedMarker
		} else {
			tab.URL = (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host}).String()
		}
	}
	if tab.Conversation != nil {
		tab.Conversation.ID = ""
	}
	// Handoffs[].From is the same AgentConversationData under a different name,
	// carrying an id with the same resume semantics — so it is the same policy,
	// not an adjacent one (#3405). The ledger arrived with agent handoff (#2013)
	// five days after the conversation-id policy was written and inherited none
	// of it, so every bundle from a session that had ever swapped agents shipped
	// the OUTGOING agent's resumable id while the incoming one was redacted
	// beside it.
	//
	// A loop over the whole ledger rather than a first/last entry: it is
	// append-only and unbounded, and "the ones we thought of" is not a policy.
	for i := range tab.Handoffs {
		tab.Handoffs[i].From.ID = ""
	}
}

// redactedTask is the structural, secret-free projection of a task.Task. The
// prompt and watch command — both free-text that can carry secrets — collapse
// to a marker (and a boolean recording that one was present). LastRunStatus is
// kept for diagnostics after known session titles are removed.
//
// ProjectPath keeps its shape rather than its name: redactTasks registers it as
// a repo root, and the projection collapses it to that root's token exactly as
// the instance path fields collapse theirs. It used to be documented as
// "scrubbed for $HOME/username by the text pass", which was the same
// unguaranteed assumption those fields carried — a project outside the home
// directory shipped its directory names (#3588).
//
// Program takes the same reduction as InstanceData.Program: the agent the
// command detects, or the marker when it detects none.
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

// redactTask maps a task.Task to its redacted projection. Recording the target
// here keeps both title defenses inseparable: the structured task field is
// dropped below, and scrubLog removes the same title from daemon log lines.
func (r *redactor) redactTask(t task.Task) redactedTask {
	r.noteTitle(t.TargetSession)
	rt := redactedTask{
		ID:            t.ID,
		Name:          t.Name,
		CronExpr:      t.CronExpr,
		ProjectPath:   r.collapsePathField(t.ProjectPath),
		Program:       redactProgram(t.Program),
		Enabled:       t.Enabled,
		LastRunStatus: r.scrubUnstructured(t.LastRunStatus),
	}
	if t.TargetSession != "" {
		rt.TargetSession = redactedMarker
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

// redactTasks first registers every current task target, then creates the
// secret-free projections. The two passes make task order irrelevant: one
// task's historical status may mention another task's current target.
func (r *redactor) redactTasks(tasks []task.Task) []redactedTask {
	for _, t := range tasks {
		r.noteTitle(t.TargetSession)
		// A task's project path is a repo path, registered the same way a
		// session's is so the two share one token when they name one repo.
		r.noteRepoRoot(t.ProjectPath)
	}
	out := make([]redactedTask, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, r.redactTask(t))
	}
	return out
}
