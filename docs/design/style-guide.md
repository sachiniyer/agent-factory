# Interface style guide

The [interface design](interface-design.md) prescribes one treatment for each component.
There are **23 design tokens: 12 colour roles, five type steps, four spacing steps and
two radii**. The six liveness bindings are fixed semantics, including the empty running
glyph. The only user-facing theme choice is **Light / Dark / System**. These are internal
product constants, not user-editable tokens, presets or a custom-palette API.

## Beauty thesis · sessions exemplar

The [beauty thesis](thesis.md) takes the accepted TUI as the in-house benchmark
for expression with few means. Its exemplar changes only web Sessions: 28px
focused desktop identity, a 20px fleet heading, 16px row names and 13px metadata.
Agent/account and state are visible; repo/branch belong beside the focused work.
Raised navigation/identity chrome separates the base terminal surface using the
two existing fills. Semantic state colours carry attention alongside one accent.
Phone keeps its 48px session-first header and 44px keybar. No animation is added.
Shared CSS size changes do not change terminal cells or generated TUI paint.
Other web component recipes await the sequenced Phase 2 slices.

## Rules applied

Read the rule, then compare its light/dark web and TUI specimens with the current screen.
The specimens consume the generated CSS. TUI examples show the prescribed cell layout
and colours in HTML; they are not a running Bubble Tea app. Example controls are inert,
except native disclosure menus. System selects one of these same two themes.

The web stills show the redesigned product after P2, recorded with the repository's
[container recorder](../dev/demo-assets.md). TUI stills are real app-model Update/View
frames after P5, with deterministic fixtures in both themes; they are not live agent
recordings. The [TUI appearance gallery](tui-design-c-stills.md) covers the complete
matrix, including Light · Dark · System. Empty/error web specimens link to the
[P4 recovery matrix](recovery-stills.md). Click a still for the full screen.


<h3>Rail</h3>
<p>Use surface behind the rail, ink for names and ink-muted only for branch/time metadata. Selected rows use surface-raised plus an accent marker and bold name; the state keeps its own colour. Body type, space-2 row insets and space-1 glyph gaps are fixed. Show one label per liveness state; disclose mechanical detail on selection.</p>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="light"><h4>Web · Light</h4><div class="sg-state-row"><span class="sg-state-name">tidy-tests</span><span class="sg-state-label" style="color:var(--af-running)">Running</span></div><div class="sg-state-row sg-selected"><span class="sg-state-glyph" aria-hidden="true" style="color:var(--af-ready)">●</span><span class="sg-state-name">add-json-export</span><span class="sg-state-label" style="color:var(--af-ready)">Ready</span></div><div class="sg-state-row"><span class="sg-state-glyph" aria-hidden="true" style="color:var(--af-lost)">◌</span><span class="sg-state-name">remote-build</span><span class="sg-state-label" style="color:var(--af-lost)">Lost</span></div><div class="sg-state-row"><span class="sg-state-glyph" aria-hidden="true" style="color:var(--af-dead)">○</span><span class="sg-state-name">fix-empty-add</span><span class="sg-state-label" style="color:var(--af-dead)">Dead</span></div><div class="sg-state-row"><span class="sg-state-glyph" aria-hidden="true" style="color:var(--af-archived)">▧</span><span class="sg-state-name">document-cli</span><span class="sg-state-label" style="color:var(--af-archived)">Archived</span></div><div class="sg-state-row"><span class="sg-state-glyph" aria-hidden="true" style="color:var(--af-limit-reached)">◆</span><span class="sg-state-name">nightly-review</span><span class="sg-state-label" style="color:var(--af-limit-reached)">Limit reached</span></div></section>
<figure><a href="../../assets/web/dashboard.png"><img loading="lazy" src="../../assets/web/dashboard.png" alt="Current web dashboard screen in light theme"></a><figcaption>Rules applied · Real web screen · dashboard · light</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="light"><h4>TUI · Light</h4><pre>    tidy-tests · <span style="color:var(--af-running)">Running</span>
<span class="sg-selected">› <span style="color:var(--af-ready)">●</span> add-json-export · <span style="color:var(--af-ready)">Ready</span></span>
  <span style="color:var(--af-lost)">◌</span> remote-build · <span style="color:var(--af-lost)">Lost</span>
  <span style="color:var(--af-dead)">○</span> fix-empty-add · <span style="color:var(--af-dead)">Dead</span>
  <span style="color:var(--af-archived)">▧</span> document-cli · <span style="color:var(--af-archived)">Archived</span>
  <span style="color:var(--af-limit-reached)">◆</span> nightly-review · <span style="color:var(--af-limit-reached)">Limit reached</span></pre></section>
