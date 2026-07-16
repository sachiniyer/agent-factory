// Package bugreport builds `af bug-report`: a single, self-contained,
// best-effort-redacted diagnostics bundle a user can attach to a GitHub issue.
// It gathers the daemon log tail, versions, the configured tasks, the session
// state (instances.json), the daemon health snapshot, and the global config,
// then runs every free-text and secret-bearing field through the redactor in
// redact.go. Redaction is best-effort by design — the bundle always carries a
// "review before sharing" warning — but the policy is conservative: structural
// triage fields are kept, free-text bodies are dropped, and $HOME/username and
// known credential shapes are scrubbed everywhere.
package bugreport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/task"
)

const (
	// logTailMaxBytes bounds how much of the (possibly large) log we read —
	// the tail is what matters for triage and the bundle must stay pasteable.
	logTailMaxBytes = 2 << 20 // 2 MiB
	// logTailMaxLines further caps the tail after the byte read so a log of
	// very long lines still yields a bounded, readable slice.
	logTailMaxLines = 5000

	// reviewWarning is repeated at the top of the text bundle and carried in
	// the JSON so no consumer can miss it: redaction is best-effort.
	reviewWarning = "REVIEW THIS BUNDLE BEFORE SHARING. Redaction is best-effort — " +
		"paths, prompts, and secrets are scrubbed on a conservative default, but " +
		"perfect redaction is impossible. Open it, read it, and remove anything " +
		"sensitive before attaching it to a public issue."
)

// Inputs are the already-collected, caller-supplied facts the bugreport
// package cannot gather without importing package main. The command layer
// resolves the af version and the daemon status (it owns collectDaemonStatus)
// and passes them in; everything else this package reads itself.
type Inputs struct {
	// AFVersion is the `af` binary version (main.version).
	AFVersion string
	// GeneratedAt is a caller-formatted timestamp for the bundle header.
	GeneratedAt string
	// DaemonStatus is the structured `af daemon status` snapshot, embedded
	// verbatim in the JSON manifest (its socket/pid paths are scrubbed by the
	// final text pass).
	DaemonStatus any
	// DaemonHuman is the pre-rendered human `af daemon status` text for the
	// text bundle's daemon section.
	DaemonHuman string
	// BundlePath is where the caller will write the text bundle. It is resolved
	// BEFORE the build so the issue draft can measure the real path into the
	// body it size-checks, instead of carrying a placeholder the caller swaps
	// for a longer string afterwards. It is redacted here, not by the caller.
	BundlePath string
}

