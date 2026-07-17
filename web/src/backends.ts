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

/** The daemon's three-outcome availability answer (daemon.BackendAvailability).
 *
 *  "unknown" is load-bearing: it means the daemon could NOT check (e.g. the repo's
 *  config would not parse), which is a different answer from yes and from no. It
 *  must never be folded into either — an unknown rendered as available is a
 *  promise nobody verified, and rendered as unavailable it invents a finding. */
export type BackendAvailability = "available" | "unavailable" | "unknown";

/** One backend as the daemon reports it (daemon.BackendOption). */
export interface BackendOption {
  /** The wire value sent back as CreateSession's `backend`. */
  name: string;
  /** The checked answer for this repo. */
  status: BackendAvailability;
  /** Actionable reason whenever `status` is not "available" — the same text the
   *  CLI prints at create time. Absent when available. */
  reason?: string;
}

/** The daemon's per-repo backend catalog (daemon.ListBackendsResponse). */
export interface BackendCatalog {
  backends: BackendOption[];
  /** The backend a create with no explicit backend resolves to for this repo.
   *  EMPTY when the repo's `backend` key names something unrecognized: such a
   *  create fails rather than falling back to local, so there is no default. */
  default: string;
  /** The checked answer for the repo-default choice itself. */
  default_status: BackendAvailability;
  /** Why the default is not usable, naming the offending value and its file. */
  default_reason?: string;
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
  /** The daemon's checked answer. Only "available" may be offered as usable: both
   *  "unavailable" (checked, will fail) and "unknown" (could not be checked) are
   *  unverified promises, and the picker must not make one.
   *
   *  Such a choice stays SELECTABLE: disabling it would hide `reason`, which is the
   *  actionable half — a greyed-out "docker" tells a user they cannot have it, never
   *  that one config key would give it to them. Selecting it shows the reason and
   *  blocks the submit instead. */
  status: BackendAvailability;
  /** The actionable reason this choice is not usable; "" when it is fine. */
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
    return [{ value: REPO_DEFAULT, label: "Repo default", status: "available", reason: "" }];
  }

  const choices: BackendChoice[] = [
    {
      value: REPO_DEFAULT,
      // "Repo default" with no parenthetical when the daemon reports no default:
      // that is the misconfigured case, where naming a backend would be inventing
      // one. The reason says what is wrong with the key.
      label: catalog.default === "" ? "Repo default" : `Repo default (${catalog.default})`,
      // Taken from the daemon, not inferred here. A repo whose declared default is
      // broken resolves to that broken backend and FAILS — it does not quietly run
      // local — so the default is not automatically a safe harbour.
      status: catalog.default_status,
      reason: catalog.default_status === "available" ? "" : (catalog.default_reason ?? ""),
    },
  ];

  for (const opt of catalog.backends) {
    choices.push({
      value: opt.name,
      label: opt.name,
      status: opt.status,
      reason: opt.status === "available" ? "" : (opt.reason ?? ""),
    });
  }
  return choices;
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
 * Whether a create with this selection can proceed. Only an "available" choice may
 * — a picker is a promise, and both "unavailable" (checked, would fail) and
 * "unknown" (could not be checked) are promises nobody verified. `backendNotice`
 * says why, so the block is never a dead end.
 *
 * Blocking "unknown" cannot lock a user out: the only thing that makes a backend
 * unknown is an unreadable repo config, and local stays available through that (it
 * reads no repo config), so a session can still be created while they fix the file.
 *
 * A choice the picker does not list at all is allowed through: the daemon is the
 * authority on what it accepts, and a client that vetoed a value it merely does not
 * recognize would be the hardcoded-enum bug (#1933) wearing a different hat.
 */
export function backendSelectable(choices: BackendChoice[], selected: string): boolean {
  const choice = choices.find((c) => c.value === selected);
  return choice === undefined || choice.status === "available";
}