<figure><a href="../../assets/design/tui-c/sessions-dense-light.svg"><img loading="lazy" src="../../assets/tui/sessions-dense-light.png" alt="App-model driver · sessions dense · light"></a><figcaption>App-model driver · sessions dense Light · Regenerated after P2/P5.</figcaption></figure>
</div>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="dark"><h4>Web · Dark</h4><div class="sg-state-row"><span class="sg-state-name">tidy-tests</span><span class="sg-state-label" style="color:var(--af-running)">Running</span></div><div class="sg-state-row sg-selected"><span class="sg-state-glyph" aria-hidden="true" style="color:var(--af-ready)">●</span><span class="sg-state-name">add-json-export</span><span class="sg-state-label" style="color:var(--af-ready)">Ready</span></div><div class="sg-state-row"><span class="sg-state-glyph" aria-hidden="true" style="color:var(--af-lost)">◌</span><span class="sg-state-name">remote-build</span><span class="sg-state-label" style="color:var(--af-lost)">Lost</span></div><div class="sg-state-row"><span class="sg-state-glyph" aria-hidden="true" style="color:var(--af-dead)">○</span><span class="sg-state-name">fix-empty-add</span><span class="sg-state-label" style="color:var(--af-dead)">Dead</span></div><div class="sg-state-row"><span class="sg-state-glyph" aria-hidden="true" style="color:var(--af-archived)">▧</span><span class="sg-state-name">document-cli</span><span class="sg-state-label" style="color:var(--af-archived)">Archived</span></div><div class="sg-state-row"><span class="sg-state-glyph" aria-hidden="true" style="color:var(--af-limit-reached)">◆</span><span class="sg-state-name">nightly-review</span><span class="sg-state-label" style="color:var(--af-limit-reached)">Limit reached</span></div></section>
<figure><a href="../../assets/web/dashboard-dark.png"><img loading="lazy" src="../../assets/web/dashboard-dark.png" alt="Current web dashboard screen in dark theme"></a><figcaption>Rules applied · Real web screen · dashboard · dark</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="dark"><h4>TUI · Dark</h4><pre>    tidy-tests · <span style="color:var(--af-running)">Running</span>
<span class="sg-selected">› <span style="color:var(--af-ready)">●</span> add-json-export · <span style="color:var(--af-ready)">Ready</span></span>
  <span style="color:var(--af-lost)">◌</span> remote-build · <span style="color:var(--af-lost)">Lost</span>
  <span style="color:var(--af-dead)">○</span> fix-empty-add · <span style="color:var(--af-dead)">Dead</span>
  <span style="color:var(--af-archived)">▧</span> document-cli · <span style="color:var(--af-archived)">Archived</span>
  <span style="color:var(--af-limit-reached)">◆</span> nightly-review · <span style="color:var(--af-limit-reached)">Limit reached</span></pre></section>
<figure><a href="../../assets/design/tui-c/sessions-dense-dark.svg"><img loading="lazy" src="../../assets/tui/sessions-dense-dark.png" alt="App-model driver · sessions dense · dark"></a><figcaption>App-model driver · sessions dense Dark · Regenerated after P2/P5.</figcaption></figure>
</div>

<h3>Header</h3>
<p>Use surface with ink context and heading type; no raised toolbar. Mark the active view with an accent underline, and use ink-muted only for secondary connection detail. Use space-3 insets and space-2 between controls. Theme choice is exactly Light / Dark / System.</p>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="light"><h4>Web · Light</h4><div class="sg-bar"><strong>todo-cli</strong><span class="sg-active">Sessions</span><span>Tasks</span><span>Config</span><span>Connected</span></div></section>
<figure><a href="../../assets/web/dashboard.png"><img loading="lazy" src="../../assets/web/dashboard.png" alt="Current web dashboard screen in light theme"></a><figcaption>Rules applied · Real web screen · dashboard · light</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="light"><h4>TUI · Light</h4><pre>todo-cli · Sessions · Connected
Keyboard: navigation</pre></section>
<figure><a href="../../assets/design/tui-c/single-project-light.svg"><img loading="lazy" src="../../assets/tui/single-project-light.png" alt="App-model driver · single project · light"></a><figcaption>App-model driver · single project Light · Regenerated after P2/P5.</figcaption></figure>
</div>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="dark"><h4>Web · Dark</h4><div class="sg-bar"><strong>todo-cli</strong><span class="sg-active">Sessions</span><span>Tasks</span><span>Config</span><span>Connected</span></div></section>
<figure><a href="../../assets/web/dashboard-dark.png"><img loading="lazy" src="../../assets/web/dashboard-dark.png" alt="Current web dashboard screen in dark theme"></a><figcaption>Rules applied · Real web screen · dashboard · dark</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="dark"><h4>TUI · Dark</h4><pre>todo-cli · Sessions · Connected
Keyboard: navigation</pre></section>
<figure><a href="../../assets/design/tui-c/single-project-dark.svg"><img loading="lazy" src="../../assets/tui/single-project-dark.png" alt="App-model driver · single project · dark"></a><figcaption>App-model driver · single project Dark · Regenerated after P2/P5.</figcaption></figure>
</div>

