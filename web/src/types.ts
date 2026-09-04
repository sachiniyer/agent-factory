// The wire DTOs the sidebar reads (#1592 Phase 5 PR3). These are a hand-mirror of
// the Go source of truth — NOT a fork of it: SessionData mirrors the subset of
// session.InstanceData (session/storage.go) the rail renders, and the Liveness /
// InFlightOp enums mirror session/liveness.go. Per design §3.2 the client reuses
// the daemon's projection shapes verbatim; it must not invent its own status
// logic. (FLAG §7.1: hand-mirror vs codegen — hand-mirror in v1, revisit if it
// drifts.)
//
// Liveness and InFlightOp are Go `int` types with no custom MarshalJSON, so they
// travel as bare integers on the wire; these const objects pin the exact numeric
// values from session/liveness.go's iota blocks. Adding a value there is a
// deliberate, breaking change here too — the same "the switch is TOTAL" discipline
// the TUI renderer keeps (ui/tree/render.go:274).

/** session.Liveness (session/liveness.go): the daemon-owned health axis. */
export const Liveness = {
  Unset: 0,
  Running: 1,
  Ready: 2,
  Lost: 3,
  Dead: 4,
  Archived: 5,
  LimitReached: 6,
} as const;

/** session.TabKind (session/tab.go): the kind of process a tab hosts. The agent
 *  tab is always index 0 and is unclosable; shell/process tabs are user-created. */
export const TabKind = {
  Agent: 0,
  Shell: 1,
  Process: 2,
  /** A URL/iframe tab (no PTY): rendered as an iframe, not an xterm. A loopback
   *  target is reverse-proxied by the daemon (/v1/webtab/...); an external URL is
   *  iframed directly. Mirrors session.TabKindWeb (session/tab.go). */
  Web: 3,
  /** A VS Code editor tab (no PTY, and no URL either): a daemon-managed
   *  per-session code-server rooted at the session's worktree, reachable only
   *  through the daemon proxy (/v1/webtab/...). Mirrors session.TabKindVSCode
   *  (session/tab.go) — the kind travels as a bare int, so this MUST stay in
   *  lockstep with the Go enum. */
  VSCode: 4,
} as const;

/** session.InFlightOp (session/liveness.go): the transient client-op axis. */
export const InFlightOp = {
  None: 0,
  Creating: 1,
  Killing: 2,
  Archiving: 3,
  Restoring: 4,
  Replacing: 5,
  Respawning: 6,
} as const;

/** session.LifecycleAction: the daemon-owned archive/restore verb for a row.
 *  Absence means the row must expose neither reversible lifecycle control. Kill
 *  addressability is projected independently because startup-unknown rows may be
 *  torn down but must not reuse their unconfirmed runtime binding. */
export type LifecycleAction = "archive" | "restore";

/** session.IdleReason: mechanically established only; no pane-text inference. */
export type IdleReason =
  | "usage-limit"
  | "process-exited"
  | "restore-gave-up"
  | "recreate-pending"
  | "prompt-not-delivered"
  | "delivery-unconfirmed"
  | "no-pane-change-since-delivery"
  | "settled-after-pane-change";

/** A verified model transition observed after af handled an agent safety dialog. */
export interface AgentModelChange {
  before: string;
  after: string;
}

/** Durable terminal result of the daemon's automatic Lost-session restore loop. */
export interface LostRestoreFailure {
  attempts: number;
  error: string;
}

/** session.Status (session/instance.go) — the legacy single-axis int, read ONLY
 *  as a defensive fallback when a projection somehow omits `liveness` (never
 *  expected from the daemon's live Snapshot, which always emits it). */
export const Status = {
  Running: 0,
  Ready: 1,
  Loading: 2,
  Deleting: 3,
  Dead: 4,
  Lost: 5,
  Archived: 6,
} as const;