// Versions is the version/platform block of the bundle.
type Versions struct {
	AF   string `json:"af"`
	Go   string `json:"go"`
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

// repoInstances is one repo's redacted session records.
type repoInstances struct {
	RepoID    string          `json:"repo_id"`
	Instances json.RawMessage `json:"instances"`
}

// configSection is the redacted global config file, or nil when none exists.
type configSection struct {
	Path     string `json:"path"`
	Format   string `json:"format"`
	Contents string `json:"contents"`
}

// logSection is the redacted, bounded daemon log tail.
type logSection struct {
	Path      string `json:"path"`
	Truncated bool   `json:"truncated"`
	Bytes     int    `json:"bytes"`
	Contents  string `json:"contents"`
}

// Bundle is the structured manifest emitted by `--json` and the source the
// text bundle is rendered from.
type Bundle struct {
	GeneratedAt string          `json:"generated_at"`
	Warning     string          `json:"warning"`
	Versions    Versions        `json:"versions"`
	Daemon      any             `json:"daemon"`
	Tasks       []redactedTask  `json:"tasks"`
	Instances   []repoInstances `json:"instances"`
	Config      *configSection  `json:"config,omitempty"`
	Log         logSection      `json:"log"`
	// Errors records non-fatal collection failures (an unreadable section)
	// so a partial bundle is still useful and the gaps are explicit.
	Errors []string `json:"errors,omitempty"`

	// daemonHuman is the pre-rendered human daemon-status text used by the
	// text renderer. Unexported so it never appears in the JSON manifest (the
	// structured Daemon snapshot is the machine-readable form there).
	daemonHuman string
	// bundlePath is where the caller will write the text bundle, for the issue
	// draft's attach instructions. Unexported: it is a local path, of no use to
	// a --json consumer, and the draft is its only reader.
	bundlePath string
}

// Result is what Build returns: a rendered text bundle (the file the user
// attaches), the JSON manifest payload (the `--json` data member), and the
// pre-filled GitHub issue-draft title/body the default flow opens in the
// browser. Title/Body are already-redacted (built only from redacted Bundle
// facts + a static template) and bounded to fit an issues/new URL — the FULL
// bundle still never rides in the URL, but a bounded excerpt of the key
// diagnostics does, so a submitted draft carries triage signal even when the
// user never attaches the file. Body is FINAL: it needs no post-processing by
// the caller, so nothing can grow it past the size it was checked against.
type Result struct {
	Text  string
	JSON  []byte
	Title string
	Body  string
}

// Build collects, redacts, and renders the bundle. It never fails on a missing
// or unreadable section — those are recorded in Bundle.Errors and rendering
// continues — so a user on a broken install can still produce a report.
func Build(in Inputs) (Result, error) {
	r := newRedactor()
	b := Bundle{
		GeneratedAt: in.GeneratedAt,
		Warning:     reviewWarning,
		Versions: Versions{
			AF:   in.AFVersion,
			Go:   runtime.Version(),
			OS:   runtime.GOOS,
			Arch: runtime.GOARCH,
		},
		Daemon:      in.DaemonStatus,
		daemonHuman: in.DaemonHuman,
		bundlePath:  in.BundlePath,
	}

	b.Tasks, b.Errors = collectTasks(b.Errors)
	b.Instances, b.Errors = collectInstances(r, b.Errors)
	b.Config, b.Errors = collectConfig(r, b.Errors)
	b.Log, b.Errors = collectLog(r, b.Errors)

	jsonBytes, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return Result{}, fmt.Errorf("marshal bug-report bundle: %w", err)
	}
	// Final catch-all pass: scrubs $HOME/username in the passed-in daemon
	// socket/pid paths and any residue. Idempotent over the per-section
	// scrubbing already applied, and safe for JSON (replacements never
	// introduce characters that need escaping).
	jsonBytes = []byte(r.scrub(string(jsonBytes)))

	text := r.scrub(renderText(b))
	title, body := buildIssueDraft(r, b)

	return Result{Text: text, JSON: jsonBytes, Title: title, Body: body}, nil
}

// Issue-draft size budget.
//
// The draft reaches GitHub as an issues/new URL — built by the command layer,
// or by `gh --web`, which constructs the same URL — and GitHub rejects one past
// roughly 8KB. The body rides in the query string, so what has to fit is the
// PERCENT-ENCODED body, not its raw length: a newline costs 3 bytes encoded and
// a log tail is newline-dense, so a raw cap would silently overshoot the URL.
//
// url.QueryEscape maps each byte independently, so encoded length is additive
// over concatenation — the fixed template can be measured first and the log tail
// handed exactly the remainder (see fitLogTail). maxIssueBodyEncodedBytes leaves
// ~2KB of the ~8KB for the scheme, host, path, and the encoded title.
const maxIssueBodyEncodedBytes = 6000

// Per-section sub-budgets. EVERY variable-length section is bounded by ENCODED
// length before it is written, not after: a section measured only once it is
// already in the body cannot be un-written, so an unbounded one (a broken
// install's collection errors, a verbose daemon status) would blow the total cap
// no matter how hard the log tail is trimmed afterwards. Together with the fixed
// template these leave the log tail the remainder, and head+foot alone can never
// reach maxIssueBodyEncodedBytes.
const (
	// issueDaemonEncodedBudget bounds the daemon-status block. The human status
	// is ~6 lines today; this guards a future verbose status crowding out the
	// log tail.
	issueDaemonEncodedBudget = 900
	// issueErrorsEncodedBudget bounds the whole collection-errors block. The
	// errors are the highest-signal part of a broken-install report, so they are
	// fitted before the log tail claims what is left — but a broken install is
	// exactly where errors are long (unreadable config paths, parser messages),
	// so the block is bounded, not merely counted.
	issueErrorsEncodedBudget = 800
	// issueErrorEncodedMax bounds ONE error, so a single pathological message
	// cannot consume the whole errors block.
	issueErrorEncodedMax = 200
	// issueLogMaxLines bounds the inlined log tail by lines as well as bytes:
	// the byte budget alone would inline hundreds of short lines, which reads
	// as noise in an issue. The full tail is in the attached bundle.
	issueLogMaxLines = 40
)