<h3>Terminal chrome</h3>
<p>Use surface with ink title, heading type and border for an unfocused frame. Keyboard ownership adds the accent outline and the word Keyboard, never ready green. Use space-2 chrome insets; terminal content gets no decorative padding. Preserve agent ANSI output and ctrl&#43;] exit.</p>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="light"><h4>Web · Light</h4><div class="sg-focus"><strong>tidy-tests · Agent · Keyboard</strong><pre>$ ./test.sh
2 tests passed</pre><span>ctrl+] · Return to sessions</span></div></section>
<figure><a href="../../assets/web/agent-tab.png"><img loading="lazy" src="../../assets/web/agent-tab.png" alt="Current web agent-tab screen in light theme"></a><figcaption>Rules applied · Real web screen · agent-tab · light</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="light"><h4>TUI · Light</h4><pre>┌ tidy-tests · Agent · Keyboard ┐
│ $ ./test.sh                  │
│ 2 tests passed               │
└ ctrl+] · Return to sessions ─┘</pre></section>
<figure><a href="../../assets/design/tui-c/keyboard-light.svg"><img loading="lazy" src="../../assets/tui/keyboard-light.png" alt="App-model driver · keyboard · light"></a><figcaption>App-model driver · keyboard Light · Regenerated after P2/P5.</figcaption></figure>
</div>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="dark"><h4>Web · Dark</h4><div class="sg-focus"><strong>tidy-tests · Agent · Keyboard</strong><pre>$ ./test.sh
2 tests passed</pre><span>ctrl+] · Return to sessions</span></div></section>
<figure><a href="../../assets/web/agent-tab-dark.png"><img loading="lazy" src="../../assets/web/agent-tab-dark.png" alt="Current web agent-tab screen in dark theme"></a><figcaption>Rules applied · Real web screen · agent-tab · dark</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="dark"><h4>TUI · Dark</h4><pre>┌ tidy-tests · Agent · Keyboard ┐
│ $ ./test.sh                  │
│ 2 tests passed               │
└ ctrl+] · Return to sessions ─┘</pre></section>
<figure><a href="../../assets/design/tui-c/keyboard-dark.svg"><img loading="lazy" src="../../assets/tui/keyboard-dark.png" alt="App-model driver · keyboard · dark"></a><figcaption>App-model driver · keyboard Dark · Regenerated after P2/P5.</figcaption></figure>
</div>

<h3>Tabs and review</h3>
<p>Use surface and body-sized ink labels. Only the active tab gets an accent underline and bold text. Review branch changes in a process tab beside the agent tab. Use space-2 between tabs, no pill radii. TUI selection is a raised row and cursor. Preserve keyboard routes to close, switch and split.</p>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="light"><h4>Web · Light</h4><div class="sg-bar"><span>Agent</span><strong class="sg-active">diff</strong></div><pre>$ git diff --stat
2 files changed</pre></section>
<figure><a href="../../assets/web/review.png"><img loading="lazy" src="../../assets/web/review.png" alt="Current web review screen in light theme"></a><figcaption>Rules applied · Real web screen · review · light</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="light"><h4>TUI · Light</h4><pre>  1 · Agent
<span class="sg-selected">› 2 · diff</span>
  2 files changed</pre></section>
<figure><a href="../../assets/design/tui-c/pane-light.svg"><img loading="lazy" src="../../assets/tui/pane-light.png" alt="App-model driver · pane · light"></a><figcaption>App-model driver · pane Light · Regenerated after P2/P5.</figcaption></figure>
</div>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="dark"><h4>Web · Dark</h4><div class="sg-bar"><span>Agent</span><strong class="sg-active">diff</strong></div><pre>$ git diff --stat
2 files changed</pre></section>
<figure><a href="../../assets/web/review-dark.png"><img loading="lazy" src="../../assets/web/review-dark.png" alt="Current web review screen in dark theme"></a><figcaption>Rules applied · Real web screen · review · dark</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="dark"><h4>TUI · Dark</h4><pre>  1 · Agent
<span class="sg-selected">› 2 · diff</span>
  2 files changed</pre></section>
<figure><a href="../../assets/design/tui-c/pane-dark.svg"><img loading="lazy" src="../../assets/tui/pane-dark.png" alt="App-model driver · pane · dark"></a><figcaption>App-model driver · pane Dark · Regenerated after P2/P5.</figcaption></figure>
</div>

