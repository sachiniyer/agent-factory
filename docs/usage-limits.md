# Accounts and usage limits

For anyone running agents on a subscription plan. After this page you will know
how to give a session its own credential identity, what happens when an agent
hits its plan's ceiling, and the three ways out of that wall — retry, hand off,
or resume automatically.

## Accounts

An **account** is one of an agent's logged-in identities, held as a directory
the agent CLI treats as its home. `af` never reads, stores, or forwards the
credential itself: it decides which directory a session sees, and the agent's
own login flow puts the material there.

### Credential directories

Accounts are registered separately for Claude, Codex, and Gemini. The directory
is under `accounts/<agent>/<name>` in the AF home; the same name for two agents
does not share a login. `af accounts add <agent> <name>` prints the directory
path, which has these per-agent meanings:

| Agent | Environment variable | Directory shape |
|---|---|---|
| Claude | `CLAUDE_CONFIG_DIR` | The config directory itself |
| Codex | `CODEX_HOME` | The config directory itself |
| Gemini | `GEMINI_CLI_HOME` | A home-like root; Gemini appends `.gemini/` |

Point Gemini at the printed root, never its nested `.gemini` directory. af
recognizes either `<root>/.gemini/oauth_creds.json` or
`<root>/.gemini/gemini-credentials.json` as a credential artifact.

### Log in

Install each agent you want to log into on the machine running the daemon
first. For example, Codex login needs `codex` on the **daemon's `PATH`**, even if
your sessions normally use another agent. Finding it in your current shell is
not enough if that shell's `PATH` changed after the daemon started. From a shell with the corrected `PATH`, run
`af daemon restart`. If you use an installed autostart service, first rerun
`af daemon install` from that shell to replace the service's captured `PATH`;
restarting alone reuses that snapshot. Run the login command in an interactive
terminal.

```bash
af accounts login claude work
af accounts login codex work
af accounts login gemini work
af accounts list
```

Each login registers the account if needed and opens the agent's own login in
a tmux pane on the daemon host. Complete sign-in in a browser on the device you
are using. The flows differ:

| Agent | What af launches | What you do |
|---|---|---|
| Claude | `claude auth login` with `BROWSER=true`, a no-op browser opener | Open the printed URL and paste the authorization code into the pane if prompted |
| Codex | `codex login --device-auth` | Open the verification URL and enter the device code there while the CLI polls |
| Gemini | `gemini` with `NO_BROWSER=true` | Open the URL, sign in, then paste the authorization code at the terminal prompt |

Afterwards, af reports whether a credential file exists. It checks presence,
never reads the secret; presence is not proof that a token is still valid.
`af accounts login codex work --no-attach` starts the flow and prints how to
attach instead of taking your terminal.

To stop before signing in, press `ctrl+c` in the login pane: the account stays
registered but `af accounts list` reports `not logged in`, and the login command
reports that no credential was created. Re-run the login command when ready to
finish. A named Codex account has separate credentials, history, and skills.

Codex registration seeds missing `approval_policy` · `sandbox_mode` · `model`
from the operator's `~/.codex/config.toml`, with the selected profile overriding
root values; existing account settings, including selected-profile values, stand.
For effective `workspace-write` mode it also seeds missing workspace options
(`network_access` · `writable_roots` · `exclude_tmpdir_env_var` · `exclude_slash_tmp`).
It skips `model` if either config selects `model_provider` at root or in its
selected profile, and seeds nothing if the ambient config has neither approval
policy nor sandbox mode. Credentials, provider configuration, profiles, and
project trust are never copied. An unresolved ambient profile or an ambient
approval, sandbox, or active workspace option that fails schema validation prevents all
seeding; unreadable or unparseable ambient files also leave settings alone.
Both `af accounts add` and `af accounts login` print a notice explaining the
seeding policy and any refusal.

Gemini's registration pre-answers folder trust for the account directory and
selects Google OAuth in that account's non-secret settings. Existing choices
are preserved, so an account with a different authentication method can still
show the picker. This touches only the account's own `.gemini` settings; it
neither authenticates you nor writes a credential. Claude and Codex need no
such settings to reach sign-in.

The equivalent manual setup is:

```bash
CODEX_HOME=$(af accounts add codex work) codex login --device-auth
CLAUDE_CONFIG_DIR=$(af accounts add claude work) BROWSER=true claude auth login
GEMINI_CLI_HOME=$(af accounts add gemini work) NO_BROWSER=true gemini
```

Use the managed login to get af's identity environment isolation too: it
removes competing identity variables while preserving proxy and certificate
settings. Explicit `BROWSER` or `NO_BROWSER` entries in
`session_env_passthrough` override the browser-free defaults.

### Pick an account

After completing sign-in, pin a session to that account and its agent (a Codex
account cannot be used by the default Claude agent):

```bash
af sessions create --name spike --program codex --account work
```