// logElidedNote is appended to the inlined log tail whenever lines were dropped
// to fit the budget. Its cost is always reserved, so the note can never be what
// pushes the body over the cap.
const logElidedNote = "\n_Earlier lines elided to fit the issue URL — the attached bundle has the full tail._\n"

// buildIssueDraft renders the pre-filled GitHub issue-draft title and body from
// the Bundle.
//
// The full bundle (a megabyte of log + session state) cannot inline in an
// issues/new URL, and before #1914 that meant the draft carried NO diagnostics
// at all — only counts plus a path to a file on the reporter's own machine,
// which is useless to a triager unless the user hand-attaches it. So the body
// embeds a BOUNDED excerpt of the highest-signal sections (daemon status,
// collection errors, the newest log lines) in a <details> block, and still
// points at the complete bundle on disk.
//
// THE INVARIANT: nothing may change the body's encoded length after the budget
// is computed.
//
// Three separate bugs in this change were one shape — measure, then mutate.
// Collection errors were written into the head before the budget was computed;
// the bundle path was substituted after the body was measured; the final scrub
// expanded prose after the log tail had already spent the budget. Each was
// "something changes the body after we decided it fits", and patching them one
// at a time just moved the next one somewhere else.
//
// So the order below is the invariant, not a convention:
//
//  1. Build every piece.
//  2. Scrub every piece — head, foot, log, and the log chrome. Nothing reaches
//     the body unscrubbed, so this doubles as the redaction chokepoint.
//  3. Measure the scrubbed pieces and hand the log tail the remainder.
//  4. Concatenate. No transformation runs over the finished body.
//
// Step 2 before step 3 is what makes it structurally true rather than checked:
// there is no later pass left that could grow anything. The returned body is a
// fixed point of the redactor — scrubbing it again is a no-op — which is exactly
// the property a test can assert, and does.
//
// REDACTION. This body is a SECOND EXIT out of the bundle: unlike the text and
// JSON renderers it never passes through Build's final catch-all scrub, so
// anything reaching it must be redacted on the way in — a collector that
// "usually" scrubs is not enough, and one that missed an exit leaked a real
// $HOME path into a public draft (the no-log message; see collectLog). Step 2
// above is that guarantee: it scrubs the assembled head/foot wholesale, so an
// inline that forgets cannot leak, while the per-component scrubs keep the
// sub-budgets honest.
//
// b.bundlePath is measured INTO the body rather than substituted afterwards by
// the caller: a placeholder swapped out post-measurement made the final body
// longer than the one that was checked against the cap.
func buildIssueDraft(r *redactor, b Bundle) (title, body string) {
	title = fmt.Sprintf("af bug-report: %s on %s/%s", b.Versions.AF, b.Versions.OS, b.Versions.Arch)

	// The daemon state is read off the human status text the command layer
	// pre-renders. With no status text there is nothing to read, so say so
	// rather than defaulting to "running" and asserting a health the bundle
	// never established.
	daemonHuman := strings.TrimSpace(r.scrub(b.daemonHuman))
	daemonState := "running"
	switch {
	case daemonHuman == "":
		daemonState = "unknown"
		daemonHuman = "(status unavailable)"
	case strings.Contains(strings.ToLower(daemonHuman), "not running"):
		daemonState = "not running"
	}
	daemonHuman, daemonTruncated := truncateEncoded(daemonHuman, issueDaemonEncodedBudget)
	if daemonTruncated {
		daemonHuman += "\n" + truncatedNote
	}

	var headRaw strings.Builder
	fmt.Fprintf(&headRaw, "<!-- Opened by `af bug-report`. This is a DRAFT — review it, attach the bundle below, then submit. -->\n\n")
	fmt.Fprintf(&headRaw, "## Environment\n")
	fmt.Fprintf(&headRaw, "- af: %s\n", b.Versions.AF)
	fmt.Fprintf(&headRaw, "- go: %s\n", b.Versions.Go)
	fmt.Fprintf(&headRaw, "- os/arch: %s/%s\n", b.Versions.OS, b.Versions.Arch)
	fmt.Fprintf(&headRaw, "- daemon: %s\n", daemonState)
	fmt.Fprintf(&headRaw, "- sessions: %d across %d repo(s)\n", countInstances(b.Instances), len(b.Instances))
	fmt.Fprintf(&headRaw, "- tasks: %d\n\n", len(b.Tasks))
	fmt.Fprintf(&headRaw, "## What happened\n<!-- Describe the bug. -->\n\n")
	fmt.Fprintf(&headRaw, "## What you expected\n\n")
	fmt.Fprintf(&headRaw, "## Steps to reproduce\n\n")
	fmt.Fprintf(&headRaw, "---\n\n")
	fmt.Fprintf(&headRaw, "<details>\n<summary>%s</summary>\n\n", issueSummaryLabel)
	writeFenced(&headRaw, "### Daemon status", daemonHuman)
	writeErrors(&headRaw, r, b.Errors)

	var footRaw strings.Builder
	fmt.Fprintf(&footRaw, "</details>\n\n---\n\n")
	fmt.Fprintf(&footRaw, "The summary above is a bounded excerpt. The COMPLETE best-effort **redacted** "+
		"bundle (full log tail, sessions, tasks, config) was written to:\n\n")
	fmt.Fprintf(&footRaw, "    %s\n\n", b.bundlePath)
	fmt.Fprintf(&footRaw, "**Attach that file to this issue** (drag-and-drop) before submitting. "+
		"Open and review it first — redaction is best-effort and cannot catch everything.\n")

	// ---- Scrub EVERY piece, then measure. See the invariant note above. ----
	//
	// The wholesale head/foot pass is the single redaction chokepoint: it covers
	// the static template as well as every component, so an inline that forgets
	// to scrub cannot leak. It runs HERE, before the budget, rather than over the
	// finished body — over the body it could grow text the budget had already
	// spent (a username of "the" expands every "the" in the prose above to
	// "[user]"), and the draft would blow the cap it had just been checked
	// against. Scrubbing components individually as well keeps the per-section
	// sub-budgets honest; that is free, because scrub is idempotent.
	head := r.scrub(headRaw.String())
	foot := r.scrub(footRaw.String())
	logAll := r.scrubLog(b.Log.Contents)

	// Size the log fence from the WHOLE (scrubbed) tail before budgeting. A fence
	// is closed by any run of at least its own length, so it must outrun every
	// backtick run in the text it wraps — but its own cost has to be reserved
	// before the text is fitted. Sizing it against the full tail sidesteps that
	// circularity: the fitted tail is a subset of it, so its longest run can only
	// be shorter, and the fence stays valid while the budget stays exact.
	fence := fenceFor(logAll)
	logHeader := r.scrub("### Daemon log tail (newest lines, redacted)\n\n" + fence + "\n")
	logFooter := r.scrub("\n" + fence + "\n\n")
	elidedNote := r.scrub(logElidedNote)

	budget := maxIssueBodyEncodedBytes -
		encodedLen(head) - encodedLen(foot) -
		encodedLen(logHeader) - encodedLen(logFooter) - encodedLen(elidedNote)
	logText, elided := fitLogTail(logAll, issueLogMaxLines, budget)

	// ---- Assembly is pure concatenation of finished pieces. Nothing below may
	// transform the body: every byte here has already been scrubbed and counted.
	var sb strings.Builder
	sb.WriteString(head)
	if logText != "" {
		sb.WriteString(logHeader)
		sb.WriteString(logText)
		sb.WriteString(logFooter)
	}
	if elided {
		sb.WriteString(elidedNote)
	}
	sb.WriteString(foot)
	return title, sb.String()
}