<h3>Dialogs and overlays</h3>
<p>Use surface-raised, border and radius-dialog with space-3 insets. The title uses type-title; body and field labels use ink at body size. Group fields with space-2 and separate the footer with space-4. One accent-filled primary button; inline failure uses dead. Preserve input and restore focus on close.</p>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="light"><h4>Web · Light</h4><div class="sg-dialog"><strong>New session</strong><label>Title<input value="tidy-tests" readonly></label><label>Prompt<textarea readonly>Cover appending a second item…</textarea></label><div class="sg-bar"><button type="button">Cancel</button><button type="button" class="sg-primary">Create</button></div></div></section>
<figure><a href="../../assets/web/new-session.png"><img loading="lazy" src="../../assets/web/new-session.png" alt="Current web new-session screen in light theme"></a><figcaption>Rules applied · Real web screen · new-session · light</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="light"><h4>TUI · Light</h4><pre>╭ New session ───────────────╮
│ Title: tidy-tests          │
│ Prompt: Cover appending…   │
│ enter create · esc cancel  │
╰───────────────────────────╯</pre></section>
<figure><a href="../../assets/design/tui-c/prompt-light.svg"><img loading="lazy" src="../../assets/tui/prompt-light.png" alt="App-model driver · prompt · light"></a><figcaption>App-model driver · prompt Light · Regenerated after P2/P5.</figcaption></figure>
</div>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="dark"><h4>Web · Dark</h4><div class="sg-dialog"><strong>New session</strong><label>Title<input value="tidy-tests" readonly></label><label>Prompt<textarea readonly>Cover appending a second item…</textarea></label><div class="sg-bar"><button type="button">Cancel</button><button type="button" class="sg-primary">Create</button></div></div></section>
<figure><a href="../../assets/web/new-session-dark.png"><img loading="lazy" src="../../assets/web/new-session-dark.png" alt="Current web new-session screen in dark theme"></a><figcaption>Rules applied · Real web screen · new-session · dark</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="dark"><h4>TUI · Dark</h4><pre>╭ New session ───────────────╮
│ Title: tidy-tests          │
│ Prompt: Cover appending…   │
│ enter create · esc cancel  │
╰───────────────────────────╯</pre></section>
<figure><a href="../../assets/design/tui-c/prompt-dark.svg"><img loading="lazy" src="../../assets/tui/prompt-dark.png" alt="App-model driver · prompt · dark"></a><figcaption>App-model driver · prompt Dark · Regenerated after P2/P5.</figcaption></figure>
</div>

<h3>Tasks</h3>
<p>Use surface; task names, next run and action labels use body-sized ink. Only raw trigger/time metadata uses caption-sized ink-muted. Selected tasks use surface-raised plus the accent marker. Use space-2 row insets, space-4 between groups. Failures use dead and stay visible. Edit is primary; other actions are disclosed.</p>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="light"><h4>Web · Light</h4><div class="sg-selected"><strong>nightly-tests · Enabled</strong><br><span>Next run · Tomorrow at 12:00 UTC</span><br><small>cron · 0 12 * * *</small><br><button type="button" class="sg-primary">Edit task</button></div></section>
<figure><a href="../../assets/web/tasks.png"><img loading="lazy" src="../../assets/web/tasks.png" alt="Current web tasks screen in light theme"></a><figcaption>Rules applied · Real web screen · tasks · light</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="light"><h4>TUI · Light</h4><pre>Tasks
<span class="sg-selected">› nightly-tests · Enabled</span>
  Next run · Tomorrow at 12:00 UTC
  enter edit · r run now · esc back</pre></section>
<figure><a href="../../assets/design/tui-c/tasks-light.svg"><img loading="lazy" src="../../assets/tui/tasks-light.png" alt="App-model driver · tasks · light"></a><figcaption>App-model driver · tasks Light · Regenerated after P2/P5.</figcaption></figure>
</div>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="dark"><h4>Web · Dark</h4><div class="sg-selected"><strong>nightly-tests · Enabled</strong><br><span>Next run · Tomorrow at 12:00 UTC</span><br><small>cron · 0 12 * * *</small><br><button type="button" class="sg-primary">Edit task</button></div></section>
<figure><a href="../../assets/web/tasks-dark.png"><img loading="lazy" src="../../assets/web/tasks-dark.png" alt="Current web tasks screen in dark theme"></a><figcaption>Rules applied · Real web screen · tasks · dark</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="dark"><h4>TUI · Dark</h4><pre>Tasks
<span class="sg-selected">› nightly-tests · Enabled</span>
  Next run · Tomorrow at 12:00 UTC
  enter edit · r run now · esc back</pre></section>
<figure><a href="../../assets/design/tui-c/tasks-dark.svg"><img loading="lazy" src="../../assets/tui/tasks-dark.png" alt="App-model driver · tasks · dark"></a><figcaption>App-model driver · tasks Dark · Regenerated after P2/P5.</figcaption></figure>
</div>

<h3>Config and accounts</h3>
<p>Use surface with heading-sized ink section titles, body-sized ink keys, labels and values, and caption-sized ink-muted paths. Inputs use surface-raised, border and radius-control. Use space-3 panel insets and space-4 between Config and Accounts. Appearance offers Light / Dark / System only. No palette editor, presets or colour keys.</p>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="light"><h4>Web · Light</h4><strong>Config · Local daemon</strong><br><small>/work/config.toml</small><label>Appearance<select disabled><option>System</option><option>Light</option><option>Dark</option></select></label><label>Editor binary<span>Program used by editor tabs</span><input value="code-server" readonly></label><span>Saved · Applies to new tabs</span><hr><strong>Accounts</strong><p>work · Logged in</p><button type="button">Add account</button></section>
<figure><a href="../../assets/web/config-accounts.png"><img loading="lazy" src="../../assets/web/config-accounts.png" alt="Current web config-accounts screen in light theme"></a><figcaption>Rules applied · Real web screen · config-accounts · light</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="light"><h4>TUI · Light</h4><pre>Config · Local daemon
  Appearance: System · Light · Dark
