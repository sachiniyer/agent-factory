# Manual TUI testing — the driver (#1161)

Driving the real multi-pane TUI by hand for a play-test gate used to be
error-prone: keys landed in a live pane as literal text (#1156), hand-rolled
tmux harnesses died on `$TMUX`/`TMUX_TMPDIR` collisions (#1155), and every run
re-derived `send-keys`/`capture-pane`/`sleep N` plumbing from scratch — where
the blind `sleep`s were the main flake source.

`scripts/tui-driver.sh` is the fix: a **sourceable driver library** of
self-synchronizing functions that drive the live TUI and assert on it. It
builds ON the #1130 container sandbox (which already solves isolation —
throwaway home, mock repo, private tmux, pids/memory caps; see
[container-testing.md](container-testing.md)). The container is the *where*;
this driver is the *how*.

- **Library**: [`scripts/tui-driver.sh`](https://github.com/sachiniyer/agent-factory/blob/master/scripts/tui-driver.sh)
- **Self-test / acceptance proof**:
  [`scripts/tui-driver-selftest.sh`](https://github.com/sachiniyer/agent-factory/blob/master/scripts/tui-driver-selftest.sh)
- **Run it**: `make tui-driver-selftest` (gate) · `make tui-driver` (drive by
  hand)

---

## 1. The interaction model — read this first

The #1156 mis-drive happened because the driver didn't know *which mode the
TUI was in*. Get this right and the rest follows.

### Nav mode vs interactive mode

| Mode | What the keyboard does | Menu bar shows | How you got here |
|------|------------------------|----------------|------------------|
| **Nav** | Keys are TUI commands (`n` new, `s` open pane, `t` new tab, `j`/`k` move the tree cursor, …) | context hints (`n new • …`, `↵ interact • …`) | default; `Ctrl-]` from interactive |
| **Interactive** | *Every* key — including Tab — forwards to the focused pane's shell/agent | only `ctrl+] nav mode` | `Enter` on a focused pane |
| **Attached (full-screen)** | tmux owns the terminal; the TUI chrome is gone | the tmux status line (`[af_…`) | `o` on a selected instance |

The single most important primitive is **`af_ensure_nav`** — it sends `Ctrl-]`
(a no-op in nav, the escape hatch from interactive) so a scenario can never
mistake a live pane for the host. Call it before any nav action. This is the
one-line fix for the entire #1156 class.

### The focus ring

In nav mode, `Tab`/`Shift-Tab` cycle **ring focus** across regions: the
**instances tree** → each open **pane** → the **automations** strip → back.
The menu bar is context-sensitive and follows focus:

- **tree** focused → `n new • …` (plus instance verbs when a row is selected)
- **pane** focused → `↵ interact • o attach • … • s open pane • x hide pane`
- **automations** focused → `enter manage • …`

`af_focus_tree` walks the ring (checking *before* it presses, so it never
Tabs off the tree) until the `n new` hint proves the tree has focus.

### The instances tree

The left rail is a tree: each instance row carries its tabs as children. The
**display-selected instance auto-expands** (arrow `▾`, its tab children shown)
while every other instance collapses (arrow `▸`). That arrow is the reliable,
text-greppable selection signal the driver asserts on:

```
  ▾ 1.  alpha         ●      ← selected (expanded); ● = ready
       └ 1 Preview *
  ▸ 2.  beta          ●      ← not selected (collapsed)
```

On a cold boot the cursor sits on the **section header** (no instance selected
— menu shows the plain `n new • N new remote` set); the first `j` moves the
cursor down onto the first instance. `af_select` is robust to any starting
position: it anchors at the top (`k` is idempotent there), then steps down
with `j` until the target row shows `▾` **and** the tree cursor is actually on
it.

> **Display-selected ≠ cursor-on-row (#1174 / #1199).** The sticky `▾` is only
> a *display* signal. A **single** auto-selected instance renders `▾` while the
> cursor still sits on the section header — there `GetSelectedInstance()` is
> `nil`, so `o`/`D`/attach silently no-op even though the row *looks* selected
> (a false pass waiting to happen). `af_select` therefore also requires the
> menu to advertise a **row-scoped verb (`D kill`)** — present only when a real
> instance is under the cursor — and keeps pressing `j` past the header until
> it appears. `af_attach`/`af_open_pane` inherit the fix because scenarios call
> `af_select` first.

### Which key goes where (defaults, from `keys/keys.go`)

| Key | Nav action | | Key | Nav action |
|-----|-----------|-|-----|-----------|
| `n` | new instance | | `s` | open selected tab as pane |
| `Enter` | interact (enter pane) | | `x` | hide focused pane |
| `o` | attach full-screen | | `t` | new tab |
| `Ctrl-]` | exit interactive → nav | | `w` | close tab |
| `Tab` | cycle focus ring | | `1`–`9` | jump to tab |
| `j`/`k`,`↓`/`↑` | move tree cursor | | `S` | tasks overlay |
| `D` | kill instance | | `/` | search |
| `Ctrl-W` | detach (full-screen) | | `q` | quit |

`Enter`, `Tab`, `Shift-Tab`, `Esc`, `Ctrl-]`, and `1`–`9` are **reserved** and
cannot be rebound (`[keys]` config). `Ctrl-W` is the configurable detach key
(`detach_keys`); the driver reads it from `AF_DRIVER_DETACH_KEY`.

---

## 2. The wait-not-sleep principle

**Never synchronize on a fixed `sleep`.** A `sleep 2` is a bet that the TUI
finished repainting in under two seconds — it flakes when it's slow and wastes
time when it's fast. Instead, every action helper returns *only once the screen
shows its completion marker*:

```bash
af_send n                      # request a new instance
af_wait_for 'submit name'      # ← block until the name prompt actually appears
```

`af_wait_for <regex> [timeout] [label]` polls `capture-pane` (a short poll
interval, **not** a synchronization sleep) until the screen matches, and dumps
the last screen on timeout so a failure is debuggable. `af_wait_gone` is the
inverse. Every scenario helper is built from these, so a scenario is a
straight-line list of intent with no timing knobs.

---

## 3. Driver command reference

Source the library inside the container, then call the functions. State lives
in the running TUI (a private tmux session, default name `drive`), so calls
compose across `docker exec` invocations.

### Anti-flake core

| Function | What it does |
|----------|--------------|
| `af_wait_for <re> [t] [label]` | poll until the screen matches `<re>` (no blind sleeps) |
| `af_wait_gone <re> [t] [label]` | poll until `<re>` is absent |
| `af_ensure_nav` | `Ctrl-]` → force nav mode (fixes the #1156 class) |
| `af_focus_tree` | Tab the ring until the instances tree has focus |

### Scenario helpers (each self-synchronizes on its marker)

| Function | Keys | Completion marker |
|----------|------|-------------------|
| `af_reset_sandbox` | — | wipes instances/branches for a deterministic rerun (sandbox-scoped, fails closed) |
| `af_boot` | launch `af` | `Agent Factory` + `Instances (` frame |
| `af_new_instance <name>` | `n`,text,`Enter` | the row shows `<name> … ●` (ready) |
| `af_select <name>` | `k`×,`j`× | `<name>`'s row shows `▾` |
| `af_open_pane` | `s` | pane-focus menu (`x hide pane`) |
| `af_hide_pane` | `x` | focus back on tree (`n new`) |
| `af_enter_interactive` | `Enter` | interactive menu (`ctrl+] nav mode`) |
| `af_exit_interactive` | `Ctrl-]` | interactive menu gone |
| `af_send_to_pane <text>` | marker+text,`Enter` | short delivery marker echoes in the pane (then wait for output yourself) |
| `af_attach` | `o` | TUI chrome gone (full-screen) |
| `af_detach` | `Ctrl-W` | TUI chrome back **and** the attach client is reaped (guards #1157) |
| `af_new_tab` | `t` | tab-child count rises |
| `af_close_tab` | `w` | tab-child count falls |
| `af_open_tasks` / `af_close_tasks` | `S` / `Esc` | tasks overlay (`r run now`) appears / gone |
| `af_click <x> <y>` / `af_click_instance <name>` | SGR mouse | injects a left click at a cell / on an instance row |
| `af_scroll <up\|down> [x] [y]` | SGR wheel | injects a wheel event |
| `af_set_config <toml>` + `af_relaunch` | — | rewrites `config.toml` (canonical since #1030) and reboots the TUI |
| `af_resize <cols> <rows>` | — | pins the session to an exact geometry that STICKS (`window-size manual` + `resize-window`), for tiny-size gates |
| `af_quit` | `q` | back to a shell prompt |

### Assertions & introspection

| Function | Pass condition |
|----------|----------------|
| `af_assert_screen <re>` / `af_refute_screen <re>` | screen matches / does not match |
| `af_expect_selected <name>` | `<name>`'s row carries `▾` |
| `af_tmux_ls` | prints the tmux sessions (introspection) |
| `af_ps` | prints the daemon + tmux attach/new-session process tree |
| `af_assert_no_orphan_clients` | no `tmux attach-session` reparented to init (the #1155/#1157 leak signature); the daemon's own monitor clients are parented to the daemon and excluded |

### Configuration (env vars)

`AF_DRIVER_SESSION` (`drive`), `AF_DRIVER_COLS`/`ROWS` (`100`/`30`),
`AF_DRIVER_REPO` (`$HOME/sandbox/mock-repo`), `AGENT_FACTORY_HOME`,
`AF_DRIVER_TIMEOUT` (`25`s), `AF_DRIVER_POLL` (`0.25`s),
`AF_DRIVER_DETACH_KEY` (`C-w`), `AF_DRIVER_BIN` (auto-resolved).

Set `AF_DRIVER_COLS`/`ROWS` **before** `af_boot` to launch at a non-default
size — `af_boot` pins it so it sticks (see the tiny-size gate below). Change
the size mid-run with `af_resize <cols> <rows>`.

---

## 4. Running it

### The self-test (acceptance proof + bitrot guard)

```bash
make tui-driver-selftest
```

Boots a **dedicated** container sandbox (`af-driver-selftest`, so it never
disturbs a `drive`/`playtest` container you have open), then runs the exact
scenario that failed in #1156, now deterministic:

> reset → boot → create **two** instances → select each (assert selection) →
> open a pane → enter interactive → type into the pane → exit → attach
> full-screen → detach → assert selection preserved → assert no orphan clients.

Green means the driver drives the TUI reliably. Any failure prints the step
and the offending screen.

### Driving by hand

```bash
make tui-driver          # boots af via the driver, then attaches you to the
                         # live session (detach with your tmux prefix + d)
```

Or drive over `docker exec` against a detached sandbox. The container name is
unique per run (#1171); pin it with `AF_PLAYTEST_NAME` so your `docker exec`
targets it:

```bash
export AF_PLAYTEST_NAME="af-playtest-$$"
make playtest-container-detached
docker exec "$AF_PLAYTEST_NAME" bash -lc '
  source /src/scripts/tui-driver.sh
  af_boot
  af_new_instance alpha
  af_new_instance beta
  af_select beta && af_expect_selected beta
  af_open_pane && af_enter_interactive
  af_send_to_pane "echo hi"
  af_exit_interactive
  af_assert_no_orphan_clients
'
docker rm -f "$AF_PLAYTEST_NAME"   # teardown
```

Everything runs inside the container — the host tmux server, the real
`~/.agent-factory`, and this repo are all untouched.

---

## 5. Gate-recipe library

To gate a visible-TUI PR, run the scenario for its class and assert the
markers. All of these are driver calls; each already self-synchronizes.

### Any TUI-visible change → the smoke gate

```bash
make tui-driver-selftest
```

The self-test is the baseline gate for *any* PR that touches startup, the
sidebar/tree, panes, interactive mode, or attach/detach. If it isn't green,
stop.

### Tree / selection / focus changes (the #1156, #1084 class)

```bash
af_boot
af_new_instance a; af_new_instance b; af_new_instance c   # (cap: 3)
af_select a; af_expect_selected a
af_select c; af_expect_selected c
af_select b; af_expect_selected b
# after closing/killing, selection must not silently drift:
af_open_pane; af_close_tab; af_expect_selected b
```

### Pane / interactive changes (the #1088, #1089 class)

```bash
af_select a; af_open_pane
af_enter_interactive
af_send_to_pane 'echo PANE_OK'; af_wait_for 'PANE_OK'
af_exit_interactive
af_refute_screen 'nav mode'          # cleanly back in nav
af_hide_pane                          # pane hides, nothing killed
```

### Attach / detach changes (the #1155, #1157, #1159 class)

```bash
af_select a
af_attach                             # full-screen
af_detach                             # syncs on the attach client being reaped
af_assert_no_orphan_clients           # the hard leak check
af_expect_selected a                  # selection survives the round trip
```

### Tabs (the #930 class)

```bash
af_select a
af_new_tab; af_new_tab                # add two shell tabs
af_close_tab
```

### Config / keymap changes (the #1030 class)

```bash
af_set_config "$(cat <<'TOML'
default_program = 'claude'
[program_overrides]
claude = 'bash'
[keys]
new = ['c']
TOML
)"
af_relaunch
af_ensure_nav; af_focus_tree
af_send c; af_wait_for 'submit name'  # the rebound 'new' key works
```

### Mouse (the #1143 class)

```bash
af_new_instance a; af_new_instance b
af_click_instance a; af_expect_selected a
af_click_instance b; af_expect_selected b
af_scroll down; af_scroll up
```

### Tiny geometry / responsive-layout changes (the #1174-item-2 class)

A **detached** tmux session defaults to `window-size latest`, which snaps the
window back to the last-attached client (80x23) and *ignores* `new-session
-x/-y`. So a naive small-size boot silently ran at 80x23 and never exercised
the tiny layout. Boot with the size preset (`af_boot` pins it), or resize
mid-run with `af_resize`:

```bash
AF_DRIVER_COLS=60 AF_DRIVER_ROWS=15 af_boot   # boots pinned at 60x15
af_new_instance a
af_select a; af_expect_selected a             # selection still works when narrow

af_resize 40 10                               # squeeze to 40x10 mid-run
af_assert_screen 'Instances'                  # header must survive the squeeze
```

---

## 6. Gating a branch cut BEFORE #1166 (the driver isn't in the tree yet)

`scripts/tui-driver.sh` landed in **#1166**. A branch cut before that commit has
no driver to source, so `source /src/scripts/tui-driver.sh` fails with *No such
file*. Two ways to gate such a branch — pick one **before** you boot:

- **Rebase the branch onto `master`** (preferred when the branch is yours and
  rebasing is clean) — this pulls the driver + self-test into the tree
  naturally, and you gate exactly what will merge.
- **Copy the driver in from `master`** when a rebase is noisy or the branch is
  an external PR you don't want to rewrite. Inside the running sandbox
  container:

  ```bash
  # From the host, into the sandbox container (name is unique per run, #1171):
  docker cp scripts/tui-driver.sh          "$AF_PLAYTEST_NAME":/src/scripts/
  docker cp scripts/tui-driver-selftest.sh "$AF_PLAYTEST_NAME":/src/scripts/
  # then drive as usual — the driver is pure harness, so master's copy gates
  # any older product tree without changing what you're testing.
  ```

Because the driver only sends keys and reads the screen — it carries no product
code — master's copy is safe to run against an older `af` build; it asserts on
the same on-screen markers regardless of the branch under test.

---

## 7. Isolation & box safety (inherited from the container)

Every rule from the [tui-playtest skill](https://github.com/sachiniyer/agent-factory/blob/master/.claude/skills/tui-playtest.md)
is satisfied *structurally* by running inside the container: private tmux
server, throwaway `AGENT_FACTORY_HOME`, pre-built mock repo, pids/memory caps,
teardown is `docker rm -f`. The driver reinforces this:

- It only ever kills its **own** named session and (in `af_reset_sandbox`) the
  sandbox's `af_*` sessions — **never `kill-server`**.
- `af_reset_sandbox` **fails closed**: it refuses to wipe anything unless
  `AGENT_FACTORY_HOME` and the mock repo are sandbox paths, so it can never
  touch a real `~/.agent-factory`.
- Instances run the cheap `bash` program (the sandbox's `config.toml`
  override), never a real agent or an unbounded generator.