/**
 * The subset of session.InstanceData (session/storage.go) the sidebar renders.
 * Field names and JSON tags match the Go struct exactly so this decodes the
 * daemon projection as-is. Optional fields carry Go's `omitempty` semantics:
 * `liveness`/`in_flight_op` are dropped when zero, `limit_reset_at` only present
 * for a LimitReached row.
 */
export interface SessionData {
  id?: string;
  title: string;
  branch: string;
  path?: string;
  /** RFC3339 creation time; the rail orders rows by it for a stable list. */
  created_at?: string;
  /** Legacy single-axis status int (fallback source only; see Status). */
  status?: number;
  /** Daemon-owned health axis; absent (→ Unset) only on pre-#1195 records. */
  liveness?: number;
  /** Transient client-op axis; absent (→ None) in the steady state. */
  in_flight_op?: number;
  /** Daemon-owned lifecycle capability. The web consumes this decision instead
   *  of re-deriving TUI policy from liveness/in-flight fields. */
  lifecycle_action?: LifecycleAction;
  /** Daemon-owned explicit teardown capability. True means the row has a stable
   *  non-creating target; absence/false fails closed. */
  can_kill?: boolean;
  /** Daemon-owned agent-swap capability (#2013). True means this session can be
   *  handed off to a different agent in place — a local-worktree session in a
   *  runtime state that admits the swap. The web consumes this decision instead of
   *  re-deriving the TUI's handoff policy from backend/liveness fields; absence/false
   *  fails closed. */
  can_handoff?: boolean;
  /** The agent enum this session is treated AS (session.CurrentAgentName): the
   *  handoff picker excludes it, matching the daemon's same-agent guard. Absent
   *  when unknowable. */
  current_agent?: string;
  /** Daemon-owned reserved-root decision (#2513): true for the always-on root
   *  agent. The web pins root to the top of the rail and draws the demarcation rule
   *  by CONSUMING this decision (session.IsReservedTitle, projected) rather than
   *  re-matching the title in the browser; absence/false fails closed (not root). */
  is_root?: boolean;
  /** Live, projection-only model diagnostic; never restored from instances.json. */
  model_change?: AgentModelChange;
  /** Bounded warning for an archive whose complete retained report is storage-only.
   *  It remains on restored rows so an automatic Lost recovery cannot make the
   *  omitted files disappear from subsequent snapshots. */
  archive_warning?: string;
  /** Daemon-derived explanation for a non-working row. Every value is established
   *  from lifecycle/delivery/churn facts; absence means af cannot say why. */
  idle_reason?: IdleReason;
  /** Present while a Lost row has exhausted automatic restore attempts. */
  lost_restore_failure?: LostRestoreFailure;
  /** RFC3339 time of the most recent actual prompt send attempt. */
  last_prompt_attempt_at?: string;
  /** Closed delivery observation. Both unverified values are uncertainty, not failure. */
  last_prompt_delivery_status?: "delivered" | "not-delivered" | "sent-unverified" | "could-not-confirm";
  /** RFC3339 time when the daemon most recently observed pane bytes change. */
  last_pane_churn_at?: string;
  /** One-shot note on a re-created root agent that did not demonstrably come back
   *  on its prior conversation (#2629): "fresh" when its context is provably gone,
   *  "unknown" when the resolved command selects its own conversation so af cannot
   *  say. The daemon clears it the first time any client opens the session's pane.
   *  An unrecognized value renders no note — a newer daemon must never make this
   *  rail invent one. */
  root_recreate_context?: "fresh" | "unknown";
  /** Usage-limit reset time (RFC3339), present only for a LimitReached row. */
  limit_reset_at?: string;
  /** Backend discriminator; "remote" marks a remote-hook session (→ [remote]). */
  backend_type?: string;
  /** The daemon's OWN answer, per tab kind, to "may this session gain one of
   *  these" — session.Capabilities.RefuseTabKind projected onto the snapshot
   *  (#3060).
   *
   *  Read this instead of deriving anything from backend_type. The client used to
   *  compute the rule itself, which agreed with the daemon only by coincidence and
   *  would disagree the moment a refusal lifted; projecting it means the affordance
   *  and the call behind it cannot drift, and a kind that becomes available off-box
   *  needs no change here at all. Absent on a pre-#3060 daemon — see
   *  allowedTabKinds for how that degrades. */
  tab_kinds?: TabKindAllowance[];
  /** Capabilities.TabManagement projected: may this session's tab ROSTER be
   *  mutated (rename, reorder)? A different daemon rule from creating a kind or
   *  closing a tab, with a different answer — `tabMutationTarget` still gates on
   *  it — so reusing either of those verdicts here offers controls the daemon
   *  refuses (#3060). */
  tab_roster_mutable?: boolean;
  /** Worktree metadata; the rail reads `repo_path` (the session's repo root) to
   *  derive the new-session modal's project picker, exactly as the TUI does from
   *  InstanceData.Worktree.RepoPath (app/switch_project.go buildProjectListFrom). */
  worktree?: WorktreeData;
  /** The daemon-discovered PR projection (session/storage.go PRInfoData; #3232
   *  made discovery daemon-side, so every surface receives it). Go's `omitempty`
   *  cannot drop a struct field, so a session with no discovered PR arrives as
   *  `pr_info: {}` — consumers treat a record without a number and url as "no
   *  PR" (fail closed) rather than keying on the field's presence. */
  pr_info?: PRInfoData;
  /** The session's tabs (session/storage.go InstanceData.Tabs): index 0 is the
   *  agent tab, followed by up to 9 user-created shell/process tabs (#930). The
   *  web tab bar renders these and streams a selected tab via /stream?tab=<idx>.
   *  Absent (→ one implicit agent tab) only on pre-#930 records. */
  tabs?: TabData[];
}