<span class="sg-selected">› Editor binary: code-server</span>
  Saved · Applies to new tabs
Accounts
  work · Logged in</pre></section>
<figure><a href="../../assets/design/tui-c/appearance-light.svg"><img loading="lazy" src="../../assets/tui/appearance-light.png" alt="App-model driver · appearance · light"></a><figcaption>App-model driver · appearance Light · Regenerated after P2/P5.</figcaption></figure>
</div>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="dark"><h4>Web · Dark</h4><strong>Config · Local daemon</strong><br><small>/work/config.toml</small><label>Appearance<select disabled><option>System</option><option>Light</option><option>Dark</option></select></label><label>Editor binary<span>Program used by editor tabs</span><input value="code-server" readonly></label><span>Saved · Applies to new tabs</span><hr><strong>Accounts</strong><p>work · Logged in</p><button type="button">Add account</button></section>
<figure><a href="../../assets/web/config-accounts-dark.png"><img loading="lazy" src="../../assets/web/config-accounts-dark.png" alt="Current web config-accounts screen in dark theme"></a><figcaption>Rules applied · Real web screen · config-accounts · dark</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="dark"><h4>TUI · Dark</h4><pre>Config · Local daemon
  Appearance: System · Light · Dark
<span class="sg-selected">› Editor binary: code-server</span>
  Saved · Applies to new tabs
Accounts
  work · Logged in</pre></section>
<figure><a href="../../assets/design/tui-c/appearance-dark.svg"><img loading="lazy" src="../../assets/tui/appearance-dark.png" alt="App-model driver · appearance · dark"></a><figcaption>App-model driver · appearance Dark · Regenerated after P2/P5.</figcaption></figure>
</div>

<h3>Buttons, fields and menus</h3>
<p>Primary: accent fill with surface text. Secondary: surface-raised fill with ink text. All controls use border, body type, radius-control and space-2 padding; focus always adds the accent outline. Disabled controls keep ink and a dashed outline. Destructive confirmation uses dead text. Menus use surface-raised and radius-dialog. Never use ink-muted for action or field labels.</p>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="light"><h4>Web · Light</h4><label>Title<input class="sg-focus" value="tidy-tests" readonly></label><div class="sg-bar"><button class="sg-primary" type="button">Create</button><button type="button">Cancel</button><button disabled>Creating…</button><button class="sg-error" type="button">Delete session</button></div><details><summary>Project · todo-cli</summary><p>todo-cli · Selected</p><p>Register project…</p></details></section>
<figure><a href="../../assets/web/new-session.png"><img loading="lazy" src="../../assets/web/new-session.png" alt="Current web new-session screen in light theme"></a><figcaption>Rules applied · Real web screen · new-session · light</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="light"><h4>TUI · Light</h4><pre>Project
<span class="sg-selected">› todo-cli · Selected</span>
  Register project…
enter select · esc cancel
Creating… · Please wait</pre></section>
<figure><a href="../../assets/design/tui-c/project-picker-light.svg"><img loading="lazy" src="../../assets/tui/project-picker-light.png" alt="App-model driver · project picker · light"></a><figcaption>App-model driver · project picker Light · Regenerated after P2/P5.</figcaption></figure>
</div>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="dark"><h4>Web · Dark</h4><label>Title<input class="sg-focus" value="tidy-tests" readonly></label><div class="sg-bar"><button class="sg-primary" type="button">Create</button><button type="button">Cancel</button><button disabled>Creating…</button><button class="sg-error" type="button">Delete session</button></div><details><summary>Project · todo-cli</summary><p>todo-cli · Selected</p><p>Register project…</p></details></section>
<figure><a href="../../assets/web/new-session-dark.png"><img loading="lazy" src="../../assets/web/new-session-dark.png" alt="Current web new-session screen in dark theme"></a><figcaption>Rules applied · Real web screen · new-session · dark</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="dark"><h4>TUI · Dark</h4><pre>Project
<span class="sg-selected">› todo-cli · Selected</span>
  Register project…
enter select · esc cancel
Creating… · Please wait</pre></section>
<figure><a href="../../assets/design/tui-c/project-picker-dark.svg"><img loading="lazy" src="../../assets/tui/project-picker-dark.png" alt="App-model driver · project picker · dark"></a><figcaption>App-model driver · project picker Dark · Regenerated after P2/P5.</figcaption></figure>
</div>

<h3>Empty states</h3>
<p>Use surface, display-sized ink for the condition and body-sized ink for the next step. Use space-4 between explanation and the single primary action. No illustration, card or extra colour. Distinguish zero sessions from no project; empty TUI sections do not reserve permanent rows.</p>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="light"><h4>Web · Light</h4><strong class="sg-empty-title">No sessions yet</strong><p>Create a session in todo-cli.</p><button class="sg-primary" type="button">New session</button><hr><strong class="sg-empty-title">No project selected</strong><p>Choose a project to see its sessions.</p><button type="button">Choose project</button></section>
<figure><a href="../../assets/web/dashboard.png"><img loading="lazy" src="../../assets/web/dashboard.png" alt="Current web dashboard screen in light theme"></a><figcaption>Rules applied · Real web screen · dashboard · light · Context only; see the P4 recovery matrix</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="light"><h4>TUI · Light</h4><pre>No sessions yet
Press n to create a session.

