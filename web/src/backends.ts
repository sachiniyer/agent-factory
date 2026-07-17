// The create form's backend choice (#1933).
//
// The daemon has always accepted a backend on create (the CLI's `--backend`), but
// the web never offered one, so a remote session could only be started from the
// TUI/CLI. This module is the web half of closing that: it turns the daemon's
// ListBackends answer into the option list the new-session modal renders.
//
// The one rule this file exists to hold: THE WEB KNOWS NO BACKEND NAMES. There is
// no local enum, no name→label map, no `if (name === "docker")`. Every option is
// built from the response, so a backend added server-side is offered here with no
// change to this file — the property web/src/backends.test.ts pins. A label map
// would look harmless and would silently render a new backend as blank.

/** One backend as the daemon reports it (daemon.BackendOption). */
export interface BackendOption {
  /** The wire value sent back as CreateSession's `backend`. */
  name: string;
  /** False when this repo's config cannot satisfy the backend. */
  available: boolean;
  /** Actionable reason when `available` is false — the same text the CLI prints
   *  at create time. Absent when available. */
  reason?: string;
}

/** The daemon's per-repo backend catalog (daemon.ListBackendsResponse). */
export interface BackendCatalog {
  backends: BackendOption[];
  /** The backend a create with no explicit backend resolves to for this repo. */
  default: string;
}

/** The sentinel value of the "repo default" choice. It is the EMPTY STRING on
 *  purpose: it is what createSession omits on, so picking the default sends no
 *  backend and the repo's own config decides — the same defaulting the CLI gets
 *  by leaving `--backend` off. Any non-empty sentinel here would eventually be
 *  sent as a literal backend name. */
export const REPO_DEFAULT = "";

/** A backend choice, ready to render as an <option>. */
export interface BackendChoice {
  /** The <option> value: REPO_DEFAULT, or a backend name to send verbatim. */
  value: string;
  /** The visible label. It is the daemon's backend name verbatim — deliberately
   *  NOT looked up in a local name→label map, which is what would silently render
   *  a newly added backend as blank. */
  label: string;
  /** False when this repo's config cannot satisfy the choice, so creating with it
   *  would fail. Such a choice stays SELECTABLE: disabling it would hide `reason`,
   *  which is the actionable half — a greyed-out "docker" tells a user they cannot
   *  have it, never that one config key would give it to them. Selecting it shows
   *  the reason and blocks the submit instead. */
  available: boolean;
  /** The actionable reason this choice is unusable; "" when it is fine. */
  reason: string;
}

/**
 * Builds the backend picker's choices from the daemon's catalog.
 *
 * The first choice is always "repo default", labelled with the backend it
 * actually resolves to, so the user can see that a repo defaults to docker
 * without having to pick docker. The rest are the daemon's backends in the order
 * it sent them (its canonical enum order).
 *
 * Unavailable backends are kept rather than dropped: a user who read the docs and
 * looks for docker deserves the reason it is unusable, not a mystery absence.
 */
export function backendChoices(catalog: BackendCatalog | null): BackendChoice[] {
  if (catalog === null) {
    return [{ value: REPO_DEFAULT, label: "Repo default", available: true, reason: "" }];
  }

  const reason = defaultReason(catalog);
  const choices: BackendChoice[] = [
    {
      value: REPO_DEFAULT,
      label: catalog.default === "" ? "Repo default" : `Repo default (${catalog.default})`,
      // A repo whose declared default is unconfigured is genuinely unusable: that
      // create resolves to the broken backend and fails. Reporting it as such
      // surfaces the config bug at choose time instead of at create time — which is
      // the whole point of #1933 — and is why this is computed, not hardcoded true.
      available: reason === "",
      reason,
    },
  ];

  for (const opt of catalog.backends) {
    choices.push({
      value: opt.name,
      label: opt.name,
      available: opt.available,
      reason: opt.available ? "" : (opt.reason ?? ""),
    });
  }
  return choices;
}

/**
 * The reason the "repo default" choice is unusable, when the repo's declared
 * default is itself unconfigured (e.g. `backend = "docker"` with no docker.image).
 *
 * Returns "" when the default is fine, or when it names a backend the daemon did
 * not list — nothing is known about that, so claiming a problem would be a guess.
 */
function defaultReason(catalog: BackendCatalog): string {
  const resolved = catalog.backends.find((b) => b.name === catalog.default);
  if (resolved === undefined || resolved.available) {
    return "";
  }
  return resolved.reason ?? "";
}

/**
 * The message to show for the currently-selected backend, or "" when the
 * selection is fine. This is the choose-time answer #1933 asks for: the user reads
 * "backend=docker requires docker.image…" while picking, instead of submitting and
 * decoding a create failure.
 */
export function backendNotice(choices: BackendChoice[], selected: string): string {
  const choice = choices.find((c) => c.value === selected);
  return choice === undefined ? "" : choice.reason;
}

/**
 * Whether a create with this selection can proceed. False only for a selection the
 * daemon reported unusable — the create would fail, so the form blocks it and
 * `backendNotice` says why.
 *
 * An unknown selection is allowed through: the daemon is the authority on what it
 * accepts, and a client that vetoed a value it merely does not recognize would be
 * the same hardcoded-enum bug (#1933) wearing a different hat.
 */
export function backendSelectable(choices: BackendChoice[], selected: string): boolean {
  const choice = choices.find((c) => c.value === selected);
  return choice === undefined || choice.available;
}
