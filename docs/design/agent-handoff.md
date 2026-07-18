# Design: Agent handoff on a usage limit (#2013)

Status: **Proposed — three decisions await the owner (§2, §3, §4)** · Author: Captain Claude · Issue: [#2013](https://github.com/sachiniyer/agent-factory/issues/2013) · Builds on: [#1146](https://github.com/sachiniyer/agent-factory/issues/1146)

> Nothing here is implemented. This document exists to get D1/D2/D3 answered
> before code is written, because each one is expensive to reverse.

## 0. Summary

When a session's agent hits its plan usage limit, af today **waits** (#1146).
#2013 asks for the other branch of that fork: **switch agents and keep going**.

The headline finding of this design pass is that handoff is far less new
machinery than the issue assumes, and far more of a *policy* problem than a
*mechanism* problem:

- The **executor already exists**. `LocalBackend.Respawn`
  (`session/backend_local.go:427`) is guard-free by design — it was made so for
  the #1146 limit-retry path — and recomputes the whole program from the
  persisted `Instance.Program` on every attempt. Writing a different agent name
  into `Instance.Program` and calling `Respawn` swaps the agent in place, with
  correct override resolution and system-prompt injection, for free.
- The **detector already exists** (`task/limit.go:71`), the **park state**
  already exists (`LiveLimitReached`, `session/liveness.go:54`), and the
  **scheduler that acts on it** already exists (`daemon/limitresume.go:67`).
- What does *not* exist, and what this design is mostly about: a per-agent
  notion of "is this agent limited right now" (§8), a durable record of who
  wrote what (§6), and a defensible answer to "when is it safe to fire" (§2).

Recommendations in one line each:

| # | Decision | Recommendation |
|---|---|---|
| **D1** | Trigger (§2) | **Prompted first, automatic second and separately gated.** Auto-handoff on today's detector is not safe. |
| **D2** | State transfer (§3) | **Mission + worktree, never transcript.** Re-issue the stored prompt to the new agent with an explicit continuation brief. |
| **D3** | Session identity (§4) | **Swap in place.** A same-branch successor is *impossible* without destroying the original — verified, §4.2. |
| — | Agent matrix (§5) | Trigger on claude/codex only; target any configured agent; never target a known-limited one. |
| — | Attribution (§6) | Append-only handoff ledger on the tab, anchored to the HEAD SHA at swap time. |
| — | Surface (§7) | CLI verb is the primitive; TUI key + web action required by the parity gate. |

---

## 1. What already exists

Handoff should add one verb and one policy layer, not a subsystem. The
inventory:

| Capability | Where | Reusable for handoff? |
|---|---|---|
| Usage-limit detection | `task/limit.go:71` `builtinLimitMatchers` | Yes — but see §2.1 |
| Reset-time parsing | `task/limit.go:140` `Check() (hit, resetAt, hasResetTime)` | Yes, three-valued already |
| Park state | `LiveLimitReached` = 6, `session/liveness.go:54` | Yes |
| Park→act scheduler | `daemon/limitresume.go:67` `ResumeLimitedSessions` | Yes — handoff is a sibling action |
| Resume executor | `daemon/limit.go:285` `resumeFromLimitLocked` | Yes — same shape, different program |
| **Program swap** | `session/backend_local.go:427` `Respawn` + `tmux.SetProgram` | **Yes — this is the executor** |
| Prompt (re)delivery | `task/start.go:29` `StartAndSendPrompt` | Yes |
| Stored mission | `Instance.Prompt`, persisted `session/storage.go:77` | Yes — this is the anchor (§3) |
| Agent resolution | `Instance.ResolvedAgent()` `session/instance_accessors.go:201` | Yes — **mandatory**, see §5.3 |
| Per-agent limit state | — | **Missing** (§8) |
| Durable per-session history | — | **Missing** (§6) |

`Instance.Program` holds the bare enum name (`"claude"`), resolved at spawn by
`resolveProgramForInstance` (`session/backend_local.go:28`). It is written in
exactly two places, both pre-start (`session/instance_factory.go:222`,
`app/handle_input.go:92`), and **no RPC, CLI verb, or API route mutates it**.
That missing write path is the whole of the new mechanism.

---

## 2. D1 — Trigger  ⚠️ LOAD-BEARING

**Options:** (a) automatic on `LimitReached`, (b) prompted — offer the user an
action, (c) both.

**Recommendation: (c), but strictly staged — ship (b) first, and gate (a)
behind its own config key with a tightened detection predicate.**

### 2.1 Why automatic-on-today's-detector is not safe

Detection is an unanchored regex over captured tmux pane text:

```go
// task/limit.go:56,65
claudeLimitDetect = regexp.MustCompile(`Claude usage limit reached\.`)
codexLimitDetect  = regexp.MustCompile(`You've hit your usage limit`)
```

Any pane *containing* that text matches — including a pane that is merely
**displaying** it. This is not hypothetical. Measured against the current tree:

```
docs/usage-limits.md:  codex-detect=true
docs/configuration.md: codex-detect=true
```

Both files document the patterns, so both contain the literal codex banner
string. A codex session with either file on screen — `cat`, a pager, a diff, an
editor — satisfies the detector.

Under #1146 this false positive is nearly free: the session shows `[limit]` and
a resume is attempted. Under handoff the *same* false positive **switches which
agent is editing the user's code**. Same signal, categorically larger blast
radius. And the failure is self-referential: the session most likely to trip it
is one working on af's own usage-limit code.

It is a probe that cannot distinguish "the agent is stalled at this banner" from
"the agent is looking at this banner", yet answers anyway — and here the fake
answer authorizes an action on the user's branch.

Mitigations, in ascending order of cost:

1. **Idle-gated (already true).** Detection runs only in `resolveIdleLiveness`
   (`daemon/limit.go:77`), so a working agent is never sampled. Necessary, not
   sufficient — an agent that just printed the doc and returned to prompt is idle.
2. **Tail-anchored.** Require the banner in the last N lines of the capture: a
   stalled agent's banner *is* the last output. Cheap, and kills the
   documentation case outright. **Recommended as part of the auto path.**
3. **Stability-gated.** Require the banner to persist across ≥2 consecutive
   polls with unchanged pane content. Cheap, composes with (2).
4. **Confirmed by the agent-server.** The real fix, and the same shape as
   [#2070](https://github.com/sachiniyer/agent-factory/issues/2070) (submit
   should be *reported*, not inferred from pixels). Out of scope here; worth
   noting that both issues want the same thing.

### 2.2 Recommended staging

- **Phase 1 — prompted.** A limit-blocked session gains a handoff action
  alongside the existing `c` retry. The user chooses; no predicate risk. This is
  also the primitive that the auto path calls, so it is not throwaway work.
- **Phase 2 — automatic.** New key `limit_action = "wait" | "handoff"`
  (default `"wait"` = today's behavior), *plus* mitigations (2)+(3). Auto-handoff
  additionally refuses when the session has no stored prompt (§3.3).

Keeping `limit_action` separate from `limit_auto_resume` matters: they answer
different questions (*what* to do vs *whether* to do it unattended), and
collapsing them would make "wait, but only when I'm watching" unexpressible.

---

## 3. D2 — State transfer  ⚠️ LOAD-BEARING

**Options:** (a) branch/worktree + a summary, (b) replay the conversation,
(c) branch + handoff prompt.

**Recommendation: (a)+(c) — transfer the *mission* and the *worktree*. Never
attempt the transcript.**

### 3.1 Option (b) is not merely hard — the repo already rules it out

af has two tiers of conversation resume, and both are agent-private:

- **Tier 1, resume-latest** — all six agents, keyed to **cwd**
  (`session/tmux/resume.go:48`): `claude --continue`, `codex resume --last`,
  `aider --restore-chat-history`, `gemini --resume latest`,
  `amp threads continue --last`, `opencode --continue`.
- **Tier 2, resume-this-exact-conversation** — claude/codex/amp only
  (`session/tmux/resume.go:201`), which **explicitly refuses a cross-agent
  request**:

```go
// session/tmux/resume.go:208
if agentIdx < 0 || agent == "" || recordedAgent != agent {
    return program, false
}
```

There is no import path between providers: each stores its transcript in its own
format, in its own location, readable only by its own CLI. Option (b) is not a
budget question.

### 3.2 What actually transfers

Three things, and they cover more than "replay" would:

1. **The worktree** — every file the previous agent wrote, *including uncommitted
   work*. This is the substantive state, and under D3-swap it transfers by not
   moving at all.
2. **The mission** — `Instance.Prompt` (`session/instance.go:163`, persisted
   `session/storage.go:77`). For task-driven sessions this is the stored task
   prompt; #1146 already re-sends exactly this on resume.
3. **The visible history** — `git log` and `git diff` on the branch. The new
   agent reads these itself; af should point at them rather than summarize them.

"Continue" concretely means: same worktree, same branch, agent process replaced,
and this delivered as the first prompt:

```
You are continuing work already in progress in this worktree.

A previous agent (codex) was working on it and stopped because it hit its
provider usage limit. Its conversation is not available to you.

The original goal:
<Instance.Prompt>

Work already done on this branch (siyer/fix-auth):
  4 commits since master, plus uncommitted changes in the working tree.
  Review them before you start:  git log master..HEAD  ·  git diff master...HEAD

Continue from that state. Do not start over, and do not revert work you did
not write.
```

Note what af does *not* do: it does not summarize the previous agent's work.
Any summary af writes is af's inference about work it did not do, presented to
the new agent as fact — a fabricated intermediate. The diff is the ground truth
and the new agent can read it.

### 3.3 The honest limitation, and where it bites

#1146 already concedes that even a *same-agent* resume loses context when there
is no stored prompt — it sends a bare `"continue"` (`daemon/limit.go:393-398`,
`docs/usage-limits.md:53-57`).

Handoff makes this decisive rather than merely lossy. A *fresh* agent sent
`"continue"` on a strange worktree has nothing at all — no transcript, no
mission, no idea what "continue" refers to.

**Therefore: a session with no stored prompt has no sound automatic handoff.**
The recommendation:

| Session kind | `Instance.Prompt` | Automatic handoff | Prompted handoff |
|---|---|---|---|
| Task-driven (cron/watch) | stored task prompt | **yes** | yes |
| Created with `--prompt` | present | **yes** | yes |
| Bare interactive | empty | **no — refuse** | yes, user supplies the brief |

Refusing is the honest outcome, not a gap: the alternative is dispatching an
agent onto someone's branch with no instructions. In the prompted path the user
can supply the brief inline (`--brief`), which is strictly better information
than anything af could synthesize.

---

## 4. D3 — Session identity  ⚠️ LOAD-BEARING

**Options:** (A) swap the program in place, (B) successor session + archive the
original, (C) successor on the *same* branch.

**Recommendation: (A) swap in place.**

### 4.1 Sachin's manual pattern, and why it does not generalize

The observed manual workaround was: create a claude successor pointed at the old
session's branch and mission, then archive the original. That is strong evidence
the *workflow* is right. It is not evidence that *successor* is the right
product shape — it was the only move available without an in-product mechanism.

### 4.2 Option (C) is impossible — verified

Git refuses to check out a branch that another worktree already holds:

```
$ git worktree add ../wt2 feature
fatal: 'feature' is already used by worktree at '.../wt1'
```

And **archiving does not release it.** af's archive moves the worktree rather
than removing it (`teardownArchive` → `MoveWorktree`,
`session/git/worktree_archive.go:99`), so the branch stays checked out at the
archive location:

```
$ mv wt-live archived-wt && git worktree repair archived-wt
$ git worktree add ../wt-successor feature
fatal: 'feature' is already used by worktree at '.../archived-wt'
```

Only `kill` frees the branch, and kill deletes the worktree and prunes the branch
(`session/git/worktree_ops.go:521-536`). So option (C) requires **destroying the
original to hand off from it** — which forfeits both restorability and the
attribution the issue asks for. Rejected on evidence.

There is a further blocker: branch names are *derived from the session title*
(`git.NewGitWorktree`, `session/git/worktree.go:155`) and cannot be supplied. A
successor with a different title gets a different branch by construction.

### 4.3 Option (B) — successor on a forked branch — is viable but worse

Archive the original (which commits its WIP as `af: pre-archive snapshot`,
`session/git/worktree_push.go:11`), then create a successor branching from that
tip. This sidesteps §4.2 and gives per-agent attribution structurally: one
branch per agent.

Costs: the work is split across two branches and two sessions, so the PR story
fragments; the sequence is multi-step and non-atomic, and a failure between
steps strands the work; the task binding breaks mid-run for cron/watch sessions;
and the original agent's conversation is stranded in the archived worktree
(§4.4).

### 4.4 Why (A) wins

1. **It is the only option that keeps uncommitted work in place** without a
   commit-and-fork dance.
2. **The mechanism already exists and is already exercised** — `SetProgram` is
   documented as mutable-after-creation (`session/tmux/program.go:5-10`), and
   `Respawn` recomputes from `Instance.Program` on every attempt
   (`session/backend_local.go:435`).
3. **Handoff becomes reversible.** Because every agent's resume is keyed to the
   *cwd*, one worktree accumulates N private per-agent transcripts. Keep the
   worktree and codex's thread survives the handoff — when its limit resets,
   handing back re-enters codex's own conversation via
   `ResumeProgramWithConversationID`. A successor with a new worktree strands it.
4. **It preserves every association**: task binding, PR info, tabs, session id,
   branch. Nothing downstream has to learn that a session can have a predecessor.

Cost of (A), stated plainly: `Instance.Program` becomes time-varying, so "which
agent is this session?" is no longer a constant. That is exactly what §6's
ledger is for — and any consumer that needs the *live* answer must already call
`ResolvedAgent()`, not read the field (§5.3).

---

## 5. Agent matrix

### 5.1 Who can trigger a handoff

Only **claude** and **codex** — they are the only agents with limit detection
(`task/limit.go:71`), because they are the only plan-metered ones with a parseable
reset window. gemini/aider/amp/opencode are API-key-metered; a "limit" there is a
transient 429 the CLI already retries (`docs/usage-limits.md:12-25`). Unchanged
by this design.

### 5.2 Who can receive one

**Any configured agent**, subject to three gates:

1. **Explicit ordering, not "any available".** `handoff_order = ["claude", "gemini"]`,
   default empty (= feature off). Nondeterministic selection of who edits a
   user's code is precisely what the issue's opt-in requirement guards against.
2. **Never a known-limited agent** (§8).
3. **Never the outgoing agent itself.**

No pair is prohibited. Handing claude→codex and codex→claude are both fine; the
constraint is availability, not compatibility.

Two properties worth setting expectations on rather than blocking:

- **AutoYes support is uneven** — `autoYesUnsupported` (`session/backend_local.go:110`)
  lists codex, amp, opencode. Handing an autoyes session to one of them yields a
  session that will stop at confirmation prompts. Warn at handoff time; do not
  refuse.
- **`--here` sessions** attach to the repo's own working tree
  (`session/instance_factory.go:38`). Swap-in-place works fine for them; nothing
  special is needed. (Archive is what `--here` cannot do — another cost of D3-B.)

### 5.3 Mandatory: resolve through `ResolvedAgent()`

`program_overrides` can point an agent name at an arbitrary command, so the enum
an instance was created with and the program that actually runs diverge. The
codebase states the rule outright:

```go
// session/tmux/resume.go:285-290
// Every agent-specific spawn/restore behavior (flag injection, readiness
// heuristics, trust-prompt handling) must key off THIS — what will actually
// run — never off the config-name enum an instance was created with
```

Handoff adds two more agent-keyed behaviors (target selection, limit-registry
keying) and both must obey it. Keying off `Instance.Program` would pick a
fallback that isn't what runs.

Also: `SupportedPrograms` (`session/tmux/session.go:27`) is **positionally
load-bearing** — `app/handle_overlay.go:24` indexes it by overlay row. Any
agent-picker reuse must not reorder it.

---

## 6. Attribution

The issue is firm here: *"the PR must say so — otherwise a reviewer trusts a diff
with two authors' assumptions blended."* Agreed, and it is the requirement most
at risk of being satisfied with a cosmetic label.

**af cannot inject commit trailers.** The agent authors the commits; af creates
exactly one commit ever (the pre-archive WIP snapshot,
`session/git/worktree_push.go:11`) and writes no trailers anywhere. Any claim
that af "marks the commits" would be false.

**What af can do is make the boundary exact.** Record the HEAD SHA at the moment
of the swap. Then "codex wrote everything up to `abc1234`, claude wrote
everything after" is not a claim — it is a `git log` range a reviewer can verify.

Proposed shape, following the additive/rollforward precedent of `TaskID`
(`session/storage.go:19-28`) and `AgentConversationData`
(`session/conversation.go:15-18`):

```go
// Append-only. One entry per handoff.
type AgentHandoff struct {
    From       AgentConversationData `json:"from"`        // outgoing agent + its conversation id
    To         string                `json:"to"`          // incoming agent
    At         time.Time             `json:"at"`
    HeadSHA    string                `json:"head_sha"`    // the attribution boundary
    Reason     string                `json:"reason"`      // "usage limit" | "manual"
    Automatic  bool                  `json:"automatic"`
}
```

stored as `Tab.Handoffs []AgentHandoff`.

This one structure discharges three requirements at once:

- **Attribution** — exact commit ranges per agent, plus the automatic/manual
  distinction so a reviewer knows whether a human chose this.
- **Hand-back** — it preserves the outgoing `AgentConversationData`, which a
  swap would otherwise destroy. `Tab.Conversation` is a **single slot**
  (`session/conversation.go:71`): the incoming agent's capture overwrites the
  outgoing agent's id. Without this list, handing back to codex could only reach
  its Tier-1 latest-in-cwd behavior instead of its exact thread.
- **Loop detection** — the history makes "this session has bounced between two
  limited agents three times" directly answerable (§8).

Surfaces: a `[handoff]`-style marker in the sidebar and web (the `[limit]` badge
is the precedent, `ui/tree/render.go:97`), the full ledger in
`af sessions get --json`, and a `session.handoff` event in
`agentproto/message.go:87` for live clients. The event plane is **not** storage —
it is drained, not retained — so the persisted ledger is the record of truth.

---

## 7. UX surface

**The parity gate decides this, not taste.** `parity/inventory.json` is a
checked-in capability table and `parity_test.go` derives real surfaces from the
cobra tree, route catalog, TUI binding table, and web RPC call sites — **the
build fails when a surface grows a capability with no entry**. Adding handoff
forces a recorded answer for CLI, TUI, and web.

Recommended:

- **CLI — the primitive.** `af sessions handoff <title> --to <agent> [--brief <text>]`,
  in a new `api/sessions_handoff.go` (the 1000-line file lint makes appending to
  `api/sessions.go` the wrong move). Registration mirrors
  `api/sessions_sendprompt.go`.
- **Daemon — one RPC.** `HandoffSession` next to `SendPrompt`
  (`daemon/httproutes.go:103`, `daemon/control_server.go:464`), resolving the
  target via `Manager.resolveActionSession` (`daemon/manager_sessions.go:380`)
  and taking the existing per-session op-lock. The daemon stays sole writer
  (#960).
- **TUI — a key plus the existing picker.** The agent picker at session-create
  (`overlay.NewSelectionOverlay`, `app/handle_input.go:119-134`) is reusable
  verbatim; add `stateSelectHandoffAgent` beside `stateSelectProgram`. Confirm
  via `confirmAction` (the `handleArchive` template,
  `app/handle_actions.go:324`). New `KeyName` entries **append** — the block is
  iota-based (`keys/keys.go:118-122`).
- **Web — required, not optional.** The agent `<select>` in `newSessionModal`
  (`web/src/modals.ts:183`) is directly reusable. Note this is exactly the gap
  [#1934](https://github.com/sachiniyer/agent-factory/issues/1934) already
  reports: the web renders `Limit reached` with *no way out*. Shipping handoff
  TUI-only would reproduce that bug one level down. Remember `web/dist/` is
  committed — a web change is a two-part commit including `make web-build`.
- **Agent-facing docs.** `session/systemprompt.go:16` `afUsageReference` is the
  hand-written command list taught to every agent; a new verb must be added there
  or agents won't know it exists.

---

## 8. Loop guard, and the missing per-agent limit registry

The issue asks that af stop rather than round-robin when everything is limited.
Delivering that needs something af does not have.

**Limit state today is strictly per-session.** There is no agent-keyed limit
state anywhere — `limitResumeStates` is keyed by daemon instance key
(`daemon/manager.go:116`), and the detector's agent identity is discarded
immediately after `Check`, leaving only a bool and a timestamp on the instance.

But **a plan limit is a property of the account, not the session**. Two codex
sessions on one account hit the same wall; today they discover it independently,
park independently, and each schedules its own resume. For handoff this is not
merely redundant — it is wrong: the fallback picker would happily hand a session
to an agent that another session discovered was walled thirty seconds ago.

Proposal: a daemon-held `map[agent]limitFact{resetAt, observedAt, sourceSession}`,
populated by the existing detector. Small, and it improves #1146 independently
(a session need not rediscover a known-exhausted plan).

**Three-valued, per the repo's standing discipline** — and the third value is
load-bearing here:

| State | Meaning | Handoff treatment |
|---|---|---|
| `limited` | detected, reset time known or not | Skip. |
| `available` | ran recently without a limit banner | Eligible. |
| `unknown` | never observed, or **no detector for this agent** | **Eligible, but never reported as verified-available.** |

gemini/aider/amp/opencode are permanently `unknown` — af has no detector for
them (§5.1). The trap to avoid is rendering `unknown` as `available` and
claiming an agent is fine when af cannot know. Try it and observe; do not
assert it.

Consequently **"all agents limited" must require all to be *known* limited.**
`unknown` agents get tried first. Loop guard: stop after one full pass of
`handoff_order` with no non-limited candidate, report `all agents rate-limited`
as a real reported state, and leave the session parked so the #1146 wait path
still applies. The §6 ledger bounds ping-ponging.

---

## 9. Config

All keys global-only (`config/inrepo.go:107`) — they configure the daemon, and a
cloned repo must not be able to flip who edits your code. Adding each requires
the four coordinated edits enforced by `config/manifest.go` tests: field,
default, manifest entry, settable spec.

```toml
handoff_order = []            # ordered fallbacks; EMPTY = feature off (default)
limit_action  = "wait"        # "wait" (default, today) | "handoff"
handoff_brief = ""            # optional extra context appended to every brief
```

Default-off is non-negotiable per the issue. `handoff_order = []` as the off
switch means the feature cannot engage without the user naming who is allowed to
touch their branch — the opt-in and the policy are the same knob, which is one
fewer way to be half-configured.

---

## 10. Build plan

Serialized; each PR independently useful and independently revertable.

| PR | Scope | Gated on |
|---|---|---|
| 1 | Per-agent limit registry (§8), three-valued, daemon-held. Improves #1146 standalone. | — |
| 2 | `Instance.Program` write path + `AgentHandoff` ledger (§6) + `HandoffSession` RPC. No UI. | 1 |
| 3 | CLI `af sessions handoff` + parity entries + docs + `afUsageReference`. | 2 |
| 4 | TUI key + agent picker + confirm. **Play-test gate applies** (visible TUI change). | 3 |
| 5 | Web action (§7) — also closes the #1934 dead-end. `make web-build`. | 3 |
| 6 | Automatic trigger: `limit_action`, tail-anchoring + stability gate (§2.1), no-stored-prompt refusal (§3.3). | 4, 5 |

PR 6 is deliberately last and separately revertable: it is the only one that
acts without a human in the loop.

---

## 11. Open questions for the owner

1. **D1 (§2)** — Confirm prompted-first, with automatic as a later separately
   gated phase? The alternative is shipping auto in PR 1, which I recommend
   against on the false-positive evidence in §2.1.
2. **D2 (§3)** — Confirm mission-not-transcript, and specifically confirm that
   **a session with no stored prompt refuses automatic handoff** (§3.3) rather
   than sending a fresh agent a bare `continue`.
3. **D3 (§4)** — Confirm swap-in-place over successor+archive. This one
   contradicts the manual pattern, on the evidence in §4.2 that a same-branch
   successor cannot exist while the original does.
4. **Naming** — `af sessions handoff` and `limit_action` are new public
   surface (CLI noun, config key) and are cheap to change now, expensive later.
5. **Scope check** — is the per-agent limit registry (§8) welcome as PR 1? It is
   strictly required for a correct loop guard, and it independently improves
   #1146, but it is more than the issue literally asked for.