No project selected
Choose a project to continue.</pre></section>
<figure><a href="../../assets/design/tui-c/zero-sessions-light.svg"><img loading="lazy" src="../../assets/tui/zero-sessions-light.png" alt="App-model driver · zero sessions · light"></a><figcaption>App-model driver · zero sessions Light · Regenerated after P2/P5.</figcaption></figure>
</div>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="dark"><h4>Web · Dark</h4><strong class="sg-empty-title">No sessions yet</strong><p>Create a session in todo-cli.</p><button class="sg-primary" type="button">New session</button><hr><strong class="sg-empty-title">No project selected</strong><p>Choose a project to see its sessions.</p><button type="button">Choose project</button></section>
<figure><a href="../../assets/web/dashboard-dark.png"><img loading="lazy" src="../../assets/web/dashboard-dark.png" alt="Current web dashboard screen in dark theme"></a><figcaption>Rules applied · Real web screen · dashboard · dark · Context only; see the P4 recovery matrix</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="dark"><h4>TUI · Dark</h4><pre>No sessions yet
Press n to create a session.

No project selected
Choose a project to continue.</pre></section>
<figure><a href="../../assets/design/tui-c/zero-sessions-dark.svg"><img loading="lazy" src="../../assets/tui/zero-sessions-dark.png" alt="App-model driver · zero sessions · dark"></a><figcaption>App-model driver · zero sessions Dark · Regenerated after P2/P5.</figcaption></figure>
</div>

<h3>Errors and notices</h3>
<p>Use surface and body-sized ink for consequence and recovery instructions; only the failure heading uses dead. A full unavailable screen may use display type. Use space-2 inside the message and space-4 before the recovery action. Save/restart notices use ink, never ready green. Wrap actionable text and preserve entered input.</p>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="light"><h4>Web · Light</h4><strong class="sg-error sg-empty-title">Cannot reach the daemon</strong><p>Sessions could not be loaded. Check the daemon, then retry.</p><button type="button">Retry</button><hr><strong>Login expired</strong><p>Sign in again to reconnect.</p><button type="button">Sign in</button></section>
<figure><a href="../../assets/web/config-accounts.png"><img loading="lazy" src="../../assets/web/config-accounts.png" alt="Current web config-accounts screen in light theme"></a><figcaption>Rules applied · Real web screen · config-accounts · light · Context only; see the P4 recovery matrix</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="light"><h4>TUI · Light</h4><pre>Cannot reach the daemon
Sessions could not be loaded.
Check the daemon, then retry.

Login expired · Sign in again</pre></section>
<figure><a href="../../assets/design/tui-c/no-daemon-light.svg"><img loading="lazy" src="../../assets/tui/no-daemon-light.png" alt="App-model driver · no daemon · light"></a><figcaption>App-model driver · no daemon Light · Regenerated after P2/P5.</figcaption></figure>
</div>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="dark"><h4>Web · Dark</h4><strong class="sg-error sg-empty-title">Cannot reach the daemon</strong><p>Sessions could not be loaded. Check the daemon, then retry.</p><button type="button">Retry</button><hr><strong>Login expired</strong><p>Sign in again to reconnect.</p><button type="button">Sign in</button></section>
<figure><a href="../../assets/web/config-accounts-dark.png"><img loading="lazy" src="../../assets/web/config-accounts-dark.png" alt="Current web config-accounts screen in dark theme"></a><figcaption>Rules applied · Real web screen · config-accounts · dark · Context only; see the P4 recovery matrix</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="dark"><h4>TUI · Dark</h4><pre>Cannot reach the daemon
Sessions could not be loaded.
Check the daemon, then retry.

Login expired · Sign in again</pre></section>
<figure><a href="../../assets/design/tui-c/no-daemon-dark.svg"><img loading="lazy" src="../../assets/tui/no-daemon-dark.png" alt="App-model driver · no daemon · dark"></a><figcaption>App-model driver · no daemon Dark · Regenerated after P2/P5.</figcaption></figure>
</div>

<h3>Help and status bar</h3>
<p>Use surface and body-sized ink for shortcuts and exit instructions; only supplemental annotations use caption-sized ink-muted. Use space-2 between fragments, no boxes or state colours. Show valid shortcuts for the current keyboard owner; the exit route survives narrow widths. Connection labels are static.</p>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="light"><h4>Web · Light</h4><div class="sg-bar"><span>ctrl+] · Return to sessions</span><span>? · Help</span></div><p>Connecting…</p></section>
<figure><a href="../../assets/web/agent-tab.png"><img loading="lazy" src="../../assets/web/agent-tab.png" alt="Current web agent-tab screen in light theme"></a><figcaption>Rules applied · Real web screen · agent-tab · light</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="light"><h4>TUI · Light</h4><pre>ctrl+] return · ? help
Connecting…</pre></section>
<figure><a href="../../assets/design/tui-c/help-light.svg"><img loading="lazy" src="../../assets/tui/help-light.png" alt="App-model driver · help · light"></a><figcaption>App-model driver · help Light · Regenerated after P2/P5.</figcaption></figure>
</div>