// truncatedNote marks a section the budget cut short, so a trimmed block never
// reads as complete.
const truncatedNote = "…(truncated; see the attached bundle)"

// writeErrors writes the collection-errors block, fitted to
// issueErrorsEncodedBudget. Each error is scrubbed and individually capped, and
// whatever did not fit is counted rather than dropped silently.
func writeErrors(sb *strings.Builder, r *redactor, errs []string) {
	if len(errs) == 0 {
		return
	}
	var block strings.Builder
	remaining := issueErrorsEncodedBudget
	shown := 0
	for _, e := range errs {
		line, _ := truncateEncoded(r.scrub(e), issueErrorEncodedMax)
		line = "- " + strings.ReplaceAll(line, "\n", " ") + "\n"
		cost := encodedLen(line)
		if cost > remaining {
			break
		}
		block.WriteString(line)
		remaining -= cost
		shown++
	}
	fmt.Fprintf(sb, "### Collection errors\n\n")
	sb.WriteString(block.String())
	if shown < len(errs) {
		fmt.Fprintf(sb, "- …and %d more (see the attached bundle)\n", len(errs)-shown)
	}
	sb.WriteByte('\n')
}

// writeFenced writes a titled code block whose fence outruns any backtick run in
// body, so nothing in body can close it early.
func writeFenced(sb *strings.Builder, title, body string) {
	fence := fenceFor(body)
	fmt.Fprintf(sb, "%s\n\n%s\n%s\n%s\n\n", title, fence, body, fence)
}

