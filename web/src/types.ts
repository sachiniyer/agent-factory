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
} as const;

/** session.LifecycleAction: the daemon-owned lifecycle verb for a row. Absence
 *  means the row must expose no archive/restore/kill controls (#2234). */
export type LifecycleAction = "archive" | "restore";

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
  /** Usage-limit reset time (RFC3339), present only for a LimitReached row. */
  limit_reset_at?: string;
  /** Backend discriminator; "remote" marks a remote-hook session (→ [remote]). */
  backend_type?: string;
  /** Worktree metadata; the rail reads `repo_path` (the session's repo root) to
   *  derive the new-session modal's project picker, exactly as the TUI does from
   *  InstanceData.Worktree.RepoPath (app/switch_project.go buildProjectListFrom). */
  worktree?: WorktreeData;
  /** The session's tabs (session/storage.go InstanceData.Tabs): index 0 is the
   *  agent tab, followed by up to 9 user-created shell/process tabs (#930). The
   *  web tab bar renders these and streams a selected tab via /stream?tab=<idx>.
   *  Absent (→ one implicit agent tab) only on pre-#930 records. */
  tabs?: TabData[];
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
export interface WorktreeData {
  repo_path?: string;
}

/** The Snapshot RPC response (daemon/snapshot.go: SnapshotResponse). */
export interface SnapshotResponse {
  instances: SessionData[] | null;
  delivery_alarms?: unknown[];
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
  default: string;
  purpose: string;
  tier: number;
  tier_name: string;
  /** The MANIFEST's claim: `af config set` accepts this key — or, for a dynamic
   *  family, its LEAVES (`af config set program_overrides.claude …`). Do NOT
   *  drive a control off this: "the CLI takes this key's leaves" is not "this
   *  row is one editable value". Use `editable`. */
  settable: boolean;
  /** The EDITOR's question: can this row be edited directly, as a single scalar
   *  the write path will accept? Settable minus the dynamic families, derived
   *  Go-side from the real allowlist. False renders read-only with `edit_hint`. */
  editable: boolean;
  /** How to change a key that is not directly editable. Not always "hand-edit
   *  the file" — a dynamic family's leaves ARE settable from the CLI, so the
   *  hint names that command. */
  edit_hint?: string;
  /** Present when the value is enumerated; drives a picker instead of a text
   *  field. For a table it constrains the entry NAMES, not the value. */
  enum?: string[];
  value: string;
  /** True for every key today: config.toml is read at startup, so an edit
   *  applies when af and the daemon next start. */
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
}