<div class="sg-pair">
<section class="sg-theme" data-af-theme="dark"><h4>Web · Dark</h4><div class="sg-bar"><span>ctrl+] · Return to sessions</span><span>? · Help</span></div><p>Connecting…</p></section>
<figure><a href="../../assets/web/agent-tab-dark.png"><img loading="lazy" src="../../assets/web/agent-tab-dark.png" alt="Current web agent-tab screen in dark theme"></a><figcaption>Rules applied · Real web screen · agent-tab · dark</figcaption></figure>
</div>
<div class="sg-pair">
<section class="sg-theme sg-tui" data-af-theme="dark"><h4>TUI · Dark</h4><pre>ctrl+] return · ? help
Connecting…</pre></section>
<figure><a href="../../assets/design/tui-c/help-dark.svg"><img loading="lazy" src="../../assets/tui/help-dark.png" alt="App-model driver · help · dark"></a><figcaption>App-model driver · help Dark · Regenerated after P2/P5.</figcaption></figure>
</div>


## Phone layout

At widths up to 768px, a selected terminal is the screen. Keep this composition
active while the drawer overlays it, without moving or resizing the terminal.
Use one 48px top row: drawer toggle, truncating session title, static Keyboard
ownership label and one … disclosure. Preserve the full title in title and aria
text. The disclosure holds project choices, Sessions · Tasks · Config, install,
Light · Dark · System, Disconnect, pane actions and the session tab switcher.
Project choices and new-tab types are inline inside it, without nested disclosures.
For a split session, show the focused pane and keep Hide pane in the menu; restore
the split when returning to desktop.
Escape closes the disclosure and returns focus to its trigger. The drawer, Tasks,
Config and nonterminal content retain their existing phone layouts.

While a terminal owns the keyboard, show one 44px keybar above the soft keyboard,
following the visual viewport and safe-area insets. Ctrl, Alt, Esc, Tab, ^C and
Arrows share the row. Arrows replaces it with More keys and ← ↑ ↓ →; More keys restores the
primary row. At 360px, six targets fit with 4px gaps and 8px horizontal insets.
Hide the bar when ownership leaves the terminal or the viewport exceeds 768px.
Refit the terminal so its last row remains above the bar.

With the soft keyboard closed, the terminal element must occupy at least 85% of
the visual viewport at 360, 390 and 430px. Browser assertions enforce this in both
themes. Preserve the pane padding so the inset focus border cannot paint over
column one; the screen must stay inside the host with no horizontal scroll.
Keep all targets at least 44px, truncate instead of wrapping, use existing
tokens and static glyphs, and do not animate.

Ctrl and Alt apply once to the next typed character, including composed text.
Double tap within 350ms to lock; tap again to release. An armed modifier has the
selected-row surface, bold ink and accent edge. A locked modifier also has an
accent outline and static ▸ marker. Buttons retain terminal focus on pointerdown;
there is no animation. ^C always interrupts, even with a modifier armed.

![Session-first phone terminal](../assets/design/3981/after-phone-session-390.png)

## Implementation reference

The examples above are the specification; this compact reference is for implementers,
not a palette picker. Every value below comes from `design/tokens.json`. Adding a role
or step fails validation: revise a component rule before enlarging this contract.