// fenceFor returns a backtick fence long enough that nothing in s can close it:
// CommonMark closes a fence only on a run of at least the same length, so the
// fence has to outrun the longest run in the content. A log line can carry a
// literal ``` (a hook echoing its own markdown output), which against a fixed
// three-backtick fence ended the block early and rendered the rest of the
// draft — the closing </details> and the attach instructions — as code.
func fenceFor(s string) string {
	longest, run := 0, 0
	for _, c := range s {
		if c == '`' {
			run++
			if run > longest {
				longest = run
			}
			continue
		}
		run = 0
	}
	if longest < 3 {
		return "```"
	}
	return strings.Repeat("`", longest+1)
}

// issueSummaryLabel names the collapsible diagnostics block. Tests key on it to
// assert the summary actually reached the draft.
const issueSummaryLabel = "Diagnostics summary (auto-generated, redacted, bounded excerpt)"

// fitLogTail returns the newest lines of the (already redacted) log tail that
// fit within both maxLines and budget encoded bytes, reporting whether any
// earlier line was dropped. It walks from the END because the lines nearest the
// failure are the ones that explain a bug, and stops at the first line that
// would overflow — a hard cap, never a best-effort overshoot. A budget too small
// for even one line yields no tail at all (elided=true) rather than a truncated,
// misleading fragment.
func fitLogTail(contents string, maxLines, budget int) (text string, elided bool) {
	contents = strings.TrimRight(contents, "\n")
	if strings.TrimSpace(contents) == "" {
		return "", false
	}
	lines := strings.Split(contents, "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
		elided = true
	}
	used, kept := 0, 0
	for i := len(lines) - 1; i >= 0; i-- {
		cost := encodedLen(lines[i]) + encodedLen("\n")
		if used+cost > budget {
			elided = true
			break
		}
		used += cost
		kept++
	}
	if kept == 0 {
		return "", true
	}
	return strings.Join(lines[len(lines)-kept:], "\n"), elided
}

// encodedLen reports how many bytes s costs once percent-encoded into the
// issues/new query string — the only length that matters for the URL cap.
func encodedLen(s string) int { return len(url.QueryEscape(s)) }

// truncateEncoded caps s at budget ENCODED bytes, cutting on a rune boundary and
// reporting whether anything was dropped. It walks runes and accumulates their
// encoded cost because that cost is wildly uneven — an ASCII letter is 1 byte, a
// newline 3, a multi-byte rune up to 12 — so a raw-length cap says nothing about
// how much of the URL budget a string actually spends.
func truncateEncoded(s string, budget int) (string, bool) {
	if encodedLen(s) <= budget {
		return s, false
	}
	used := 0
	for i, c := range s {
		cost := encodedLen(string(c))
		if used+cost > budget {
			return s[:i], true
		}
		used += cost
	}
	return s, false
}

// countInstances totals the session records across every repo's redacted
// instances payload, for the issue-draft environment summary. A payload that
// does not decode as an array contributes zero — the count is a best-effort
// triage hint, never load-bearing.
func countInstances(repos []repoInstances) int {
	total := 0
	for _, ri := range repos {
		var arr []json.RawMessage
		if err := json.Unmarshal(ri.Instances, &arr); err == nil {
			total += len(arr)
		}
	}
	return total
}