/** The wire shape of session/storage.go PRInfoData: the daemon's cached answer to
 *  "which PR belongs to this session's branch". Field names match the Go JSON tags
 *  (all `omitempty`), and `state` carries gh's uppercase vocabulary (OPEN / MERGED /
 *  CLOSED) verbatim — display code lowercases it rather than mapping through a
 *  table, so an unrecognized future state still renders honestly. */
export interface PRInfoData {
  number?: number;
  title?: string;
  url?: string;
  state?: string;
  /** The exact ref the lookup ran against; consumers making destructive decisions
   *  must match it, but display code does not read it. */
  branch?: string;
}

/** The subset of session.TabData (session/storage.go) the web tab bar reads: the
 *  display name and the kind (index 0 / TabKind.Agent is the unclosable agent
 *  tab). Field names match the Go JSON tags so this decodes the projection as-is. */
export interface TabData {
  /** The tab's stable id (session/storage.go TabData.ID, #1738): minted at
   *  creation and never reused, so the web addresses a tab's stream (?tab_id=) and
   *  its DnD/pane binding by this id rather than the ordinal, which shifts on a
   *  reorder/close. Absent only for a legacy record written before #1738. */
  id?: string;
  name: string;
  kind: number;
  command?: string;
  tmux_name?: string;
  /** The iframe target of a web tab (TabKind.Web); absent for other kinds. A
   *  loopback URL is rendered through the same-origin daemon proxy, an external
   *  URL directly. Mirrors session.TabData.URL (session/storage.go). */
  url?: string;
}

/** The subset of session.GitWorktreeData (session/storage.go) the web reads: the
 *  repo root the session belongs to, used to group/pick projects. */
/** One creatable tab kind and whether this session may gain one. */
export interface TabKindAllowance {
  /** The `--kind` spelling the CLI accepts: "shell", "process", "web", "vscode". */
  kind: string;
  allowed: boolean;
  /** The daemon's own refusal text, empty when allowed. Rendered verbatim: it
   *  names the requirement that is actually unmet, which a client cannot know. */
  reason?: string;
}

export interface WorktreeData {
  repo_path?: string;
}

/** The Snapshot RPC response (daemon/snapshot.go: SnapshotResponse). */
export interface SnapshotResponse {
	instances: SessionData[] | null;
	delivery_alarms?: unknown[];
}