In the TUI, press `n` to open the naming form, then `ctrl+o` to pick an
account for the selected agent. In the web client, use **New session** ·
**Program** · **Account**. The picker lists that agent's accounts and shows the
project default; changing the program changes the account list. Register and
log in from the web config view's **Accounts** section using **Log in**.
See the [TUI guide](tui.md#accounts) and [web guide](web.md#config-view).

`af` refuses an unproved fallback — a session either
runs as the account you named or does not start — and by default never rotates
accounts on its own. Rotating after a usage limit is opt-in and needs an
explicit candidate list — see [Opt-in auto-resume](#opt-in-auto-resume). A
session created with `--account` is pinned and automatic switching never rotates it. The full
command surface is in [`af accounts`](reference/cli.md#af-accounts).

### Scoping an account to a project

Typing `--account` on every create is the wrong unit of work when the answer is
always the same for a whole repository. `default_accounts` says it once — per
agent, and most usefully per project:

```bash
af projects add ~/work/monorepo                                 # once
af config set default_accounts.codex work --project ~/work/monorepo
af config set default_accounts.codex personal --project ~/side/hobby
```

Now a session created in that project — from the CLI, the TUI, the web client or
a scheduled task — runs as `work` with nothing typed. The resolution order is
**explicit `--account` (or a picker choice) → the project's default → the global
default → the agent's ambient login**, and it is applied by the background
service on the create, so every surface gets the same answer.

The default has these properties:

- **It is keyed by agent, because an account belongs to one.** claude's `work`
  and codex's `work` are different identities in different registries, so
  `default_accounts.codex` never applies to a claude session — that session keeps
  the ambient identity until you scope it too.
- **The pickers show it.** The TUI's `ctrl+o` field and the web's Account select
  preselect the project default and mark its row `project default`, so a scoped
  create is visible before you press Enter rather than discovered afterwards.
- **A default naming an account that is not registered refuses the create**,
  naming the key, the file it is set in, and how to clear it. It never silently
  falls back to the ambient identity: a session quietly running as someone
  else is the failure the whole feature exists to prevent. The one carve-out
  is a target that cannot carry an account at all — a sandbox, hook or
  unlisted SSH create drops a *default* account with a logged warning and runs
  on the target's own credentials rather than refusing a preference nobody
  typed (an explicit `--account` is still refused there).
- **A project's always-on `root` agent is deliberately not scoped by it.** Its
  command comes from `[root_agent]` rather than an agent name, and a preference
  set about ordinary creates must not be able to stop a project's guaranteed
  session from starting. Scoping a root belongs beside the program it already
  names.

Set it globally (`af config set default_accounts.codex work`, no `--project`) for
a default everywhere. It is rejected from a repository's checked-in
`.agent-factory/config.toml`: identity policy is never checked in, and a
committed account name means nothing to anyone else who clones the repository.

To clear one:

```bash
af config unset default_accounts.codex --project ~/work/monorepo  # drop the project's override
af config set default_accounts.codex ""                           # clear the global default
af config set default_accounts.codex "" --project ~/work/monorepo # opt this project out of the global default
```

An **empty value is meaningful**, and the three lines differ. `unset` removes the
project's entry so the global default applies again; an empty value at either
layer means "no default", and because the key merges per agent, an empty entry in
a project's config wins for that agent and keeps its sessions on the ambient
identity even when the global layer names an account.

### What the account reaches

A pinned account applies to every process `af` starts for that session: the
agent pane, each helper tab, and the VS Code tab's editor — its integrated
terminals and anything an extension shells out to included. Each one gets the
account's credential home injected and the agent's other identity variables
removed, so an `ANTHROPIC_API_KEY` left in your shell cannot quietly outrank the
account you selected. If the account cannot be resolved when a VS Code tab opens
— it was removed or renamed since the session was created — `af` refuses to
start the editor and the pane names the `af accounts add` that repairs it,
rather than opening one on your ambient login.

One boundary `af` cannot hold for you: an **interactive shell you open inside
the editor** reads your `~/.bashrc` and `/etc/bash.bashrc`, and a line there
exporting the agent's credential variable wins over the account — exactly as it
would in any other terminal. `af`'s own panes escape that because `af` chooses
their command; a terminal profile inside the editor is yours, and it lives in
the editor's own settings, which the editor can rewrite. If you pin sessions to
accounts, keep agent credential variables out of your shell startup files.

For a process or shell sibling tab's command, `af` checks the **visible command
line** for an override of the account's identity or shell-startup variables — a
direct assignment, one nested behind a wrapper `af` models (`env`, `exec`,
`nohup`, `nice`, `timeout`, `setsid`, `stdbuf`, `ionice`, `taskset`, `xargs`,
`command`, `builtin`), or one nested behind a listed argv-passthrough wrapper
(`strace`, `ltrace`, `perf`, `valgrind`, `gdb`, `chrt`, `numactl`, `unshare`,
`prlimit`) that runs a child and forwards argv. A listed passthrough wrapper is
refused whatever its tail contains, since a PATH-shadowed copy can replace the
account environment in ways a static command-line scan cannot enumerate. The
check covers the visible command line and the modeled wrappers; a user's own
wrapper script (a `program_overrides` target `af` does not model) has internals
the command line cannot see, so what it does with the account environment is
outside what `af` can verify.

Launcher grammars get a deliberately conservative lexical check rather
than the passthrough rule, because they inject environment into their child
and refusing them outright would block ordinary use — `sudo`, `docker`
(including `docker compose` and the standalone `docker-compose` binary, plus
the CLI-compatible `podman` and `nerdctl`), `systemd-run` (including its
`run0` argv[0] alias), `bwrap`, `su` and `script`. The rule is keyed by the
grammar, so every name it ships under gets the same check. For these launchers
`af` over-refuses rather than under-refuses: every argument word of the
launcher's command line is scanned, and the command is refused whenever any
word could assign or name a protected variable — after stripping leading
dashes and any `-e`/`-E`/`--env`/`--setenv` prefix or `=` joiner, a word that
contains `<PROTECTED>=` or names a protected variable at an identifier
boundary refuses the command. The boundary means `ENV` embedded after `_`
does not match, so `docker run -e NODE_ENV=production app` and `sudo
RAILS_ENV=production …` run — while `-eENV=`, `-ECODEX_HOME=` and
`PassEnvironment=CODEX_HOME` still refuse. The rule covers every injection
spelling the launchers accept (`sudo CODEX_HOME=/other codex`, `docker run -e
CODEX_HOME=/other busybox codex`, `systemd-run --setenv CODEX_HOME=/other
codex`, `podman run -e CODEX_HOME=/other img codex`, `bwrap --setenv
CODEX_HOME /other codex`, `su -c 'CODEX_HOME=/other codex'`, `script -qc 'env
CODEX_HOME=/other codex' /dev/null`, and the `Environment=`/`UnsetEnvironment=`/
`PassEnvironment=` forms of `systemd-run -p`, getopt abbreviations like
`--prop=` included) without modelling each launcher's option grammar. Commands
whose arguments never mention a protected name run as before — `docker compose
up`, `docker-compose up`, `docker build .`, `docker run --rm alpine echo hi`,
`sudo apt-get install -y jq`, `systemd-run --user --scope make`, `run0 --user
--scope make`. The trade-off is deliberate: a harmless mention in a
non-assigning position, such as `docker run alpine echo CODEX_HOME=x`, is
refused too, because no parse of the visible command line can prove the word
will not reach the child's environment. Every operand these launchers read —
an env assignment, an exec name, a denied-name or env-file spelling — is
judged under the same flat normalization the named-launcher piece rule
applies (one pass deletes every backslash run plus a following line break and
every quote character, closed under nesting), so an operand that still spells
`CODEX\_HOME` or `co\dex` after the shell's own decode is read as the name it
could become (`strace -ECODEX\_HOME=/o`, `sudo 'CODEX\_HOME=/o' codex` and
`sudo 'co\dex'` refuse).

- **Env files are unverifiable** — inside the launcher arms `af` models,
  `--env-file`, `docker compose`'s `env_file` and `--env-from-file`
  spellings, and `systemd-run`'s `EnvironmentFile=` all name a file whose
  contents the command line cannot prove. They are refused even when the
  file would be safe: `af` cannot read it to know whether it assigns a
  protected name. An env-file flag under a command `af` does not model is
  not refused on this basis — it is just another argument word there.
- **Any dynamic word refuses** — a `$VAR`, `${VAR}`, `$(…)`, backtick,
  arithmetic or glob expansion anywhere in the launcher's command line could
  produce a protected assignment or an env spelling the visible command line
  cannot show, so the command is unverifiable and refused wherever the word
  sits (`sudo "$CMD" -i codex`, `docker run $IMG`, `docker run -e "$V"`).
  Ordinary dynamic uses pay the same price: `docker run -v "$PWD:/app"` and
  `sudo -u "$USER" id` are refused although their expansions would stay
  benign. A word-leading `~` is the one exception: it expands only to a home
  path, so `docker run -v ~/.npm:/root/.npm` reads as ordinary data.
- **systemd-run words are judged as systemd decodes them** — systemd
  unquotes and C-unescapes property values, so a `systemd-run`/`run0`
  argument still carrying a `"`, `'` or `\` after the shell's own quote
  removal is refused on sight: `-p 'Environment=CODEX""_HOME=/o'` and
  `-p 'Environment=CODEX\x5fHOME=/o'` both land `CODEX_HOME` in the unit's
  environment even though the raw text hides it.
- **The launchers' own environment sources are gated too** — `systemctl`'s
  `set-environment`/`import-environment`/`unset-environment` subcommands and
  `dbus-update-activation-environment` write a name into the service
  manager's (or dbus activation) environment, which a later `systemd-run
  --user` unit or activated program inherits in another tab. The write is
  the effect itself, so a protected name in one of these command lines
  refuses with no command word needed — `systemctl --user set-environment
  CODEX_HOME=/o` and `dbus-update-activation-environment CODEX_HOME=/o`
  refuse, while `systemctl --user set-environment PORT=3000` and
  `systemctl --user status sshd` run. The rule is deliberately lexical —
  no option grammar: systemctl's option space is wide enough that every
  grammar spelling was bypassed (operand before the writer, operand
  after it, the writer itself swallowed as an operand, a writer after
  `--`, a dynamic subcommand). So ANY non-literal word refuses, ANY
  literal word naming a protected variable at an identifier boundary
  refuses, and the LAST `set-environment`/`import-environment`/
  `unset-environment` literal — the real subcommand — must be followed
  by a non-empty run of plain env names: `systemctl --user
  import-environment` and `systemctl --property set-environment
  import-environment` (the operand spelled the first writer word; the
  real import has no names) bulk-import the caller's whole environment
  into the shared manager, so they refuse, while `systemctl --user
  import-environment FOO` runs. `dbus-update-activation-environment` is
  the writer itself, so every argv word must be a literal env name or a
  known no-operand flag — `--all` bulk-imports everything and refuses,
  while `dbus-update-activation-environment --systemd DISPLAY
  WAYLAND_DISPLAY` (the standard session form) and
  `dbus-update-activation-environment --verbose DISPLAY` run.
- **Environment-resetting launchers refuse only when they deliver THIS
  account's agent as the program** — a launcher that drops the pane's
  environment would unpick the account's pins silently, so when the reset
  environment's tail carries the account's own agent name in a word that
  can be the PROGRAM, the command refuses. "Program" means the tail's
  command word — the first literal that is not a provable option or
  option operand — or the decoded command word of a `-c` payload:
  `sudo codex`, `sudo /usr/bin/codex`, `sudo ./codex`, `sudo -u root
  codex`, `systemd-run --user --pty codex`, `run0 codex`, `su -l -c
  'codex'` and `bwrap --clearenv codex` all refuse (`codex` must be the
  word's FINAL path component). `sudo codex` was admitted before this
  change — the behavior change is intentional. A word that merely
  mentions the agent's name outside program position is not reach:
  `sudo npm i -g @openai/codex`, `sudo chown -R me ~/.codex`, `sudo cat
  /etc/codex/config.toml`, `sudo systemctl restart codex-proxy`,
  `systemd-run --user --pty --working-directory=/home/me/src/claude-squad
  make` and `su - postgres -c 'psql codex'` all run — package names,
  config paths, service names and tool operands are not the program, and
  ANOTHER agent's name is not reach for this account (`sudo claude`
  under a codex account runs). Non-agent uses stay admitted (`sudo -u
  postgres psql`, `systemd-run --user --pty make`, `run0 -p
  Description=x make`), as do the non-resetting forms (`sudo -E codex`,
  `systemd-run --scope codex`, `su -c 'codex' work`, `bwrap codex`) and
  containers, whose environment is image-defined rather than a reset of
  the pane's (`docker run busybox codex`). When the option walk can't
  prove which word is the command, every later literal is a candidate
  program word — the same dark-region cut, applied at program positions
  only — and a nested `su`/`runuser`/`script` payload inherits the reset
  (`sudo su -c 'codex' root` refuses). A preservation flag counts only
  as the launcher's own option —
  before its command word, before `--`, and not consumed as another
  option's operand — so `sudo -p -E codex` and `systemd-run codex
  --scope` still reset and refuse, while `sudo -p PROMPT -E codex`
  preserves and admits. `sudo make -E codex` runs: `make` is the
  program, so `-E` and `codex` are its operands — a `-E` inside a make
  recipe is a hidden-input channel, out of the gate's scope. Option parsing is trusted only
  where it is provable: a clustered operand (`sudo -uroot`), a
  getopt_long abbreviation (`--us` for `--user`, `--pro` for `--prompt`)
  or any unknown option can consume a following flag as its operand, so
  the first unrecognized option word ends the trusted region and the
  launcher reads as resetting — `sudo -uroot -E codex` and `sudo --pro -E
  codex` refuse even though real sudo parses `-E`/`--prompt`'s operand
  differently. Known no-operand flags (`sudo -n -E codex`) and
  `=`-joined operands (`sudo --user=root -E codex`, `systemd-run
  --setenv=FOO=1 --scope codex`) are provable, so they still admit. And
  su follows util-linux's actual precedence: its login form wins over
  `-p`/`--preserve-environment`, so `su -lp -c 'codex' work` and `su -l
  -p -c 'codex' work` refuse.
- **Command-string runners decode their `-c` payloads** — the
  `-c`/`--command`/`--session-command` operand of `su`, `runuser` and
  `script` is a script the child shell decodes, so every spelling
  getopt accepts — separate word, `=`-joined, clustered `-lc`, attached
  `-cx`, unambiguous abbreviations like `--comm` — is re-judged as a
  command line. `co""dex` lands as `codex`, so
  `script -q -c 'export CODEX_""HOME=/other; codex' /dev/null` and
  `su -l --session-command 'co""dex' root` refuse, while
  `script -q /dev/null`, `script -c 'make' /tmp/build.log` and
  `su -c 'co""dex' work` (no login flag — the caller's environment
  survives) run. `--` does not bound the payload scan — it can be an
  option's operand (`script -O --` names the logfile `--`), and a real
  terminator's tail is handed to a shell that decodes `-c` itself — so
  `script -q -O -- -c 'export CODEX_""HOME=/other; codex'` and `su --
  work -c 'env CODEX_""HOME=/o x'` refuse while `script -- typescript`
  and `su -- work` run. Nor does an operand slot: when it is uncertain
  which `-c` word is the command string — a `-c` can itself be an
  option's operand — EVERY candidate payload is judged, so
  `su -c -c 'env CODEX_HOME=/o x' work` refuses. A runner nested
  inside a resetting launcher's tail carries the reset into its
  decoded payload — `sudo su -c 'co""dex' root` and
  `sudo script -q -c 'co""dex' /dev/null` refuse because the child
  lands `codex` under env_reset, while `sudo -E su -c 'co""dex' root`
  (the pins are preserved) and `sudo su -c 'make' root` run.
- **`sh -c`/`bash -c` payloads get the same treatment** — a shell's
  command string is not hidden input: it is a command line the gate
  can read, so `sh`, `dash`, `ash`, `bash`, `ksh` and `mksh` decode
  every `-c` spelling (`-c x`, clustered `-exc`, `-euo pipefail -c`)
  and re-judge it with every check the gate applies to a command line —
  `bash -c 'make'`, `sh -c "npm test"` and `sh -c 'make' arg0 arg1` run,
  `sh -c 'unset CODEX_HOME; codex'` refuses the way the bare line does,
  `sh -c 'xargs -a secrets'` refuses for the same hidden input, `sh -c
  '/bin/sh -i < ./script'` and `sh -c '/bin/sh -i' < ./script` refuse
  for the same shell-fed-stdin, and the reset check reaches a decoded
  agent there too (`sudo sh -c 'codex'` refuses, `sudo -E sh -c
  'codex'` runs). EVERY word that can be the executed shell is decoded
  this way — a `su`/`runuser`/`script`/`env` word in front does not
  hide a later shell's `-c` behind that runner's own option grammar
  (`env -u su sh -ce 'unset CODEX_HOME; codex'` refuses). Refusal stays for what the
  gate cannot decode: a dynamic or unparseable payload (`sh -c "$x"`),
  a missing operand (`sh -c`), a script-file form (`sh script.sh`),
  an interactive/stdin spelling (`sh -i`, `sh -s`), an option letter it
  cannot model or one that changes what an assignment word does (`-k`,
  any `--long` option, `-O`), `-o` naming anything outside an
  allowlist of `set -o` names known not to change where commands come
  from or how assignments behave (`errexit`, `nounset`, `pipefail`,
  `xtrace`, `noglob`, `noclobber` and similar are fine; `keyword`, the
  `vi`/`emacs` input modes, `posix` and any other name refuse) — and
  bash reads the `-o` name from the NEXT argv word, so `-o vi`, `-oe
  keyword` and `-c -ox keyword` all refuse — and shells
  whose `-c` still reads a startup file (`zsh`, `csh`, `tcsh`, `fish`
  read zshenv/.cshrc/config.fish even for `-c`, and ksh's `-E` reads
  ENV/kshrc the same way — bash's BASH_ENV and ksh's ENV name a file
  only through the environment, which is the
  gate's stated limit; `-l` stays admitted since a non-interactive `-c`
  login shell has no extra input the gate cannot see). A bare `sh`,
  `bash` or `zsh` invocation stays
  refused as before — only the exact `/bin/sh -i` tab form is proven.
- **The named launchers are modelled by name** — `pkexec`, `doas`,
  `setpriv`, `machinectl`, `nix-shell`, `nix`, `flatpak-spawn`, `ssh`,
  `tmux`, `distrobox`, `toolbox`, `watch`, `pwsh`, `nu`, `busctl` and
  `gdbus` are the launchers whose tails one grammar-free text rule
  judges. The names matter because each can re-root the account's
  environment or carry a command the pane cannot see, in three shapes:
  - *Reset-class* — `pkexec`, `doas`, `setpriv --reset-env`,
    `machinectl shell`/`login`, `nix-shell --pure`, `nix develop`/`nix
    shell` with `-i`/`--ignore-environment`, every `flatpak-spawn`
    (each spawn gets the sandboxed environment), `ssh` (the remote
    command runs under the host's own environment), `tmux`'s
    pane-creating subcommands and their aliases (`new-session`/`new`,
    `new-window`/`neww`, `split-window`/`splitw`, `respawn-pane`/
    `respawnp`, `respawn-window`/`respawnw` — a pane runs under the
    tmux SERVER's environment, never the pane's), and
    `toolbox`/`distrobox`'s `enter`/`run` forms. Each refuses only
    when its reset reaches the selected account's own agent as the
    program — the A′ rule `sudo codex` already keeps — so `pkexec
    codex`, `ssh localhost codex`, `tmux new-window codex`,
    `nix-shell --pure --run codex` and `flatpak-spawn --clear-env
    codex` refuse, while `pkexec ls`, `ssh host make`, `tmux
    new-window htop`, `machinectl login host` and `distrobox enter x`
    run. A denied env spelling in the launcher's own argv refuses the
    same way (`flatpak-spawn --env=CODEX_HOME=/o make`), and a buried
    form still judges at its own suffix (`prog pkexec codex`,
    `sudo -E pkexec codex` — the buried reset survives sudo's -E).
  - *Command-string runners* — `nix-shell --run`/`--command`, `nix -c`
    on the develop/shell subcommands, `ssh`'s remote command, every
    tmux pane command string, `distrobox -e`, `toolbox -e`, and
    `watch`'s argv (which `watch` joins and hands to `sh -c`) are
    payloads the gate re-judges as command lines: `ssh localhost
    'CODEX_HOME=/o codex'`, `tmux respawn-pane -k 'CODEX_HOME=/o
    codex'`, `distrobox enter x -e 'CODEX_HOME=/o codex'` and `watch
    'export CODEX_HOME=/o; codex'` refuse, while `watch -n 5 'kubectl
    get pods'`, `nix-shell -p hello --run hello` and `ssh localhost
    'make -j4'` run. `pwsh` and `nu` are NOT POSIX-decoded — the gate
    cannot parse their payload grammar, so the approved rule refuses
    the `-c`/`-Command`/`--commands`/`-e` operand only when its text
    names a protected variable or the account's agent at identifier
    boundaries (`pwsh -c '$env:CODEX_HOME="/o"; codex'`, `nu -c
    'codex'` refuse), and admits it otherwise (`pwsh -c Get-Process`,
    `nu -c 'ls'`). Encoded-command spellings (`pwsh -enc`,
    `--EncodedCommand`) carry unreadable text and refuse, as do the
    interactive/script-file forms (`pwsh script.ps1`, `nu script.nu`)
    — the `sh script.sh` class.
  - *Environment writers* — `busctl`/`gdbus`'s `SetEnvironment` call
    and `tmux setenv`/`set-environment` write a launcher's own
    environment source (the systemd user manager and the tmux server's
    global environment respectively), so a literal word spelling a
    protected NAME or `NAME=value` anywhere in the argv refuses —
    `busctl --user call … SetEnvironment as 1 CODEX_HOME=/o`, `gdbus
    call … SetEnvironment CODEX_HOME=/o` and `tmux setenv -g
    CODEX_HOME /o` refuse — while non-protected names and ordinary
    calls run (`tmux setenv -g FOO /o`, `busctl --user get-property
    org.a /x org.a.Name`).

  The verdict for a named launcher is a grammar-free text rule — the
  gate never parses a launcher's options, so no finding of the form
  "option X's position, arity or value hides the command" can exist.
  For the whole tail, refuse when ANY of these holds:

  - a literal piece names the account's agent or a protected variable
    at identifier boundaries — a piece is every argv word and every
    piece of a word split on whitespace, `=`, `;`, `&`, `|`, quotes
    and parentheses, and the agent also matches as a path basename.
    `ssh localhost -p 2222 codex`, `tmux new-window codex`,
    `flatpak-spawn --env=CODEX_HOME=/o make`, `busctl … SetEnvironment
    as 1 CODEX_HOME=/o`, `tmux list-sessions \; new-window codex` (the
    `;` split sees the second command's words), ssh's `-o
    'RemoteCommand=co""dex'` and `watch 'export CODEX_\HOME=/o;
    co\dex'` all refuse, whichever options precede them — each literal
    is checked raw AND normalized: one flat pass deletes every
    backslash run (plus a line break — LF or CRLF — right after it,
    sh's line continuation) and every quote character, an
    over-approximation of POSIX escapes closed under nesting, so
    `co""dex`, `co\dex`, `co\<newline>dex` and `co\\\\dex` — quoting
    quoted again at any depth — all check as `codex`, and a piece
    whose bracket atom sits among literal letters that could spell a
    name is matched as a pattern since pathname expansion happens at
    run time — `co[d]ex` or `co[[:alpha:]]ex` can resolve to the
    codex on disk (the match over-approximates any `[…]` as one
    character; an unterminated `[` is literal, as POSIX keeps it),
    while a bare `*`/`?` is never a name reading — `*.log`, `ls
    /tmp/*` and `tar czf - *` all admit exactly as master does — and
    brace expansion, which runs before globbing on downstreams that
    do it, is checked per alternative: `co{de,}x` reads as `codex`,
    a `{x..y}` range over-approximates to the `[x-y]` atom, and a
    comma-less `co{ab}x` stays literal as bash would keep it;
  - a word does not literalize — a substitution or expansion in the
    argv itself (`sudo "$CMD" -i codex` hides its program the same
    way master hides it). Text the gate CAN read is judged on what
    it literally spells: a `$`, backtick, `$'…'`/`$"…'` or extglob
    opener inside a modelled launcher's carried text constructs a
    name only at run time, which is the stated #7641 limit rather
    than a refusal — `tmux new-window 'cd ~/src && $EDITOR .'` and
    `watch 'printf B64 | base64 -d | sh'` admit exactly as master
    admits them;
  - a word spells input the gate cannot see — the per-launcher
    hidden-input table: a descriptor, stdin, or a language it does not
    parse (flatpak-spawn's `--env-fd`, nix's expression flags and
    `--arg-from-stdin`, `pwsh -EncodedCommand`/`-EncodedArguments`,
    nu's `-e`/`--stdin`), resolved the way the launcher resolves it —
    the leading run of option prefixes goes (`-`, `--`, and for pwsh
    `/`, `–`, `—`, `―` too), a `:`/`=` operand attachment is cut,
    pwsh's body reads case-insensitively, and a body that IS a hidden
    name or its unique prefix refuses (`/ENC`, `–EncodedCommand`,
    `-ea`); a single-dash word attaches its operand in-word for
    clap/nix spellings (`-Eexpr`), and a body that also prefixes a
    non-hidden option is ambiguous — the real launcher errors on it,
    so it admits (`-ex` is -ExecutionPolicy, nix `-i` is
    --ignore-environment) — the same refusal class `sh -s` gets. The
    classification runs on EVERY reading of the word — `-en""c` is the
    `-enc` the downstream shell sees — a glob- or brace-carrying body
    classifies by what pathname expansion could produce (`-EncodedComman
    [d]` can land `-EncodedCommand`; `-ex[ec]` cannot land a hidden
    spelling and admits). The glob judgement covers the produced word's
    whole option shape, not just its body: an atom that could produce
    the prefix is part of the run (`[-]EncodedCommand` can land
    `-EncodedCommand`, `?EncodedCommand` and `*nc*` likewise, and a
    `[-]` before `E` can land `-EE` — nix's `-E` with the operand
    attached), a `:`/`=` the production spells cuts the body, and a
    produced prefix run is judged at its own length (`-[-]xpr` lands
    `--xpr`, which nix does not resolve to `-expr`) — and a
    command-string flag left with no operand is that same stdin feed
    (`pwsh -Command`, `nu -c` alone);

  A launcher that hands its tail argv to a shell as ONE joined string
  (watch by default, ssh's remote command, tmux's and machinectl's
  command lines) is judged on the re-split words too, never only on the
  preserved argv fields — `watch pwsh '-enc x'` is `pwsh -enc x` to the
  downstream shell, so an option and its operand sharing one argv word
  classify separately, a nested name triggers across argv boundaries,
  and operator characters delimit words exactly as whitespace does —
  `pwsh -EncodedCommand</dev/null` is the hidden option plus its
  redirect, and `pwsh -enc|cat` the option plus a pipe;

  Launcher names nest by NAME, not by position: a piece (or word, or a
  field of a nested tail) that spells a modelled launcher's name
  re-enters that launcher's own rule on the text after it — `watch 'pwsh
  -EncodedCommand …'` refuses exactly as a direct `pwsh -EncodedCommand`
  does, at any depth, inside the same total work bound. A piece can
  spell several names (`nix-shell` is `nix` at a `-` boundary and
  `nix-shell` exactly) and EVERY match applies its own rule, so
  `watch 'pwsh -enc x'` refuses on pwsh's table and `watch sudo pwsh
  -EncodedCommand x` on pwsh's. The name lookup is the same one the
  agent name gets — the normalized piece's path basename with bracket
  atoms over-approximated, so `/usr/bin/pwsh`, `./pwsh`,
  `~/.local/bin/watch` and `/usr/bin/pw[s]h` all read as the launcher —
  while a bare `*`/`?` spells no launcher name, the way master reads it.

  `gdbus` call operands are GVariant text decoded through their
  escapes alongside the raw check (`['CODEX\u005fHOME=/o']` still
  names CODEX_HOME, and an escape the decoder does not know refuses).
  Every collapsed reading is ADDED to the plain reading, never
  substituted for it: a glob-bracket `[…]` is one atom to the piece
  reading while its interior is judged as plain text on its own span,
  so `[; pwsh -EncodedCommand x ;]` cannot hide the launcher the atom
  would otherwise eat. Work is one linear pass — each word
  literalizes once, each piece checks once — and nested launchers
  share one total budget with per-word verdicts memoized; the
  character-level scans (brace alternatives, bracket matching, glob
  atoms, interior spans) charge to that same budget, so a
  pathological tail or nested pattern (a tmux inside an ssh inside a
  watch inside a nix-shell) refuses inside the bound rather than
  paying quadratic work; spending past the budget refuses. An option
  spelling the gate does not know is just text: `ssh --frobnicate
  host make` admits, the way the real launcher's own error path
  would.

  The accepted cost is measured, not hidden: the rule refuses tails
  that merely MENTION a denied name — `ssh host 'cat
  notes/codex.md'`, `tmux new-window 'vim codex.md'` and `watch
  --exec codex` name codex at a boundary. The ordinary-usage corpus
  (the 162-row master-parity set plus the `ssh host make`, `tmux
  new-window htop`, `watch -n 5 'kubectl get pods'` admits) loses no
  row to the rule; the losses are the accidental-mention rows above,
  stated here rather than predicted away. Commands and config read
  from a FILE are the stated limit, not a refusal: `ssh -F
  /tmp/cfg`, `ssh -vF /tmp/cfg`, `tmux -f /tmp/cfg`,
  `tmux source-file /tmp/cfg`, `pwsh script.ps1`, `nu script.nu`,
  `nu --config /tmp/override.nu` and `nix-shell --arg-from-file
  /tmp/f` all admit the way `bash ./build.sh` does — the gate judges
  the visible command line, and a file's content is outside it.
- **Shell names are master's own list** — `KnownShellName` matches
  the exact spellings master models (`sh`, `bash`, `dash`, `zsh`
  and the POSIX/csh/fish family), so a restricted or versioned
  spelling the list does not cover (`rbash`, `bash5`, `zsh-5.9`,
  `ksh93`) reads as an ordinary program exactly as master reads it:
  `rbash -c 'export CODEX_HOME=/o; codex'` and `zsh-5.9 -c 'make'`
  keep master's verdicts, and `bash -c 'export CODEX_HOME=/o; codex'`
  still refuses on master's own `-c` rule.
- **Hidden inputs refuse** — a flag that loads arguments or
  environment from a file or file descriptor puts the real command
  outside the VISIBLE command line, so `bwrap --args 3`, bwrap's
  file-style injectors (`--file`/`--bind-data`/`--ro-bind-data`/
  `--symlink`/`--seccomp`/`--add-seccomp-fd`), `--env-file` spellings
  and `xargs -a`/`--arg-file` all refuse; `docker build --file
  Dockerfile` reads a build file, not child argv, and runs. The gate
  judges the visible command line — content inside script files,
  sourced files or programs it cannot read is out of scope; its job
  is the obvious, accidental re-root, and a determined user can
  always write a script file.

A launcher buried inside another command's argv (`nohup sudo
CODEX_HOME=/other codex`, `docker build env CODEX_HOME=/other codex`) is
judged the same way the bare command is: the ordinary mutation scan runs over
the launcher's whole tail, so a buried `env`, `nohup`, or modelled launcher
still refuses (`prog sudo CODEX_HOME=/other codex`, `prog bwrap --setenv
CODEX_HOME /o codex`, `prog systemctl --user set-environment CODEX_HOME=/o`).
A command `af` does not model at all is judged exactly as it always has been —
the walk that has always scanned its tail for nested `env` invocations,
unprovable words, option-carried assignments and shell forms, and nothing
more. A protected NAME, a `NAME=value` token or a builtin-spelled word in
such a tail is inert data: `prog CODEX_HOME=/o codex`, `echo CODEX_HOME=/x
a`, `make CODEX_HOME=1 install`, `prog --setenv CODEX_HOME /o codex`,
`rg CODEX_HOME internal/ session/`, `grep -rn OPENAI_API_KEY src docs`,
`git log -S CODEX_HOME -- internal`, `gh issue list --search CODEX_HOME`,
`make ENV=prod test` and `grep PS1 .bashrc .zshrc` all run, exactly as on
master. The launchers the gate models are the ones named above — every
other environment-changing program (`schroot`, `chroot`, `jexec`,
`nsenter`, `firejail`, `containerd`/`ctr`, `kubectl exec`, `vagrant`,
`mosh`, `direnv exec`, `guix shell`, and any launcher new enough to
ship a grammar `af` has not written down) is still read only by the
unrecognized-tail scan, and a protected name inside its argv stays
inert data. Closing that residual gap is deliberately a name-at-a-time
job: each launcher's grammar is modelled explicitly rather than
guessed.

### Docker and backend boundaries

Account selection supports local and Docker sessions, and ssh sessions on a host in
the global host list, and requires tmux 3.2 or
newer. Docker bind-mounts the selected credential directory read-write so token
refreshes reach the real account. This differs from
`docker.mount_agent_credentials`, which opts into mounting an ambient agent
credential read-only. Account selection must establish the selected identity
or refuse the create; it never silently uses an ambient login.

On an ssh session placed on a listed host (`--host <name>`), af syncs the account's
credential files into that session's own directory on the host, writes refreshes
back, and removes them at kill and archive; see
[Session-scoped account sync](backends.md#session-scoped-account-sync-listed-hosts).
A `default_accounts` entry is applied the same way, and on a target that is not a
listed host it is not applied (the session runs on the host's own credentials)
rather than failing the create. An explicit `--account` on an unlisted ssh target,
or on a sandbox or hook target, is refused with the remedy. Automatic account
replacement also excludes Docker and every off-box session; see
[Opt-in auto-resume](#opt-in-auto-resume).

**Environment names the agent's own loader can fill.** Gemini's `.env` loader
walks up from the workspace and fills only names that are unset — so on an
account-scoped gemini session, the names a checked-in `.env` could use to
redirect the account's identity are pinned by af at the agent boundary:
`CLOUD_SHELL`, `GEMINI_CLI_USE_COMPUTE_ADC`, `GCE_METADATA_IP`,
`GCE_METADATA_HOST`, `GIT_CONFIG_COUNT`, and `GIT_CONFIG_PARAMETERS`. The pin
is **keep-if-set**: a name the operator forwards through the global
`session_env_passthrough` carries the operator's own value — that is the
claim af exists to deliver — while a name the operator did not forward is
installed defined-and-empty, so neither a workspace `.env` nor a repo- or
image-supplied value can claim it. A forward is a real choice, not decoration:
forwarding `CLOUD_SHELL` or `GEMINI_CLI_USE_COMPUTE_ADC` set true moves
Gemini's authentication off the selected account onto the host's Compute ADC
entirely, and a `GIT_CONFIG_*` forward lets the operator — not the repository
— supply git config into the session.

Gemini's first-party endpoint overrides (`CODE_ASSIST_ENDPOINT`,
`CODE_ASSIST_API_VERSION`, `GOOGLE_GEMINI_BASE_URL`,
`GOOGLE_VERTEX_BASE_URL`) are pinned on an account-scoped session too, but
**unconditionally** rather than keep-if-set: the three names gemini reads with
falsy checks are installed defined-and-empty (gemini then falls back to the
first-party default), while `CODE_ASSIST_ENDPOINT` — which gemini reads with a
nullish coalesce that would treat defined-empty as a real — broken —
endpoint — is dropped from the ambient environment and installed with that
default instead.

Where the keep-if-set test is evaluated is where the value lives, and that
differs per backend:

- **Local** — at the pane boundary, on the environment composed for the pane:
  names the operator's tmux `update-environment` import delivered keep their
  imported value, and names absent there are pinned empty.
- **Docker** — in two places. At `docker run` a forwarded name the daemon
  environment sets rides bare `-e NAME`, and a forwarded-but-unset loader or
  indexed-pair name (`GIT_CONFIG_KEY_*`/`GIT_CONFIG_VALUE_*`, which a forwarded
  count would activate) is emitted `-e NAME=` so an image `ENV` value cannot
  reach the agent as if it were the claim; then at the agent's own exec filter
  the names still unset are pinned empty. Any other forwarded-but-unset name
  keeps the ordinary contract — no `-e`, so the image's own value stands.
  `docker.run_args` entries naming any of the six — or the indexed
  `GIT_CONFIG_KEY_*`/`GIT_CONFIG_VALUE_*` pairs — are refused: forward the
  pairs through `session_env_passthrough` instead.
- **SSH and sandbox** — in the remote launch shell, evaluated against the
  *remote* environment, because that is where a forwarded value lives. A name
  set remotely — including defined-empty — keeps its remote value; a forwarded
  name unset remotely is set empty there; an unforwarded name is pinned empty
  unconditionally.

`GIT_CONFIG` itself is deliberately not pinned: git reads a defined-empty
value as a real config-file path and `git config` would break. On claude and
codex sessions none of these names are pinned, so the `GIT_CONFIG_*`
`safe.directory` passthrough those agents use keeps working.

### Account-scoped handoff

Local account-scoped sessions support both same-agent and cross-agent handoffs.
Use `af sessions handoff <session> --account <name>` to choose another registered
account of the current agent. Add `--to <agent>` to change agent and account
together. The target account must belong to the incoming agent and must not be
currently walled in the daemon's limit ledger.

An explicit `--account` selects a pinned identity: an existing pin moves to the
chosen account, and an ambient session becomes pinned to that account. For an
ambient session, `--to <agent>` without `--account` keeps the replacement ambient.
A session already scoped to an account must specify a target account when
changing to an agent whose resolved command supports accounts (claude, codex,
gemini as shipped); omitting it does not bypass the pin. Handing it to a target
whose resolved command has no account support drops the scope instead, and the
response reports the drop on `from_account`. Capability follows the resolved
command rather than the enum, so `program_overrides` moves a target between the
two cases: `aider` redirected to `codex` requires a codex account, and `codex`
redirected to `aider` drops the scope. A third case refuses outright: a target
whose resolved command af cannot classify as an agent at all (a wrapper such
as `npx codex` may launch an account-capable agent underneath) can neither be
proven to carry the scope nor proven safe to drop it, so the handoff refuses
rather than destroy a durable pin on an unproven answer — point the override
at a literal agent command to make it classifiable. Because a scope drop
restarts only the agent pane, a session with shell, process, or VS Code
sibling tabs is refused until those tabs are closed — they would keep running
under the dropped account's environment.

A scope drop is one-way: the session records the dropped name on the handoff's
`from_account` and runs ambient from there, but a later handoff back to an
account-capable agent does not restore it — name the account again with
`--account`.

The session keeps its worktree and branch. See [Hand off to another account](#hand-off-to-another-account)
for the conversation outcome, examples, admission checks, and the TUI and web pickers.

### Bug report redaction

Bug report bundles replace session account fields and registered account labels
with a redaction marker, including labels embedded in collected config and log
text. Credential directories and their secrets are not collected. The local
operator log retains account names so login and switching remain diagnosable;
label redaction happens when the bundle is assembled. If the registry cannot
be read, collection still produces a bundle but reports that account labels
could not be redacted. Review that collection error before sharing it.

## Usage limits

Subscription-plan agents (`claude`, `codex`, `devin`) can hit a **plan
usage-limit wall**: the CLI stops working and prints a banner like *"Claude usage
limit reached. Your limit will reset at 2pm (America/New_York)"* or *"You've hit
your usage limit … try again at Jul 25th, 2026 5:55 PM"*. The agent is not dead —
it is parked until its limit window resets.

Agent Factory detects that state, surfaces it, and can bring the session back on
its own once the window elapses. This page covers the whole flow end to end.

## What is detected

- **`claude` and `codex`** — their usage-limit banners are recognized, and when
  the banner states a reset time it is parsed into an absolute instant. These are
  the plan-metered agents that stall at a dead prompt with a reset window, so they
  are the ones whose reset time schedules a **timed** auto-resume.
- **`devin`** — detected too (#2411), but **detect-only**. Its exhaustion banner
  is inferred from the binary and docs rather than captured live, and it carries
  **no reset time**, so a devin limit gets the badge and the manual `c` retry but
  never a scheduled (timed) auto-resume — only the `limit_retry_interval` fallback
  if `limit_auto_resume` is on. Its healthy `N% remaining` / `resets in …`
  quota-status line is deliberately **not** treated as a limit.
- **`gemini` and `aider`** — **not** detected. They are API-key-metered: a
  "limit" there is a transient HTTP 429 the CLI already retries, with no plan
  reset time to schedule against. They are surface-only in the sense that
  nothing special happens — no badge, no auto-resume.
- **`opencode`** — **not** detected, deliberately. It is API-key-metered against
  your own Anthropic/OpenAI credentials, and its TUI reports spend (`$0.10
  spent`) rather than a plan wall — there is no plan-reset banner to match and no
  reset window to schedule against, so af ships no usage-limit matcher for it.

Detection runs on captured pane content, so it needs no agent cooperation. You
can tune the detection regex per agent with
[`limit_patterns`](#custom-detection-patterns).

Claude reset times honor a parenthesized timezone accepted by Go's
`time.LoadLocation`, including single-component names such as `(UTC)` and
`(Japan)` and slash names such as `(America/New_York)` or `(Etc/GMT+5)`.
When several parenthesized groups appear, the first loadable zone wins. If the
clock parses but no zone loads, one warning names the banner and all rejected
candidates, and the wall records no reset rather than guessing the daemon's timezone. An unparseable
clock does not warn or suppress a later warning. Repeated warnings are
suppressed by a process-local cache of the last 64 distinct banner-and-candidate keys; an
evicted banner can warn again. A banner without a zone uses the
daemon's timezone. The sidebar displays the resulting instant in your local
timezone; auto-resume uses that same instant plus its grace buffer.

## The `[limit]` badge

When the daemon's status poll sees a usage-limit banner for a `claude`, `codex`,
or `devin` session, it marks the session **LimitReached**. In the sidebar the row
shows a `[limit]` badge, and — when the banner carried a parseable reset time (not
`devin`, which carries none) — when the limit resets:

```
▸ fix-auth-bug   [limit] resets 2:00 PM
```

`[limit]` means "parked at a usage-limit wall", distinct from working, idle
(ready), or dead. The badge clears automatically once the session resumes work
(whether you resume it, the daemon auto-resumes it, or the banner scrolls away
on its own).

## The usage report

`af quota` prints a per-agent report of the same signal, answered on all three
surfaces identically — the TUI and web render it as the **Usage** section of
the config view, the CLI as a table:

- **TUI**: open the config overlay (`,`) and scroll to **Usage**.
- **Web**: open the **Config** view; Usage sits under Accounts.
- **CLI**: `af quota` — and `af quota --daemon-url <url>` reports on the
  remote daemon's host, not yours.

Each row keeps two things apart on purpose: **quota** is what the provider
reports about the account's ceiling — `not reported` everywhere today, because
no supported agent exposes a quota API and af does not guess one — while
**observed** is what af's own sessions show (running counts, a session parked
at a wall, and the earliest recorded reset). A record file af could not read
makes the report say so rather than look complete.

## Manual retry

Resume a limit-blocked session immediately from any surface:

- TUI: select it and press **`c`**.
- Web: select it and click **Retry limit**.
- CLI: run `af sessions retry-limit <title>` (with `--repo` when needed).

Every surface calls the same daemon recovery action. It:

1. Re-spawns the agent if its tmux session exited while blocked (the rare case);
   a live stall just gets nudged.
2. Re-delivers the pending prompt — a task-driven session re-sends its **stored
   task prompt** so it resumes its actual work; an interactive session with no
   stored prompt is sent a bare `continue` (which loses the agent's prior
   in-context state, an unavoidable limitation — see
   [anthropics/claude-code#5977](https://github.com/anthropics/claude-code/issues/5977)).
3. Clears the `[limit]` badge so the poll re-resolves the real state.

The TUI's `c` binding is rebindable like any other via the `[keys]` table
(action `limit_retry`); see
[configuration.md](configuration.md#key-bindings-keys).

## Opt-in auto-resume

By default a limit is **surface-only**: the badge plus the manual retry. Set
`limit_auto_resume = true` in your global config to let the **daemon** resume a
parked `claude`/`codex` session on its own once its limit window elapses (a
reset-less banner — including every `devin` limit — resumes on the
`limit_retry_interval` fallback below instead):

```toml
limit_auto_resume = true
limit_account_candidates = ["work", "personal"] # optional account-switch order
limit_retry_interval = "30m"   # fallback cadence when a banner states no reset time
```

- **Parsed reset time** → the daemon resumes shortly after that time (a small
  grace buffer is added because limit windows are rolling and approximate). A
  reset time already in the past resumes promptly.
- **No parseable reset time** → the daemon retries on the fixed
  `limit_retry_interval` cadence. Set it to empty or `0` to leave such a session
  surface-only.
- **Re-limit backoff** → if a resumed session immediately hits the wall again,
  the daemon backs off exponentially (settling at one attempt every 5 minutes)
  rather than hammering an exhausted plan. Killing the session is always the
  off-ramp.
- **Optional account switch** → before waiting for the old identity's reset, an
  unpinned local session may restart on the first registered account
  in `limit_account_candidates` that has no current limit observation. The
  ordered list is the opt-in; af never chooses from every registered account.
  If every listed account is limited or absent, the existing wait remains.
  Docker account-scoped creates remain supported, but automatic Docker
  replacement stays disabled until its complete provision plan and container
  cleanup identity can survive a daemon crash.
- **An explicit `--account` is pinned.** It is a pin, so automatic switching
  never overrides it. Accounts selected by the scheduler may move again after
  they later hit their own limit.
- **No invented quota claim.** Providers expose no quota API af can read. The
  scheduler uses the same honest evidence as `af quota`: a candidate is rejected
  when af currently observes one of its sessions at a limit; absence of that
  observation is not presented as a provider guarantee. A recorded wall ends
  exactly two ways: when its claimed reset passes (plus the short rolling-window
  grace), or when the account's own usage record — the `rate_limits` snapshot
  codex writes into its rollout store — reports every window below the
  configured wall at a record time comfortably after the wall was last seen,
  which is positive proof it is serving again. That proof is kept as a
  retraction in the same ledger so one stale sighting cannot exclude a
  recovered account for days, and it is ordered by the record's own timestamp,
  never by when af happened to read it. The timestamp is the moment codex
  *wrote* the line — a response completing after the wall can belong to a
  request the wall already blocked — so af requires the record to clear a
  thirty-minute margin past the last sighting before it counts; the record
  format carries no request-start field to order more precisely. Only a
  verified usage reading counts: today that is codex alone, so a claude,
  devin, or other agent's wall still ends at its claimed reset — or never,
  when none was claimed. A reading that reports no windows — or omits any
  window the account is subject to, like `"secondary": null` — is silent
  about that window's wall, not proof it is below; and the indexer only
  scans each account's newest few rollouts —
  content the cap leaves unexamined reads as unknown only while it
  could still hold a newer record than the newest found claim. The
  indexer bounds that with an assumed horizon, not a guarantee: a
  record can postdate its file's mtime only within the day of clock-skew
  overshoot the indexer allows, and a conversation resumed past the
  thirty-day append horizon, or a record skewed past the overshoot,
  falls outside the bound — so skipped content could then hide the
  truly-newest claim and the reading lags one file. Exceeding the
  rollout window no longer reads as unknown once the missed content is
  irrelevant; the wall itself persists until it expires or a proof
  retracts it, which a reading that is hot, incomplete, zero/future-dated,
  or silent on a window the account is subject to never supplies, and
  which must clear every wall's ordering margin. A proof
  dated in the future is quarantined permanently — dropped from the ledger
  rather than suspended — so an ordinary clock correction can never let one
  count later. Records an account inherited from a copied transcript prefix
  are the source account's numbers: a carry commits a durable byte watermark
  at copy time naming exactly which leading bytes came from the previous
  account, and the usage reader ignores claims below it — a carry that
  cannot record its watermark commits only when the copied span is
  quarantined in the rails file instead, so no claim inside it frees a
  wall until a covering watermark lands. When neither durable record can
  be written the carry does not commit at all: the install is unwound
  and the replacement starts a fresh conversation whose brief says why —
  committing bytes no record can mark untrustworthy would silently let
  the source account's claims count as the target's. The
  quarantine ledger is bounded, and its cap never evicts a marker — a
  dropped marker would re-trust a file whose poison span is still on
  disk. A failed carry that cannot get a marker flags its account
  instead: that account's usage claims are then untrusted for retraction
  wholesale until a watermark covering the named rollout lands, or the
  rails file is repaired. Pane chrome, busy
  indicators, a delivered prompt, and idle or attached pane state prove
  nothing either — none of them shows the account accepted a request. And
  because the proof must postdate a claimed reset too, retraction mainly
  frees walls that named none — a wall that named one cannot be retracted
  before that instant, and by then it expires on its own. Nothing else ends
  a wall: not a timeout, and never the mere absence of a wall. A wall
  sighted after the proof is the newer fact and excludes again, and a
  session parked at its own `[limit]` stays parked — retraction clears the
  stored record admission consults, never a live wall. A parked session that
  re-reads the same unchanged wall records no new sighting at all — the
  parked row keeps its original instant rather than churning a durable
  write and an event for every poll, which is how a retraction's ordering
  stays honest about when the wall was genuinely last seen.
- **Visible in the session.** The first prompt after replacement names the old
  and new identities before repeating the stored task prompt.
- **One credential boundary.** A local swap stops every agent, shell, and process
  pane before committing the new identity, then restores the agent and shell
  panes with the selected account environment. A claude or codex agent keeps
  its conversation: af copies the transcript into the new account's home before
  committing the new identity, and the replacement resumes it. The copy stays in
  the new account's home, and the provider replays that history under the new
  account, so an automatic rotation moves the conversation's content to the
  candidate account as well as the work. If the conversation cannot be carried,
  the agent starts a fresh conversation, and the notice it receives says why.
  Other agents start a fresh provider conversation. New
  account-scoped terminal tabs remain interactive but skip shell startup files,
  because an rc file can otherwise replace the selected identity after af has
  established it. A resolved command that explicitly pins `--continue`,
  `--resume`, `--session-id`,
  or `codex resume` would take away af's choice of which conversation the
  replacement opens, so af names those arguments and keeps the existing wait
  instead.
- **Operator-only.** `limit_auto_resume`, `limit_retry_interval`, and
  `limit_account_candidates` are rejected
  in in-repo configs. A save through `af config set` applies them to the running
  daemon at once — no restart. The candidate list may also be replaced in a
  machine-local personal-project config, so one project can use a narrower set
  without trusting checked-in repository config with identity policy.

Set the global ordered list with:

```sh
af config set limit_account_candidates work,personal
```

Or add `--project <id-or-path>` for a personal per-project override.

Full config reference: [configuration.md](configuration.md#usage-limit-auto-resume).

## Hand off to another account

To continue under another registered account of the same agent:

```sh
af sessions handoff fix-auth --account personal
```

The session keeps its identity, worktree, branch tip, and stored prompt. For
claude and codex **on a local worktree** it also keeps the conversation. Each
account has its own provider home (`CLAUDE_CONFIG_DIR` or `CODEX_HOME`), so
after stopping the outgoing agent af copies that conversation's file into the
new account's home: the transcript for claude, the rollout for codex. The new
account then resumes the same conversation id. The copy only ever adds to the
new account's home; nothing in the previous account's home changes.

An off-box (listed-host ssh/sandbox) `--account` swap is different: af cannot
reach the previous account's provider store on the host, so the new account
always starts a **fresh conversation** with a notice saying the earlier
messages are unavailable — and handing back starts fresh again, since nothing
was carried either way.

The copied transcript is kept in the new account's home, and from then on the
provider replays that history under the new account, whose credentials send
it. Treat a handoff, manual or automatic, as moving the conversation's content
to that account.

The new account starts a fresh conversation instead, and its brief says af
tried to carry the conversation and why it could not, when:

- the file is missing, or reached through a symbolic link;
- the new account already holds a different version of it, including a
  copy under the other spelling of a symlinked worktree's path that is newer
  than the carried one or that the carried one does not extend (af's log names
  the file; move it aside before the next swap);
- the conversation af recorded is no longer the newest one in this worktree,
  because a new one was started with a Claude `/clear`, or the session moved
  to a thread af didn't see, whether a new one from a Codex `/new` or an older
  one it resumed (af follows a Codex session to a thread it starts with `/new`
  or resumes with `/resume`, so the swap carries the thread it followed). This
  applies only in a worktree af created for the session: in your own checkout
  (`--here`, an adopted worktree, the root agent) your own agent files
  conversations there too, so a newer one says nothing about the session and
  af carries the recorded one, as a restore resumes it. For a worktree reached
  through a symlink, Claude can file conversations under the path as given or
  as resolved, so af checks both. When both hold a copy of the conversation,
  af carries the one that holds every turn of the other, and refuses when the
  two copies diverge (af's log names both files; move aside the one whose
  turns you don't want to keep before the next swap);
- the recorded Codex thread was backtracked in Codex (Esc-Esc) and now
  continues in a thread/revert rollout af does not carry, so the new
  account starts fresh rather than resume the turns the user backtracked
  away from;
- the new account already holds a thread/revert rollout of this
  conversation, which its Codex would resume instead of the carried
  copy, so the carry cannot put that copy in front of it;
- the recorded Codex thread was forked in Codex (`/fork`) and its
  rollout holds only the turns after the fork, while the history it
  shares with its parent lives in the parent's rollout, which af does
  not carry, so the new account starts fresh rather than resume a fork
  whose lineage it cannot resolve;
- af could not look at the conversation's file on either home within
  the bound (an unavailable or slow mount), so the new account starts
  fresh rather than carry the conversation af could not verify;
- a replacement already launched on the carried conversation stopped before it
  was confirmed working.

Other agents always start fresh with a brief. Use `--brief` to replace the prompt, or combine
`--to claude --account work` to change both agent and account; changing the
agent always starts a fresh conversation. The recorded handoff includes the
outgoing and incoming accounts and branch tip.

The `af sessions handoff` response answers the same question its output
carries under `conversation`: `carried` when the transcript was copied and
resumed, or `carry_failure` naming why the replacement starts fresh — a copy
that could not be made, a sandbox store af cannot reach, or a different
program. Retiring the swap's delivery obligation writes the same answer onto
the recorded handoff entry, so `af sessions get` reports it afterwards.
`af sessions retry-limit` reports the same object for its own resume — the
swap's carry, a re-spawn that resumed the recorded conversation, or a fresh
start it names. For a session running in a sandbox the answer comes from the
sandbox's own recovery: `carried` only when the in-place relaunch it actually
performed reported resuming this session's own conversation — the relaunch
returns the verdict with its result, so the answer always matches the branch
that ran. Every other case answers `carry_failure` with the true reason: a
re-provision (including `--force-reap`), a fresh start the relaunch reported,
an older sandbox build that sent no report, or a report that did not bind to
this session and its conversation.

If an agent or account handoff starts its replacement but cannot confirm whether
the mission was submitted, af suppresses automatic redelivery because the first
submission may already have landed. Inspect the replacement pane, then choose
the explicit override when it is needed: if the agent is already acting on the
mission, mark it delivered (no resend) — **Mark delivered** in the TUI's
**`c`** picker, **Mark delivered** in the web pane header, or
`af sessions retry-limit <title> --delivered`; otherwise resend it — **Retry
send** in the **`c`** picker, **Retry handoff** in the web pane header, or run
`af sessions retry-limit <title>`. When the unconfirmed handoff is also parked
at its usage-limit wall with an unknown startup state, the override is neither
a resend nor a restart — the daemon queues the mission into the session's
stored prompt for the next limit resume, and the surfaces name it **Queue for
limit resume** (TUI `c` picker and web pane header). The retry verdict is
attached to that pending handoff mission; sending another prompt does not
make the daemon resend it.
If that explicit retry delivers the mission but cannot persist its final
settlement, every client reports the retry as completed with a warning. The TUI
shows the warning as a completion message, the web keeps it in the confirmed
mutation notice, and CLI JSON includes a `warning` field; do not retry the
already-delivered mission.

This is an operator-chosen account swap: the target must be registered for the
incoming agent and have no current limit observation in the daemon's ledger.
An unregistered or currently limited target is refused before stopping the old
runtime. No `limit_auto_resume` or `limit_account_candidates` setting is needed.
An explicit pin moves to the chosen account and remains pinned against automatic
rotation. The same local account-swap path replaces all credential-bearing panes;
its launch checks and restrictions, including VS Code tabs, still apply.

In the TUI, press **F** on the `[limit]` session and choose another account.
The project default is preselected when it is another registered, logged-in
account. In the web, use **Handoff** beside **Retry**, then choose the agent and account. Both
pickers list accounts belonging to the selected agent.

## Shadow-mode automatic handoff

`[account_handoff] mode = "shadow"` turns on the automatic-handoff engine in
**observe-only** mode ([epic #6776](https://github.com/sachiniyer/agent-factory/issues/6776)):
every ~45 seconds the daemon assembles each session's facts — liveness, wall
banner, working/idle, composer draft, account evidence — runs the same
decision the live mover will run, and appends the verdict to an
edge-triggered JSONL log at
`~/.agent-factory/instances/auto-handoff-events-YYYYMMDD.jsonl`. It never
moves a session. Shadow exists so the decision engine can be audited against
reality — and against an external watcher you may already run — before it is
allowed to touch anything.

```toml
[account_handoff]
mode = "shadow"
```

- **Edge-triggered.** A session emits rows when its verdict *changes* — wall
  seen, candidate chosen, draft blocking, pinned-ineligible — plus at most one
  unchanged-verdict heartbeat every 30 minutes. Rows are JSON with
  `ts`, `kind`, `session_key`, `agent`, `title`, `from`, `to`, `reason_code`,
  `params`, `mode`; files rotate daily and prune after seven days.
- **Same evidence as live.** Shadow reads the daemon's existing status
  snapshot — no extra pane reads — the durable wall ledger, the bounded usage
  indexer, and the registered-account drift check. A candidate whose
  `config.toml` runtime settings (`model`, `model_reasoning_effort`,
  `approval_policy`, `sandbox_mode`) differ from the session's home is
  reported `config-drift` and skipped; one af cannot verify is skipped too.
- **`require_carry = true` refuses a move that would start fresh.** Each
  tick the engine asks whether the session's conversation would carry. It
  asks in two steps, and the row tells you which step refused:
  - **The session's own side refused.** Examples: no recorded conversation,
    a transcript missing from the current account, a newer conversation in a
    worktree af owns, or a newer conversation in a directory af could not
    resolve within the bound (an unavailable or slow mount), which counts
    the same way. The session is deferred with an `ignored` row whose
    `reason_code` is `carry-refused`. `params.carry` holds the refusal
    code, such as `missing`, `stale` or `forked`. `to` is empty, because no
    target was at fault.
  - **The chosen target account's home refused.** Examples: it already holds
    a different copy of the conversation, or holds a thread/revert rollout
    of it. That account is skipped, as a drifted one is: the row's
    `skipped` param lists it as `<account>=carry:<code>`, and the next
    candidate is asked on the same tick. If every candidate refuses, the
    no-account (or idle no-target) row lists each one.
  - **af cannot ask about the target at all.** The session is deferred with
    a `carry-refused` row whose `to` names that target.

  Rows carry only the code, never a path or the refusal's prose.
- **A composer draft blocks every move**, and a pane whose composer cannot be
  located counts as draft-present — a move must never eat half-typed input.
- **Pinned sessions stay pinned** unless `include_pinned = true`; an automatic
  move under that mode *consumes* the pin (the session lands auto-selected on
  the target) rather than carrying it. `exclude_sessions` keeps named sessions
  out regardless.
- **`default_login = "move"`** extends governance to sessions running on the
  agent's ambient login (no `--account`). A working ambient session needs a
  qualifying usage reading to move; a provably idle one may leave on the
  banner alone.
- **`report_to` prompts another session** with the structured events:
  `<project>/<title>` or a unique bare title resolves one live session —
  ambiguous or absent matches are skipped and logged, never guessed. A
  persisted session that failed to restore still claims its title, so a
  stalled row counts toward ambiguity rather than redirecting the report
  to a surviving same-titled session in another project. Reports
  use a fixed template of reason codes and parameters (never pane text) and
  batch into a digest at most once a minute — every row batches; shadow
  mode has no immediate-delivery path. Delivery never delays a tick. By
  design the digest drops rows that name a resolved recipient — a session
  never reads reports about itself (that loop is how report ping-pong
  starts), so its own wall events reach it only through the event log. A
  recipient af cannot safely prompt — attached, or its composer won't
  prove empty — holds its backlog instead of being pasted over; retries
  back off exponentially, and a hold longer than ten minutes warns once
  in the log and on `af quota`'s `mover:` line.
- **Opt-in discovery is event-driven, not polled.** Whether the engine
  observes at all is decided by discovery, which runs at daemon start and on
  every global config save — `af config set` or `unset` without `--project`,
  or a save in the TUI or web config editor. Nothing else triggers it: not a
  timer, not a session create, not an `af config set --project` write, and
  no file watcher — af watches neither config file. It checks the global
  table and the resolved config of each project that has a session.
  `[account_handoff]` is rejected in in-repo config, so those are the only
  places an opt-in can come from.
- **An observing engine reads edits on its next pass.** Each pass re-resolves
  every session's config, reading the personal project file from disk and
  the global layer from the config the daemon last applied. When an
  `[account_handoff]` edit takes effect therefore depends on how it arrives
  and on whether the engine is already observing:

  | Edit arrives as | Engine not observing yet (a first `shadow` opt-in) | Engine already observing |
  | --- | --- | --- |
  | A global save (`af config set`/`unset`, TUI or web config editor) | At once — the save reruns discovery | At once — the next pass uses the new values |
  | `af config set`/`unset --project` | At the next global save or daemon restart, once the project has a session | On the next pass |
  | Hand edit of a personal project file | At the next global save or daemon restart, once the project has a session | On the next pass |
  | Hand edit of `~/.agent-factory/config.toml` | At the next global save or daemon restart | At the next global save or daemon restart |

  "On the next pass" covers every leaf: thresholds, `exclude_sessions`,
  `report_to`, and switching a project back to `off`. With no daemon
  running, every edit waits for the next daemon start. Each save prints the
  matching sentence, and `af accounts handoff status` shows whether the
  engine is observing.
- **Turning the last opt-in off outside a global save leaves status saying
  `engine: observing`.** After a `--project` write or a hand edit sets the
  last opted-in project to `off`, the engine stops governing its sessions on
  the next pass. But only discovery turns the engine itself off, so `af
  accounts handoff status` keeps printing `engine: observing` until the next
  global save or daemon restart. No session is governed in the meantime, and
  no rows reach the event log.
- **`probe_ttl` currently has no effect.** There is no live usage probe in
  this version: wall confirmation uses the banner plus the account store's
  own `rate_limits` records. The leaf is staged configuration for a later
  slice — writing it is harmless, it just changes nothing yet.
- **`mode = "live"` is rejected at load.** Live moves, verification, and
  recovery land in later slices; until then `shadow` and `off` are the only
  accepted spellings.

### Inspecting and resetting the rails

The engine keeps its durable record across two files under
`~/.agent-factory/instances/`, both keyed by stable session identity, never
by title:

- `auto-handoff-state.json` — the file older af versions already read and
  write: carried-prefix watermarks and the mover halt.
- `auto-handoff-rails.json` — the records the mover keeps for itself:
  refusal backoffs, the recent-move ledger the switch-loop rail
  (`max_moves` inside `max_moves_window`) and the anti-pile-on move credits
  count, held-report markers, the unrecorded-carry quarantine, and the
  account-level flags a full quarantine records. Keeping
  these out of the shared file means a downgrade's carry rewrite cannot
  erase them.

Both files preserve fields a later version adds, refuse symlinked paths,
and fail closed on corruption: a corrupt file is an error on every surface,
never an empty report. Two operator verbs read and repair the records:

```
af accounts handoff status
af accounts handoff reset <halt|backoff|moves> [--session <key>]
```

- **status** prints the resolved `account_handoff.mode` (the absent table is
  `off`, exactly the pre-feature behavior; a personal-project opt-in reports
  the mode the engine actually resolved), whether the daemon's engine is
  observing, the recorded halt with its reason and timestamp, live refusal
  backoffs — expired ones stay in the file for the next prune but never
  print — and the recent-move entries alongside the `max_moves`/`window`
  they are counted against. Either file corrupt is an error, not an empty
  report — the rails fail closed.
- **reset clears only the record you name.** `halt` clears the mover halt;
  `backoff` and `moves` clear refusal backoffs and recent-move entries,
  all of them or one session's with `--session`. Clearing a halt never
  silently takes the loop history or a held report with it — each rail
  protects the others from an impatient reset, and a reset that matched
  nothing says so rather than claiming a clearance it did not perform.
- **A halt stops only automatic moves.** `on_failure = "halt"` (the default)
  is for the live-mover slice; nothing moves today. The recorded halt is
  still honored on the surfaces: it appears on `af accounts handoff status`,
  on `af quota`'s `mover:` line, and on the TUI and web Usage sections, so a
  stopped mover is never silent. Ordinary wait-for-reset resume —
  including a `limit_account_candidates` auto-swap — is *not* gated by the
  halt record: it is a different mechanism with its own rails.
- **The verbs are daemon calls on every transport.** Local runs use the
  control socket; `--daemon-url`/`AF_DAEMON_URL` targets route over the
  authenticated HTTP API (`POST /v1/HandoffStatus`, `POST /v1/HandoffReset`)
  to the daemon the flag names — never to the local daemon's home by
  accident. Neither route is reachable with a sandbox credential: the rails
  name the operator's sessions and accounts with no per-session owner to
  narrow to, and reset is operator authority over the mover itself.

## Hand off to another agent

Waiting is not the only option. If the work should not sit until the window
resets — a day, sometimes several — hand the session to a **different agent**:

```
af sessions handoff fix-auth --to claude
```

In the TUI, press **`F`** on the selected session, pick the agent, and confirm.
The key is advertised on the status bar for a limit-blocked session, next to the
`c` retry, because the two are the two answers to the same wall: `c` waits for
*this* agent's window, `F` continues under another one.

What a handoff does, and does not do:

- **The session is the same session.** Same worktree, same branch, same tabs,
  same task binding, same name. Only the agent process is replaced. Nothing is
  archived, nothing is re-cloned, and uncommitted work is untouched — it is
  simply still there, because the worktree never moved.
- **A different agent starts fresh, with a brief.** Agent conversations are not
  portable between providers: claude cannot read codex's transcript and vice
  versa. So instead of a transcript, the incoming agent is told the session's
  goal, that it is continuing someone else's work, and where to look
  (`git log`/`git diff` on the branch). It is explicitly told not to start over.
- **The swap is recorded.** af notes which agent handed off to which, and the
  branch tip at that moment. That tip is the attribution boundary: everything up
  to it is the outgoing agent's work, everything after is the incoming agent's.
  af cannot label the commits themselves — your agent writes them — so the
  recorded tip is what lets a reviewer check the split rather than take it on
  trust.

Use `--brief` when the stored goal is stale or too broad, which is common on a
long-running session:

```
af sessions handoff fix-auth --to gemini --brief "just finish the retry test; leave the docs alone"
```

Any supported agent can be a target. Two things worth knowing rather than being
blocked on:

- **Approval policy belongs to the target agent.** A handoff starts the target's
  resolved command and configuration; it does not carry approval settings from
  the outgoing agent. See [Agent approval
  behavior](configuration.md#agent-approval-behavior).
- **Off-box handoff depends on the sandbox's agent-server.** An ssh or sandbox
  session whose agent-server advertises `agent-swap` can be handed off in
  place — the sandbox swaps the agent in the same workspace and resolves the
  new agent's program on its own host. An already account-scoped listed-host
  ssh/sandbox session whose agent-server advertises `account-swap` can be handed
  off in place for a same-agent `--account` swap (rotating its existing scope); a
  cross-agent `--to` on an account-scoped sandbox session, and any handoff on
  docker or hook sessions, cannot be handed off yet.

Handing off is **reversible**. Each agent's conversation history is stored per
directory, so the outgoing agent's thread is still in the worktree — hand back
to it once its limit resets, starting fresh with a brief. A scope drop
is one-way, so name the dropped account again with `--account` on the way back —
without it the returned agent runs ambient. Returning to the account re-pins
its credentials, but the return is still a cross-agent handoff, so the
incoming agent starts a fresh conversation rather than resuming the dropped
thread (see [Account-scoped handoff](#account-scoped-handoff)).

There is no automatic handoff. A swap changes which agent is editing your
branch, so it is always something you ask for.

## Task runs: park, don't fail

A **task** (cron or watch) can fire while your plan is already exhausted. When a
task-driven session hits a usage-limit wall as it starts up, or a task targets a
session already marked `[limit]`, Agent Factory **parks** the run instead of
typing into the limited pane or reporting a false success:

- The session is **kept**, not torn down, and marked `[limit]` (with its reset
  time) so the badge, the manual `c` retry, and auto-resume all apply to it.
- The task's run status is recorded as **`parked: usage limit`** — *not* an
  errored/failed run. It shows in the task manager as waiting for the limit
  window, and no failure side-effects fire.
- For a newly created task session, once the window resets the **same resume
  machinery** takes over: auto-resume (if `limit_auto_resume` is on) or your
  manual `c` retry re-delivers the session's stored task prompt, and the run
  proceeds to completion. A parked run becomes a completed one — never a failed
  one.

For a task with `target_session`, trigger kind determines what parking retains.
A cron occurrence is skipped and recorded `parked: usage limit`; the first cron
fire after reset runs normally, without replaying a burst of identical prompts.
A watch event is distinct data, so it enters the durable FIFO and replays after
the target leaves `[limit]`. Limit-held watch backlogs are not expired at the
ordinary 72-hour outage boundary. Once the bounded queue reaches 500 events or
256KB, Agent Factory stops reading the watch command's stdout and lets pipe
backpressure hold the producer until replay makes room, rather than dropping an
event or growing managed queue storage without limit.

This automatic park is deliberately task-only. A manual `af sessions
send-prompt` still delivers to a `[limit]` session, because typing may be needed
to answer a credits or limit **picker**. Do not type into a timed auto-continue
banner: Claude explicitly treats any typing as cancellation. In a Codex numbered
picker, use the arrow keys to select the intended choice before sending Enter;
digits are ignored and Enter accepts whichever choice is already highlighted.

Before this behavior, such a run spun a readiness timeout and was recorded as a
failure even though nothing was actually wrong — you'd just hit your plan limit.

## Scope summary

| Agent | Detected | `[limit]` badge | Manual `c` retry | Auto-resume | Task park | Handoff target |
|-------|:--------:|:---------------:|:----------------:|:-----------:|:---------:|:--------------:|
| `claude` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| `codex` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| `gemini` | — | — | — | — | — | ✅ |
| `aider` | — | — | — | — | — | ✅ |
| `amp` | — | — | — | — | — | ✅ |
| `opencode` | — | — | — | — | — | ✅ |
| `devin` | ✅ | ✅ | ✅ | fallback | ✅ | ✅ |

Auto-resume is **timed** for `claude`/`codex` because their banners carry a
parseable reset window. `devin` is detected but carries no reset window, so it has
no timed auto-resume — only the `limit_retry_interval` **fallback** cadence when
`limit_auto_resume` is on (the same path a `claude`/`codex` banner with an
unparseable reset time takes). The remaining agents are not detected: they expose
no plan-reset banner, or are API-key-metered (transient 429s the CLI retries) with
no plan window to schedule against.

The last column runs the other way, and deliberately so. Detection answers "can
af tell this agent hit a wall", which `claude`/`codex`/`devin` support — so only
they can be handed *from* on a limit. Being handed *to* needs nothing from the
agent at all: af stops one process and starts another in the same worktree, so
every supported agent is a valid destination.

## Custom detection patterns

If an agent reworded its banner, override the detection regex per agent with
`limit_patterns` (the built-in reset-time parser is kept):

```toml
[limit_patterns]
claude = "Claude usage limit reached\\."
codex  = "You['’]ve hit your usage limit"
```

Keys must be a supported agent (`claude`, `codex`, `aider`, `gemini`, `amp`,
`opencode`, `devin`); an override for an agent with no built-in matcher
(`aider`/`gemini`/`amp`/`opencode` today) is ignored, and an uncompilable regex
warns and falls back to the built-in default. A `devin` override replaces its
inferred detect pattern (still detect-only — there is no reset parser to keep).
See [configuration.md](configuration.md#custom-usage-limit-detection-limit_patterns).
