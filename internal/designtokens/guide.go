package designtokens

import (
	"bytes"
	"html/template"
	"os"
	"path/filepath"
	"strings"
)

type component struct {
	Name, Spec, WebStill, TUIStill, TUINote string
	Web                                     template.HTML // Authored static specimens, never external content.
	TUI                                     string
}

func components() []component {
	return []component{
		{"Rail", "Name first, one state label second. Selected row uses selection fill; focus has an outline and keyboard cue. Keep names reachable by search and keyboard; disclose branch and churn details on selection.", "dashboard", "demo-poster.png", "Recorder poster: sessions rail.", `<div class="sg-selected"><strong><span class="sg-ready">●</span> add-json-export</strong><br><small>Ready · Review changes</small></div><div>fix-empty-add<br><small>Running</small></div>`, "Sessions · 3\n› ● add-json-export · Ready\n    fix-empty-add · Running"},
		{"Header", "Project and active view provide context. Keep one primary create action. Connection status is separate from session liveness and always static.", "dashboard", "demo-poster.png", "Recorder poster: rail header and project context.", `<div class="sg-bar"><strong>todo-cli</strong><span class="sg-selected">Sessions</span><span>Tasks</span><span>Config</span><span>Connected</span></div>`, "todo-cli · Sessions · Connected\nKeyboard: navigation"},
		{"Terminal chrome", "Content owns the area. One title line names session, tab and keyboard owner. A focus outline does not imply ready or success. Preserve agent ANSI output and ctrl+] exit.", "agent-tab", "demo-poster.png", "Recorder poster: preview frame; agent output is not recoloured by this spec.", `<div class="sg-focus"><strong>tidy-tests · Agent · Keyboard</strong><pre>$ ./test.sh
2 tests passed</pre><small>ctrl+] · Return to sessions</small></div>`, "┌ tidy-tests · Agent · Keyboard ┐\n│ $ ./test.sh                  │\n│ 2 tests passed               │\n└ ctrl+] · Return to sessions ─┘"},
		{"Tabs and review", "Use label and underline for the active web tab; retain tab numbering and a selected tree row in the TUI. PR link stays beside the review context. Closing and splitting remain keyboard reachable.", "review", "demo-poster.png", "Recorder poster: selected child tab and review content.", `<div class="sg-bar"><span>Agent</span><strong class="sg-active">diff</strong><span>PR #128 · Open</span></div><pre>$ git diff --stat
2 files changed</pre>`, "  1 · Agent\n› 2 · diff · PR #128\n  2 files changed"},
		{"Dialogs and overlays", "One title, labelled fields, inline error and one primary action. Busy copy is static. Preserve entered text after failure, trap web focus and return it on close. TUI pickers share the same frame and esc return.", "new-session", "", "No recorder still of a TUI creation overlay is committed; web still documents the corresponding workflow.", `<div class="sg-dialog"><strong>New session</strong><label>Title<input value="tidy-tests" readonly></label><label>Prompt<textarea readonly>Cover appending a second item…</textarea></label><div class="sg-bar"><button type="button">Cancel</button><button type="button" class="sg-primary">Create</button></div></div>`, "╭ New session ───────────────╮\n│ Title: tidy-tests          │\n│ Prompt: Cover appending…   │\n│ enter create · esc cancel  │\n╰───────────────────────────╯"},
		{"Tasks", "Task name, enabled state and next occurrence lead; full trigger and delivery details expand on selection. Errors stay visible. Edit is primary; destructive actions belong in the selected task's actions.", "tasks", "tui-tasks.svg", "Existing Tasks SVG still; not a light/dark recorder pair.", `<div class="sg-selected"><strong>nightly-tests · Enabled</strong><br><small>Next run · Tomorrow at 12:00 UTC</small><br><button type="button">Edit task</button></div>`, "Tasks\n› nightly-tests · Enabled\n  Next run · Tomorrow at 12:00 UTC\n  enter edit · r run now · esc back"},
		{"Config and accounts", "Key, purpose and value form one group. Save feedback stays with that key. Accounts have their own section; login state is text, not a second session-liveness dot. Show daemon host before a truncatable path.", "config-accounts", "", "No recorder still of TUI Config is committed; inspect ui/config_pane.go and ui/config_pane_accounts.go.", `<strong>Config · Local daemon</strong><label>Editor binary<small>Program used by editor tabs</small><input value="code-server" readonly></label><small>Saved · Applies to new tabs</small><hr><strong>Accounts</strong><p>work · Logged in</p><button type="button">Add account</button>`, "Config · Local daemon\n› Editor binary: code-server\n  Saved · Applies to new tabs\nAccounts\n  work · Logged in"},
		{"Buttons, fields and menus", "A visible label survives placeholder removal. Primary, secondary, disabled and destructive states share geometry. Hover is optional; focus is visible. A disclosure offers project, filter or tab actions without a permanent toolbar.", "new-session", "", "No recorder still of TUI picker or menu states is committed.", `<label>Title<input class="sg-focus" value="tidy-tests" readonly></label><div class="sg-bar"><button class="sg-primary" type="button">Create</button><button type="button">Cancel</button><button disabled>Creating…</button><button class="sg-danger" type="button">Delete session</button></div><details><summary>Project · todo-cli</summary><p>todo-cli · Selected</p><p>Register project…</p></details>`, "Project\n› todo-cli · Selected\n  Register project…\nenter select · esc cancel\nCreating… · Please wait"},
		{"Empty states", "Name what is absent and one next action. Distinguish zero sessions from no project. Keep navigation available. Empty sections do not reserve permanent rows in the TUI.", "dashboard", "demo-poster.png", "Poster shows empty Automations and the surrounding workspace; no zero-session recorder still is committed.", `<strong>No sessions yet</strong><p>Create a session in todo-cli.</p><button class="sg-primary" type="button">New session</button><hr><strong>No project selected</strong><p>Choose a project to see its sessions.</p><button type="button">Choose project</button>`, "No sessions yet\nPress n to create a session.\n\nNo project selected\nChoose a project to continue."},
		{"Errors and notices", "Cause, consequence and one next action. Daemon unavailable and expired login must not masquerade as an empty list. Wrap actionable error text; retain recoverable input. Details are a disclosure, never the only explanation.", "config-accounts", "", "No recorder still of TUI errors is committed; inspect ui/err.go and app/home_view.go.", `<div class="sg-danger"><strong>Cannot reach the daemon</strong><p>Sessions could not be loaded. Check the daemon, then retry.</p></div><button type="button">Retry</button><hr><strong>Login expired</strong><p>Sign in again to reconnect.</p><button type="button">Sign in</button>`, "Cannot reach the daemon\nSessions could not be loaded.\nCheck the daemon, then retry.\n\nLogin expired · Sign in again"},
		{"Help and status bar", "Show only shortcuts valid for the current keyboard owner; full help remains discoverable. Narrow layouts keep the exit route first. No blinking cursor or progress animation in af chrome.", "agent-tab", "demo-poster.png", "Recorder poster: bottom shortcut strip.", `<div class="sg-bar"><kbd>ctrl+]</kbd><span>Return to sessions</span><kbd>?</kbd><span>Help</span></div><p>Connecting…</p>`, "ctrl+] return · ? help\nConnecting…"},
	}
}

