# Lifecycle testing — clean install and install → upgrade

Every other gate in this repo tests **code in isolation**. `make test-container`,
the `*-roundtrip-container` harnesses, `make tui-driver-selftest` — each builds
the current tree and exercises it. None of them ever installs a release,
upgrades it, and checks what the machine looks like afterwards.

That is where the bugs users actually hit live. They are not logic bugs; they
are **lifecycle** bugs, and they need two versions and a real machine to exist
at all:

| | what escaped |
|---|---|
| [#1921](https://github.com/sachiniyer/agent-factory/issues/1921) | a **new client** against an **old daemon** hard-failed (`unknown field "tab_id"`). The daemon is upgraded independently of its clients, so this is only reachable across a version boundary. |
| [#1916](https://github.com/sachiniyer/agent-factory/issues/1916) | `af reset` SIGKILLed a wedged daemon; the service manager relaunched it mid-wipe. Found by a **user**. |
| [#796](https://github.com/sachiniyer/agent-factory/issues/796) | the daemon **demoted** from supervised (a systemd unit) to an ad-hoc child: a daemon still runs and still answers, while the unit sits inactive/dead. |

A dev box is the *opposite* of the environment those need: it has an installed
unit, a live daemon, and months of state. "Works here" says nothing about a new
machine, which has **no config, no unit, no home, no daemon**.

`scripts/lifecycle.sh` builds that machine from nothing, then upgrades it.

## Running it

```bash
# The whole gate, container-fenced. The only way to run it on a dev box.
make lifecycle-container

# Narrow it while iterating.
make lifecycle-container LIFECYCLE_SCENARIO=scenario-a
make lifecycle-container LIFECYCLE_SCENARIO=scenario-b
```

It needs **network**: it downloads real published release tarballs. That is the
point — the whole gate is about the boundary between two real releases — but it
also means this is the one testbox target that is not hermetic, which is why it
is wired **nightly** rather than per-PR (`.github/workflows/lifecycle.yml`). It
also runs on a PR that touches the gate's own files, so a change to the harness
proves itself.

## The scenarios

### A — clean first run

The machine a new user has: no `config.toml`, no AF home, no autostart unit, no
daemon. Nothing in af may assume any of them exist. It asserts the preconditions
first (so a "clean run" that wasn't clean cannot pass quietly), then: `af
version` works, `af doctor` reports **zero FAIL** a new user cannot act on
(missing agents are WARNs with a fix line — correct), the **TUI renders its
first frame** on a virgin home without panicking, and the daemon comes up
**exactly once**.

### B — install → upgrade

Install a **real previous release**, put sessions on it, upgrade the way a user
actually does, assert the machine is coherent. Both upgrade paths are covered,
because they are different code paths into the same restart:

* `af upgrade`
* the **launch-time auto-update** (`commands/root.go` → `autoUpdateOnLaunch`),
  which is gated on stdout being a TTY and so can only be driven through a real
  terminal — a tmux pane, never a plain exec.

| # | assertion |
|---|---|
| 1 | the daemon is **restarted onto the new binary** — not left running the old one |
| 2 | **client version == daemon version**, queried rather than inferred |
| 3 | exactly **one** daemon afterwards; no orphan survives |
| 4 | if a unit was installed, it is **still the supervisor** — not demoted to an ad-hoc child, and the unit is not left inactive/dead |
| 5 | pre-existing **sessions survive** (count before == after; none Lost) |
| 6 | `af doctor` reports **zero FAILs** afterwards |

The release pair is resolved from the API at run time (the two newest
non-prereleases), not pinned: nothing here asserts a specific version, so the
gate keeps testing the current boundary as releases cut.

## How assertion 2 queries the daemon

There is no "what version are you?" RPC on the daemon today — that is
[#1920](https://github.com/sachiniyer/agent-factory/pull/1920). So the daemon's
version is read from the image it is **actually executing**: copy
`/proc/<pid>/exe` and ask it. This is a real query, not an inference from the
binary on disk, and that distinction is the whole bug — after an upgrade the
on-disk binary is N while a daemon that was never restarted is still executing
the N-1 image. `/proc/<pid>/exe` still resolves to those bytes even though the
path now reads `(deleted)`.

`lc_doctor_fail_count` **feature-detects** `af doctor --json`: it parses the text
summary today, and switches to the structured summary the moment #1920 lands. No
follow-up edit needed here.

## Isolation

The harness is destructive by design — it installs binaries, registers autostart
units, upgrades af over itself, and stops daemons. It refuses to run unless
**both** are true:

1. `AF_LIFECYCLE_DISPOSABLE=1` is set explicitly, and
2. the environment *positively* looks disposable — `/.dockerenv` (container) or
   `CI=true`.

A shared dev box has neither, so it refuses there **even with the opt-in set**.
The workspace is validated too: it can never be, or sit inside, the real AF
home. Every daemon it signals is proven to serve its own throwaway home by
reading `/proc/<pid>/environ` — so it cannot touch a real daemon, or another
user's.

## Proving the gate still bites

Most of the bugs above are fixed, so a green run proves little on its own — the
value is the ones it catches **next**. `AF_LIFECYCLE_INJECT` deliberately breaks
the machine so the assertions can be watched failing:

```bash
AF_LIFECYCLE_INJECT=skip-daemon-restart make lifecycle-container LIFECYCLE_SCENARIO=scenario-b
```

| injection | reconstructs | must fail |
|---|---|---|
| `skip-daemon-restart` | the #1921 machine: binary swapped to N, daemon never restarted — new client, old daemon, everything apparently "running" | assertions 1 and 2 |
| `unhealthy-session` | a session left in a non-healthy state across the upgrade (`Archived(6)/LiveArchived(5)`) | assertion 5 |

Re-run these whenever you change the harness. **An assertion that cannot be
watched failing is not evidence** — and this gate has caught itself twice on
exactly that:

* the Lost check tested `status/liveness == 4` (`Dead`/`LiveDead`), both
  **write-never** since #1108 — deaths record `Lost`, and persisted `Dead` is
  rewritten to `Lost` on load. It could not match anything on any machine, and
  reported "0 sessions Lost: PASS" forever. It is now **fail-closed**: it asks
  "is every session in a state I recognise as healthy?", so an unknown or
  drifted enum value fails LOUDLY instead of quietly matching nothing;
* an unscoped `sessions list` counted the wrong project's sessions and passed
  locally by accident (see the `--repo` note above).

Why `unhealthy-session` archives rather than killing a pane: **it can't be Lost
on demand.** Killing a session's tmux does not strand it — the daemon restores it
within ~4s (measured: Running→Ready, and it heals even with the worktree
deleted, #1108's restore loop). So an injection built on that would quietly
inject nothing, which is the same vacuity in a different hat. Archive is durable
and lands on a real non-healthy state.

## Where assertion #4 (supervision) actually runs

This is the assertion the gate most needs — an upgrade demoting the daemon off
its unit (#796) is **invisible to every other check here**, because the demoted
daemon keeps running and keeps answering. So it gets its own rules.

It runs on the **CI native leg**, against a real systemd user manager, and it
passes there today:

```
PASS  assertion 4: the unit is still active (= active)
PASS  assertion 4: af still sees the autostart unit (= true)
PASS  assertion 4: the running daemon IS the unit's child (MainPID=7712) — not demoted
```

That last line is the one that catches the demotion: it compares *the daemon
that is running* against *the daemon systemd owns*. A demoted daemon still
answers pings; it just is not the unit's child any more.

It **cannot** run in the test container, and that is not a policy choice:

| approach | verdict |
|---|---|
| `systemd --user` standalone in the container | **impossible** — it refuses without PID 1 systemd: *"Trying to run as user instance, but the system has not been booted with systemd."* |
| full systemd as PID 1 in the container | works, but needs `--privileged` + host cgroups — which dissolves the very fence the container exists to provide on a shared box. Rejected as a default. |
| assert it at the af layer instead | does not help: with no service manager, `af daemon install` fails outright, so there is no unit to be the supervisor of. The layer was never the problem. |
| the CI runner | **works** — it is itself a clean, ephemeral machine with a real systemd. This is where it runs. |

Two guards keep that honest:

* a **SKIP is not a pass**: any skipped check exits the run non-zero unless
  `AF_LIFECYCLE_ALLOW_PARTIAL=1` is set. `make lifecycle-container` sets it (a
  dev-box container genuinely cannot host a service manager) and prints a
  PARTIAL COVERAGE banner naming what went untested;
* the CI native leg **fails the job** if assertion #4 ever silently starts
  skipping — a green run that quietly stopped testing supervision is the exact
  lie this gate exists to prevent.

## What this does NOT cover yet

* **macOS / launchd.** `lc_unit_active` and `lc_unit_main_pid` already have
  Darwin branches, so assertion #4 asserts the *same property* against launchd
  (`state = running` and `pid = N` in the `gui/<uid>` domain af restarts) rather
  than a re-invented one. The blocker for the `macos-latest` leg is **assertion
  2**: `lc_daemon_version` reads `/proc/<pid>/exe`, which macOS has no
  equivalent of — `ps` would report the path, whose bytes are the NEW binary
  after an upgrade, i.e. exactly the inference this assertion refuses to make.
  The macOS leg therefore wants **#1920**'s daemon-reported version. Given
  #1931 turned on macOS CI and immediately found three real darwin defects
  (#1939, #1940, #1941), this leg is worth landing soon.
* **#1916's reset-vs-relaunch race** — this scenario upgrades, it does not
  reset.
* **Downgrades and channel switches** (`--allow-downgrade`, preview → stable).
* **Package-manager installs** (Homebrew, `install.sh`); the gate installs the
  release tarball the way `af upgrade` does.