/** apiproto.Theme: the daemon-resolved semantic palette. Browser light/dark is
 * deliberately absent; theme.ts derives both modes from these source slots. */
export interface DaemonTheme {
  name?: string;
  foreground: string;
  foreground_strong: string;
  foreground_muted: string;
  foreground_dim: string;
  background: string;
  background_subtle: string;
  background_panel: string;
  accent: string;
  success: string;
  warning: string;
  error: string;
  info: string;
  purple: string;
  selection_background: string;
  selection_foreground: string;
  pane_border_default: string;
  pane_border_selected: string;
  pane_border_interactive: string;
  pane_border_preview: string;
}

/** GetThemeResponse (daemon/control_types.go). */
export interface ThemeResponse {
  theme: DaemonTheme;
}

/**
 * The subset of task.Task (task/task.go) the tasks view reads and mutates (#1592
 * Phase 5 PR8). Field names and JSON tags match the Go struct EXACTLY so this
 * decodes the daemon's ListTasks projection as-is and round-trips through
 * AddTask/UpdateTask unchanged. `id` is globally unique — the stable key every
 * mutation (UpdateTask/TriggerTask/RemoveTask) resolves by, NEVER the name (which
 * is optional and non-unique). Optional fields carry Go's `omitempty` semantics:
 * exactly one of `cron_expr` / `watch_cmd` is set on an enabled task, and the
 * `last_run_*` fields are absent until the task first runs.
 */
export interface TaskData {
  id: string;
  name?: string;
  prompt: string;
  /** Time trigger (cron schedule); exactly one of cron_expr / watch_cmd on an
   *  enabled task (task.ValidateTrigger). */
  cron_expr?: string;
  /** Event trigger: a long-lived watch command whose stdout lines fire the task. */
  watch_cmd?: string;
  /** Route deliveries into this session by title (empty ⇒ a fresh session per run). */
  target_session?: string;
  /** The repo root the task belongs to — the project it groups under. */
  project_path: string;
  /** The agent program; empty resolves the repo default at run time. */
  program: string;
  enabled: boolean;
  /** RFC3339 creation time. */
  created_at: string;
  /** RFC3339 last-run time (absent until the task first runs). */
  last_run_at?: string;
  /** The outcome of the last run (scheduler-owned; absent until first run). */
  last_run_status?: string;

  // Schedule health (#3623). Every field below is DERIVED BY THE DAEMON at read
  // time and never stored, so a client renders them and never sends them —
  // AddTask/UpdateTask ignore them, and the store strips them on both sides.
  // They arrive on the records ListTasks returns, which is why showing them here
  // is rendering rather than plumbing (#3626).

  /** The task's last run precedes its most recent scheduled occurrence by more
   *  than one slack window: it has stopped firing. */
  overdue?: boolean;
  /** How many scheduled fires it has missed since. */
  missed_occurrences?: number;
  /** The count above hit the derivation's cap and is a FLOOR, not an exact
   *  number — render it as "N+" rather than as N. */
  missed_occurrences_capped?: boolean;
  /** The scheduler cannot derive a next run from this task's expression: it has
   *  no trigger, the expression does not parse, or nothing matches inside the
   *  scheduler's search horizon. Not overdue (nothing was ever due) and not
   *  healthy either. */
  unschedulable?: boolean;
  /** WHICH shape of unschedulable, straight from the daemon's own classifier
   *  (task.UnschedulableReason). Present only when `unschedulable` is set.
   *  Re-deriving this from `cron_expr` here is what had other surfaces calling an
   *  ABSENT expression invalid, so read it rather than reclassify. */
  unschedulable_reason?: "no-trigger" | "invalid-expression" | "no-occurrence";
  /** No lateness verdict could be reached — nothing to measure from, or nothing
   *  the schedule can be evaluated against. UNKNOWN, not healthy. */
  unassessable?: boolean;
  /** RFC3339 time the LIVE scheduler entry will next fire, read off what is
   *  armed rather than recomputed from the expression. Absent when the task is
   *  not armed, and that absence is itself the signal. */
  next_run_at?: string;
  /** The live arming observation: "armed", "not-armed", or ABSENT when no daemon
   *  has reported on it (none running, or one still starting). Absent must never
   *  be read as "not armed". */
  arming?: string;
}