func guidePage(root string, t Tokens) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(root, "design/style-guide.tmpl"))
	if err != nil {
		return nil, err
	}
	tmpl, err := template.New("guide").Funcs(template.FuncMap{
		"colorKeys": func() []string { return keys(t.Colors) },
		"valueKeys": func() []string { return keys(t.Values) },
		"themeLabel": func(mode string) string {
			if mode == "light" {
				return "Light"
			}
			return "Dark"
		},
		"tuiSample": func(sample string) template.HTML {
			lines := strings.Split(sample, "\n")
			for i, line := range lines {
				escaped := template.HTMLEscapeString(line)
				for _, state := range t.States {
					if state.Glyph != "" {
						escaped = strings.ReplaceAll(escaped, state.Glyph, `<span style="color:var(--af-`+state.Color+`)">`+state.Glyph+`</span>`)
					}
				}
				if strings.HasPrefix(line, "›") {
					escaped = `<span class="sg-selected">` + escaped + `</span>`
				}
				lines[i] = escaped
			}
			return template.HTML(strings.Join(lines, "\n"))
		},
	}).Parse(string(raw))
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	err = tmpl.Execute(&b, struct {
		Tokens     Tokens
		Themes     []string
		Components []component
	}{t, []string{"light", "dark"}, components()})
	return b.Bytes(), err
}