// collectTasks loads and redacts the configured tasks.
func collectTasks(errs []string) ([]redactedTask, []string) {
	tasks, err := task.LoadTasks()
	if err != nil {
		return nil, append(errs, fmt.Sprintf("tasks: %v", err))
	}
	out := make([]redactedTask, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, redactTask(t))
	}
	return out, errs
}

// collectInstances loads every repo's instances.json and redacts each, sorted
// by repo id for stable output.
func collectInstances(r *redactor, errs []string) ([]repoInstances, []string) {
	all, err := config.LoadAllRepoInstances()
	if err != nil {
		return nil, append(errs, fmt.Sprintf("instances: %v", err))
	}
	repoIDs := make([]string, 0, len(all))
	for id := range all {
		repoIDs = append(repoIDs, id)
	}
	sort.Strings(repoIDs)
	out := make([]repoInstances, 0, len(repoIDs))
	for _, id := range repoIDs {
		out = append(out, repoInstances{
			RepoID:    id,
			Instances: r.redactInstancesJSON(all[id]),
		})
	}
	return out, errs
}

// collectConfig reads the global config file (config.toml preferred, else
// config.json) and scrubs it. A missing file is not an error — many installs
// run on defaults — it just yields a nil section.
func collectConfig(r *redactor, errs []string) (*configSection, []string) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return nil, append(errs, fmt.Sprintf("config dir: %v", err))
	}
	for _, c := range []struct{ name, format string }{
		{config.TomlConfigFileName, "toml"},
		{config.ConfigFileName, "json"},
	} {
		path := filepath.Join(dir, c.name)
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				continue
			}
			errs = append(errs, fmt.Sprintf("config %s: %v", c.name, readErr))
			continue
		}
		return &configSection{
			Path:     path,
			Format:   c.format,
			Contents: r.scrub(string(data)),
		}, errs
	}
	return nil, errs
}

// collectLog reads and scrubs the bounded tail of the production log.
//
// EVERY exit must leave Contents redacted, not just the one that carries log
// text. The no-log message below interpolates the resolved log path — a real
// $HOME/config path — and used to be stored raw, surviving only because the text
// and JSON renderers happened to scrub once more at the end. The issue draft
// inlines Contents directly and never goes through that final pass, so the raw
// path reached a PUBLIC prefilled draft. The field is documented as redacted;
// it now is, on every path, for every consumer.
func collectLog(r *redactor, errs []string) (logSection, []string) {
	path := aflog.LogFilePath()
	sec := logSection{Path: path}
	if path == "" {
		return sec, append(errs, "log: could not resolve log path")
	}
	data, truncated, err := tailFile(path, logTailMaxBytes)
	if err != nil {
		if os.IsNotExist(err) {
			sec.Contents = r.scrub("(no log file at " + path + ")")
			return sec, errs
		}
		return sec, append(errs, fmt.Sprintf("log %s: %v", path, err))
	}
	text, lineTruncated := lastLines(string(data), logTailMaxLines)
	sec.Truncated = truncated || lineTruncated
	sec.Contents = r.scrubLog(text)
	sec.Bytes = len(sec.Contents)
	return sec, errs
}

// tailFile reads at most maxBytes from the end of path. When the file is
// larger, the read starts at a byte offset (so the first, likely-partial line
// is dropped by lastLines) and truncated is true.
func tailFile(path string, maxBytes int64) (data []byte, truncated bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	size := info.Size()
	if size <= maxBytes {
		buf, readErr := os.ReadFile(path)
		return buf, false, readErr
	}
	if _, err := f.Seek(size-maxBytes, 0); err != nil {
		return nil, false, err
	}
	buf := make([]byte, maxBytes)
	n, err := f.Read(buf)
	if err != nil {
		return nil, false, err
	}
	// Drop the partial first line left by the mid-file seek.
	if idx := bytes.IndexByte(buf[:n], '\n'); idx >= 0 && idx+1 <= n {
		return buf[idx+1 : n], true, nil
	}
	return buf[:n], true, nil
}

// lastLines keeps the final maxLines lines of s, reporting whether earlier
// lines were dropped.
func lastLines(s string, maxLines int) (string, bool) {
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s, false
	}
	return strings.Join(lines[len(lines)-maxLines:], "\n"), true
}