/**
 * A FIELD-LEVEL patch for UpdateTask (task.TaskUpdate, #1700): only the fields
 * present are changed; the daemon leaves every omitted field as-stored, merging
 * the patch onto the freshly-loaded record under its file lock. So the
 * enable/disable toggle sends just `{ enabled }` and can never clobber a
 * concurrent edit another client made to a different field. Field names/JSON tags
 * match the Go TaskUpdate struct EXACTLY (the daemon rejects unknown keys).
 */
export interface TaskUpdate {
  name?: string;
  prompt?: string;
  cron_expr?: string;
  watch_cmd?: string;
  target_session?: string;
  /** The repo root the task belongs to — the project it groups under. Present so
   *  the edit form can move a task between projects (#1935); the Go task.TaskUpdate
   *  struct carries it, and the TUI already edits it (ui/task_pane_edit.go). */
  project_path?: string;
  program?: string;
  enabled?: boolean;
}

/**
 * The project compare-and-swap every task mutation carries (#3230): the Go
 * task.ProjectExpectation, field names/JSON tags matched EXACTLY. The daemon
 * re-checks — under the same locked operation that mutates the task — that the
 * task is still bound to the project the pane displayed it under, and refuses
 * the action if another client rebound it meanwhile. `enforce` distinguishes
 * "expected to be unbound" (project_path "") from "no expectation" — an absent
 * or zero-value expect disables the check, which is why the api layer builds
 * this from the displayed record itself rather than letting callers pass one.
 */
export interface ProjectExpectation {
  enforce: boolean;
  project_path: string;
}

/**
 * The slice of a displayed task record a mutation needs: the stable id to
 * target and the project binding to pin (#3230). The api.ts mutation helpers
 * take this instead of the full TaskData both because it is all they read and
 * because the surface-parity audit (parity/derive_test.go webNestedValueReach)
 * derives AddTask's reachable payload from every TaskData-typed parameter in
 * api.ts — typing the mutations TaskData would make their pass-through call
 * sites unanalyzable. Callers still hand over the full displayed TaskData;
 * structural typing narrows it here.
 */
export interface TaskMutationRef {
  id: string;
  project_path: string;
}

/** The ListTasks RPC response (daemon/control_types.go: ListTasksResponse). */
export interface TasksResponse {
  tasks: TaskData[] | null;
}

/** agentproto.EventType (agentproto/message.go): the /v1/events discriminators. */
export type EventType =
  | "session.created"
  | "session.updated"
  | "session.killed"
  | "session.archived"
  | "session.restored"
  | "theme.changed"
  | "projects.changed"
  | "task.created"
  | "task.updated"
  | "task.removed";

/**
 * agentproto.Event (agentproto/message.go): one message on the /v1/events plane.
 * A session.* event's `data` is a marshaled InstanceData; created/updated carry
 * the full projection, while killed/archived/restored carry `{id, title}` — the
 * STABLE id plus the title (daemon/control_server.go, #1592 Phase 5 PR5). The
 * client keys its rail off the id (not the collision-prone title) and only falls
 * back to the title when a legacy/disk-only record carries no id.
 */
export interface WireEvent {
  type: EventType;
  data?: SessionData;
}

/**
 * config.ConfigEntry (config/manifest_value.go): one user-facing global config
 * key — its purpose, type, default, tier, whether it is settable, and the
 * user's live value — as returned by GetConfig.
 *
 * This is the whole reason the config screen has no key list of its own. The
 * manifest is derived from config_types.go and pinned to it by a reflective
 * coverage test, so a key added there arrives here automatically and the web
 * form renders it with no edit to the bundle. A hand-written form would drift
 * the moment someone added a key — which is exactly the class the manifest
 * exists to kill.
 */
