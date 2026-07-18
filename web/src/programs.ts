// The create/task forms' agent-program choice (#1970).
//
// The web used to hardcode ["claude","codex","aider","gemini","amp","opencode"]
// in two pickers (the new-session modal and the task form) while
// session/tmux/session.go owned the canonical list. The TUI and CLI read the
// canonical one, so both picked up a new agent for free; the web kept a copy,
// which meant adding a sixth agent server-side left the web silently offering
// five. Nothing failed — the copies happened to be current. The COUPLING was the
// bug, and a parity test that merely caught the copy going stale was a mitigation,
// not a fix.
//
// This module is the fix: it turns the daemon's ListPrograms answer into the
// option list both forms render.
//
// The one rule this file exists to hold: THE WEB KNOWS NO AGENT NAMES. There is no
// local enum, no name→label map, no `if (name === "claude")`. Every option is built
// from the response, so an agent added server-side is offered here with no change
// to this file — the property web/src/programs.test.ts pins. A label map would look
// harmless and would silently render a new agent as blank.
//
// Note what is deliberately absent: availability. backends.ts carries a tri-state
// status because a backend's preconditions are repo-config facts the daemon can
// check. Whether an agent binary is installed is a fact about the machine the agent
// will run on, which for a docker or ssh backend is not the daemon's machine — so
// the daemon does not claim it and this module has nothing to render (see
// daemon/programs.go). Programs are therefore a plain list, never blocked.

/** One agent as the daemon reports it (daemon.ProgramOption). */
export interface ProgramOption {
  /** The wire value sent back as CreateSession's / AddTask's `program`. */
  name: string;
}

/** The daemon's agent catalog (daemon.ListProgramsResponse). */
export interface ProgramCatalog {
  programs: ProgramOption[];
  /** The program a create with no explicit program resolves to for this repo.
   *  EMPTY when the daemon has no default to report, in which case the repo-default
   *  choice is rendered with no parenthetical rather than naming a guess. */
  default: string;
}

/** The sentinel value of the "repo default" choice. It is the EMPTY STRING on
 *  purpose: it is what the create/task requests already send to mean "unspecified",
 *  so picking the default sends no program and the repo's own config decides — the
 *  same defaulting the CLI gets by leaving `--program` off. Any non-empty sentinel
 *  here would eventually be sent as a literal program name. */
export const PROGRAM_REPO_DEFAULT = "";

/** An agent choice, ready to render as an <option>. */
export interface ProgramChoice {
  /** The <option> value: PROGRAM_REPO_DEFAULT, or a program name to send verbatim. */
  value: string;
  /** The visible label. It is the daemon's program name verbatim — deliberately NOT
   *  looked up in a local name→label map, which is what would silently render a
   *  newly added agent as blank. */
  label: string;
}

/**
 * Builds the program picker's choices from the daemon's catalog.
 *
 * The first choice is always "repo default", labelled with the agent it actually
 * resolves to, so a user can see that a repo defaults to codex without having to
 * pick codex. The rest are the daemon's programs in the order it sent them (its
 * canonical enum order, which is append-only server-side and so stable).
 *
 * `keep` preserves a value the catalog does not list — a program set via the CLI,
 * or one from an older af that has since been retired. Without it, opening the task
 * editor to change a prompt would silently reset such a task to the repo default,
 * which is data loss disguised as a form. It is appended last, so it never displaces
 * a real choice, and it is trusted for the same reason backends.ts trusts an
 * unrecognized selection: the daemon is the authority on what it accepts, and a
 * client that vetoed a value it merely does not recognize would be the hardcoded-enum
 * bug wearing a different hat.
 */
export function programChoices(catalog: ProgramCatalog | null, keep = ""): ProgramChoice[] {
  const choices: ProgramChoice[] = [
    {
      value: PROGRAM_REPO_DEFAULT,
      // "Repo default" with no parenthetical when the daemon reports no default —
      // naming an agent there would be inventing one.
      label: catalog === null || catalog.default === "" ? "Repo default" : `Repo default (${catalog.default})`,
    },
  ];

  for (const opt of catalog?.programs ?? []) {
    choices.push({ value: opt.name, label: opt.name });
  }

  const extra = keep.trim();
  if (extra !== "" && !choices.some((c) => c.value === extra)) {
    choices.push({ value: extra, label: extra });
  }
  return choices;
}