<details><summary>Exact internal values · 23 tokens</summary>
<table><thead><tr><th>Colour role</th><th>Light</th><th>Dark</th><th>Required use</th></tr></thead><tbody>
<tr><td>accent</td><td>#2d6271</td><td>#2296f3</td><td>Primary button fill, selected marker, active-tab underline and keyboard-focus outline; never liveness</td></tr>
<tr><td>archived</td><td>#4c566a</td><td>#d8dee9</td><td>Archived glyph and label only; retained history</td></tr>
<tr><td>border</td><td>#657084</td><td>#3d3d3d</td><td>Plane boundaries and disabled controls; resting session controls use transparent borders, keyboard focus uses accent</td></tr>
<tr><td>dead</td><td>#883b43</td><td>#e4c8cd</td><td>Dead glyph/label and failed-operation or destructive-confirmation text; never ordinary selection</td></tr>
<tr><td>ink</td><td>#2e3440</td><td>#cccccc</td><td>All body text, names, headings, field labels and action labels</td></tr>
<tr><td>ink-muted</td><td>#4c566a</td><td>#9d9d9d</td><td>Secondary metadata only: path, timestamp and shortcut annotation; never body text or field labels</td></tr>
<tr><td>limit-reached</td><td>#73436b</td><td>#dbb9d5</td><td>Limit reached glyph and label only; no decorative purple</td></tr>
<tr><td>lost</td><td>#705014</td><td>#ebcb8b</td><td>Lost glyph and label only; do not infer an error from a slow connection</td></tr>
<tr><td>ready</td><td>#405430</td><td>#d5e2cc</td><td>Ready glyph and label only; green never means keyboard focus or generic success</td></tr>
<tr><td>running</td><td>#4c566a</td><td>#d8dee9</td><td>Running state text only; no indicator, including in-flight operations</td></tr>
<tr><td>surface</td><td>#f8f9fc</td><td>#1f1f1f</td><td>Working surface and terminal canvas; text on the primary accent button. Web Sessions chrome uses surface-raised; TUI paint is unchanged.</td></tr>
<tr><td>surface-raised</td><td>#eceff4</td><td>#2b2b2b</td><td>Web navigation and focused-session identity plane, dialogs, menus and controls. Depth comes from the two existing surfaces, never a new hue.</td></tr>
</tbody></table>
<table><thead><tr><th>Metric</th><th>Web</th><th>TUI</th><th>Required use</th></tr></thead><tbody>
<tr><td>radius-control</td><td>4px</td><td>0</td><td>Buttons and inputs only; square terminal controls</td></tr>
<tr><td>radius-dialog</td><td>8px</td><td>1</td><td>Dialogs and menus only; rounded terminal overlay</td></tr>
<tr><td>space-1</td><td>4px</td><td>1</td><td>Glyph-to-label gap; one horizontal terminal cell</td></tr>
<tr><td>space-2</td><td>8px</td><td>1</td><td>Related controls and row inset; one horizontal terminal cell</td></tr>
<tr><td>space-3</td><td>16px</td><td>2</td><td>Panel/dialog inset; two horizontal terminal cells</td></tr>
<tr><td>space-4</td><td>24px</td><td>1</td><td>Section separation; one vertical blank terminal row</td></tr>
<tr><td>type-body</td><td>0.875rem</td><td>1</td><td>14px: body, fields, buttons and session/task names; one terminal row</td></tr>
<tr><td>type-caption</td><td>0.8125rem</td><td>1</td><td>13px: secondary metadata only; one terminal row</td></tr>
<tr><td>type-display</td><td>1.75rem</td><td>1</td><td>28px: focused web session identity and empty-screen heading; terminal row unchanged</td></tr>
<tr><td>type-heading</td><td>1rem</td><td>1</td><td>16px: section and pane headings; one bold terminal row</td></tr>
<tr><td>type-title</td><td>1.25rem</td><td>1</td><td>20px: web fleet heading and dialog title; one bold terminal row</td></tr>
</tbody></table>
</details>

Body and state text meet 4.5:1 on both surfaces; body ink meets 7:1 on surface.
Light control outlines meet 3:1 on both surfaces; dark outlines meet 1.5:1 on surface. Primary
buttons use accent with surface text at 4.5:1. No extra selection, on-accent, danger,
hover, shadow or preview colour exists. Selected rows use surface-raised and an accent
marker; failed-operation text reuses dead. Ink-muted is secondary metadata only.
Ink-muted must differ from ink by at least 1.5:1 luminance contrast in both themes
and remain lower-contrast than ink on both planes. A matching liveness colour needs
a per-theme `sharesMuted` rationale in the source; running and archived share it
intentionally in light only.
Fonts, 400/600 weight, 1.5 web line height, 44px touch height and a 2px focus ring are
fixed component rules, not more tokens. Indicators never animate. All disclosures and TUI changes are immediate; no animation is introduced.

## Capture coverage

The [Sessions still](../assets/tui/tui-sessions.svg) and
[Tasks still](../assets/tui/tui-tasks.svg) use the same refreshed app-model driver
as the light/dark matrix. The [TUI video](../assets/tui/demo.mp4) uses the separate
real-agent recorder. Stand-in and model fixtures are identified at their source.

## Regeneration

Run `make docs`, then `go run ./scripts/gen-design --check` and `mkdocs build --strict`.
The docs drift gate checks the web CSS, Go theme, docs CSS copy and this page together.
Edit the token source and specimen sources, never these generated outputs.
Every web UI PR must verify a focused session at phone widths (360, 390 and 430px)
in both themes, including title truncation, overflow keyboard access and 44px
controls. Regenerate affected container goldens and keep pixel-exact checks and
performance budgets passing.

## Recovery screens · P4

The [recovery capture matrix](recovery-stills.md) shows the application’s empty,
unavailable and failed-operation states in both themes. These captures come from
asserted container selftest scenes; the surrounding chrome remains owned by P2.

The [TUI recovery matrix](tui-recovery-stills.md) contains app-model driver
captures in both themes, supplemented by an isolated real tmux onboarding run.

## TUI roles · P5 slice A

The [TUI role gallery](tui-design-a-stills.md) applies the generated roles to
96 app-model driver stills. Slice A replaces the configurable TUI palette and
private ANSI colours; the common overlay recipe and density cuts follow in B,
and the Light/Dark/System selector and migration in C.

The [TUI overlay and density gallery](tui-design-b-stills.md) applies the shared dialog frame and TUI cut list.
The [TUI appearance gallery](tui-design-c-stills.md) shows Light / Dark / System and the retired-key migration.