export interface ConfigEntry {
  key: string;
  type: string;
  /** Every accepted config shape when the normalized `type` alone is
   *  incomplete, such as theme's named string preset or custom table. */
  accepted_types?: string[];
  default: string;
  purpose: string;
  tier: number;
  tier_name: string;
  /** The MANIFEST's claim that `af config set` accepts this whole key. Dynamic
   *  families also retain their leaf form (`program_overrides.claude`). */
  settable: boolean;
  /** Present when the value is enumerated; drives a picker instead of a text
   *  field. For a table it constrains the entry NAMES, not the value. */
  enum?: string[];
  value: string;
  /** True for every key today: a successful save carries the writer's exact
   *  per-key effect notice (live now, next daemon start, or next af launch). */
  requires_restart: boolean;
}

/** GetConfigResponse (daemon/control_types.go). */
export interface ConfigResponse {
  entries: ConfigEntry[];
  /** The config.toml the values were read from, so the UI can name the file it
   *  is editing rather than leaving an AF_HOME user guessing. */
  path: string;
}

/** config.SetResult (config/configset.go), as returned by SetConfigValue. */
export interface ConfigSetResult {
  key: string;
  value: string;
  path: string;
  requires_restart: boolean;
}

/** SetConfigValueResponse (daemon/control_types.go). The restart notice rides
 *  on the response rather than being duplicated here so the TUI, the web UI,
 *  and the CLI cannot drift into three accounts of when an edit takes effect. */
export interface ConfigSetResponse {
  result: ConfigSetResult;
  restart_notice: string;
  /** Where the daemon is ACCEPTING now, when the written key moved one of its
   *  listeners (#3722) — absent for every other key. Saving network.listen_addr
   *  from this form moves the very listener the form is talking over, so the
   *  daemon names the new address in the reply it flushes on the old connection;
   *  without it the operator is told where they are NOT and left to guess where
   *  they are. Optional: an older daemon does not send it. */
  listener_addr?: string;
}

/** daemon.AccountEntry — one registered agent account (#3384/#3385).
 *
 *  It never carries credential material. `dir` is the DIRECTORY af points the
 *  agent's credential-root variable at, and `logged_in` is the presence of the
 *  agent's own credential file inside it, read by stat. So `logged_in` means
 *  "this account has an identity", not "this identity still works": only the
 *  agent can say the latter, and af never opens the file to ask. */
export interface AccountEntry {
  agent: string;
  name: string;
  dir: string;
  /** True while a session cannot yet be scoped to this agent's accounts —
   *  registering and logging in still work. */
  registration_only: boolean;
  logged_in: boolean;
}

/** ListAccountsResponse (daemon/control_types_accounts.go).
 *
 *  `agents` is not derivable from `entries`: a fresh install has no accounts and
 *  the register form still has to offer the agents one can be made for. */
export interface AccountsResponse {
  entries: AccountEntry[];
  agents: string[];
}

/** RegisterAccountResponse (daemon/control_types_accounts.go). */
export interface RegisterAccountResponse {
  entry: AccountEntry;
  /** What the agent's credential-root variable relocates, and anything af could
   *  not verify about the directory. Never about a credential's contents. */
  notices?: string[];
}

/** AccountLoginResponse (daemon/control_types_accounts.go). */
export interface AccountLoginResponse {
  agent: string;
  name: string;
  dir: string;
  /** The exact invocation the pane runs — the agent's own login command — so the
   *  UI can show what af is running rather than assert it. */
  program: string;
  /** The tmux session to attach to. EMPTY when `finished` is true: the flow ended
   *  before af could hand over a terminal and there is nothing to watch. */
  session_name: string;
  socket_path: string;
  /** True when this call joined a login flow that was already open. */
  reused: boolean;
  /** True when the agent's login command ran to completion before af could hand
   *  over the terminal. */
  finished: boolean;
  logged_in: boolean;
  notices?: string[];
}
