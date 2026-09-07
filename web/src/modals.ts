import { handoffAccountChoices } from "./handoff_accounts.js";
// The web client's modal overlays (#1592 Phase 5 PR5): the new-session form, the
// send-prompt box, and the kill/archive confirms — the write surface that
// completes the v1 loop (list → attach → type → create/kill). They mirror the
// TUI's overlays (ui/overlay/textOverlay.go, projectPickerOverlay.go) as small
// additive views.
//
// Each modal is built ONCE when opened and returns a ModalHandle: index.ts mounts
// it into the shell's persistent modal host, drives the async API call the
// submit fires, and patches busy/error IN PLACE via the handle — never rebuilding
// the DOM, so typed input survives a failed submit (e.g. "title taken") for a
// retry. This is the same build-once/patch-in-place model the terminal and rail
// header use, and it keeps the store a pure read-model: modals are ephemeral UI
// managed imperatively, not store state.
//
// CSP-safe like the rest of the client: createElement + addEventListener only, no
// innerHTML with markup and no inline handlers, so the daemon's default-src 'self'
// policy holds.

import { AccountSelection } from "./account_selection.js";
import type { CreateSessionInput, DirectoryListing } from "./api.js";
import {
  type AccountChoice,
  accountAgentFor,
  accountChoices,
  accountDefaultFor,
  accountNotice,
  accountSelectable,
} from "./account_scope.js";
import { type BackendCatalog, type BackendChoice, REPO_DEFAULT, backendChoices, backendNotice, backendSelectable } from "./backends.js";
import { type DirectoryPickerHandle, directoryPicker } from "./dirpicker.js";
import { h } from "./dom.js";
import { PROGRAM_REPO_DEFAULT, type ProgramCatalog, type ProgramChoice, handoffAgentChoices, programChoices } from "./programs.js";
import type { AccountsResponse } from "./types.js";

import { modalChrome, field, defaultsDisclosure, type ModalHandle } from "./components.js";
export { modalChrome, field, type ModalHandle } from "./components.js";

/** Wraps the card's content in a <form> so Enter submits and the browser handles
 *  focus, calling onSubmit with preventDefault already applied. */
export function asForm(card: HTMLElement, onSubmit: () => void): void {
  // The card children were appended directly; re-parent them under a form so a
  // native submit (Enter / the primary button) is captured once.
  const form = h("form", { class: "af-modal-form" });
  while (card.firstChild) {
    form.append(card.firstChild);
  }
  card.append(form);
  form.addEventListener("submit", (e) => {
    e.preventDefault();
    onSubmit();
  });
}

/** The new-session modal: title, project picker, program, backend, account, and
 *  initial prompt. Projects are the distinct repo roots derived from the current
 *  sessions (like the TUI's zero-config picker). onSubmit fires with the collected
 *  form values.
 *
 *  loadBackends fetches the picked project's backend catalog (#1933), and
 *  loadPrograms the agent catalog (#1970). Both are passed in rather than imported
 *  so this module stays free of the API/token layer, and both are re-run whenever
 *  the project changes: each catalog's default is a per-repo fact, so the choices
 *  must follow the project picker. If either rejects, its field degrades to "repo
 *  default" alone — the exact behavior before these fields existed, so a catalog
 *  failure can never block a create.
 *
 *  loadAccounts fetches the daemon host's credential-account registry (#3844). The
 *  registry itself is not per-project — accounts live in the daemon's home, not in a
 *  repo — but the DEFAULT it reports is (#3386): `default_accounts` admits the
 *  machine-local per-project config layer, so which account a create here would use
 *  depends on which project is picked. It is therefore re-run with the other two
 *  whenever the project changes. What is per-selection instead is which slice of the
 *  registry applies: an account belongs to one agent, so the offered rows follow the
 *  PROGRAM field, and the resolution of a "repo default" program comes from the
 *  program catalog. A rejection degrades that field to the ambient identity alone,
 *  which is what every create did before it existed. */
export function newSessionModal(
  projects: string[],
  defaultProject: string | null,
  callbacks: {
    onSubmit: (values: CreateSessionInput) => void;
    onCancel: () => void;
    loadBackends: (repoPath: string) => Promise<BackendCatalog>;
    loadPrograms: (repoPath: string) => Promise<ProgramCatalog>;
    loadAccounts: (repoPath: string) => Promise<AccountsResponse>;
    // #2470: fetch a random, readable session name from the daemon (the wordlist
    // is Go-only) to show as shadow text; an empty submit adopts it.
    suggestName: () => Promise<string>;
  },
): ModalHandle {
  const { handle, body, confirmBtn } = modalChrome({
    title: "New session",
    confirmLabel: "Create",
    confirmClass: "af-primary",
    onCancel: callbacks.onCancel,
  });

  const titleInput = h("input", { type: "text", class: "af-input", placeholder: "Session title", autocomplete: "off" });
  titleInput.setAttribute("aria-label", "Session title");
  // #2470: the daemon-suggested autocreate name. Shown as the title placeholder
  // (shadow text that clears the instant the user types) and, when the field is
  // submitted empty, used as the session name. "" until the fetch lands or if it
  // fails — in which case the field falls back to requiring a typed title.
  let suggestedName = "";

  const projectSelect = h("select", { class: "af-input" });
  projectSelect.setAttribute("aria-label", "Project");
  if (projects.length === 0) {
    // Post-#2456 the coherent zero-projects action is the switcher's "+ Add project",
    // not the TUI: once a repo is registered it appears here (pickerProjects ∪ the
    // registry), so point the user at it rather than at a different surface.
    const opt = h("option", { value: "" }, "No projects yet — add one from the project switcher first");
    opt.disabled = true;
    opt.selected = true;
    projectSelect.append(opt);
    confirmBtn.disabled = true;
  } else {
    for (const p of projects) {
      projectSelect.append(h("option", { value: p }, projectLabel(p)));
    }
    // Default to the currently-scoped project (redesign PR2): a new session created
    // from within a project lands in that project by default.
    if (defaultProject && projects.includes(defaultProject)) {
      projectSelect.value = defaultProject;
    }
  }

  // The program field (#1970). Like the backend field below, its options come from
  // the daemon, never from a list here, so an agent added server-side shows up with
  // no change to the web.
  const programSelect = h("select", { class: "af-input" });
  programSelect.setAttribute("aria-label", "Program");
  let programs: ProgramChoice[] = programChoices(null);

  // The backend field (#1933). Its options come from the daemon, never from a list
  // here, so a backend added server-side shows up with no change to the web.
  const backendSelect = h("select", { class: "af-input" });
  backendSelect.setAttribute("aria-label", "Backend");
  const backendHint = h("p", { class: "af-modal-hint af-backend-hint" });
  // Announce the notice when it changes: the reason a choice is unusable must
  // reach a screen reader, not only sighted users scanning under the select.
  backendHint.setAttribute("role", "status");

  let choices: BackendChoice[] = backendChoices(null);

  // The account field (#3844): which of the agent's registered credential accounts
  // the session runs as. Its options come from the daemon's ListAccounts registry,
  // never from a list here, so an account registered a second ago is offered with
  // no change to the web.
  const accountSelect = h("select", { class: "af-input" });
  accountSelect.setAttribute("aria-label", "Account");
  const accountHint = h("p", { class: "af-modal-hint af-account-hint" });
  // Announced for the same reason the backend hint is: the reason a choice is
  // unusable — or the fact that one has no credential yet — must reach a screen
  // reader, not only sighted users scanning under the select.
  accountHint.setAttribute("role", "status");

  // The registry as the daemon reports it, and the agent slice currently offered.
  // null means unknown: block while pending, disclose daemon inheritance on failure.
  let accounts: AccountsResponse | null = null;
  let accountsFailed = false;
  let programCatalog: ProgramCatalog | null = null;
  let accountRows: AccountChoice[] = accountChoices(null, "");
  const accountSelection = new AccountSelection();
  let programsPending = false;

  // Mirrors the chrome's busy flag. An async availability refresh can land at ANY
  // time — including mid-submit — and it must never be the thing that decides
  // whether Create is clickable.
  let busy = false;

  // The SINGLE writer of confirmBtn.disabled, so no update can clobber another's
  // reason for disabling it. Every input that gates Create is OR-ed here rather
  // than assigned from its own call site:
  //   busy      — a submit is in flight; re-enabling would allow a double-create.
  //   projects  — nothing to create in (set once, before any catalog lands).
  //   backend   — the selection is unusable or unverified.
  // A bare `confirmBtn.disabled = …` anywhere else silently drops the other two.
  const defaults = defaultsDisclosure();
  const accountBlock = h("div", { class: "af-defaults-account" }, field("Account", accountSelect), accountHint);
  const accountSlot = h("div", { class: "af-defaults-account-slot" });
  const syncSubmitState = (): void => {
    backendHint.textContent = backendNotice(choices, backendSelect.value);
    accountHint.textContent = accountNotice(accountRows, accountSelect.value);
    if (accountSelection.namedChoicePending && !programsPending && (accountsFailed || programCatalog === null)) {
      accountHint.textContent = "Cannot verify the selected account. Reopen this form to try again.";
    }
    const choiceLabel = (select: HTMLSelectElement) => (select.selectedOptions[0]?.textContent ?? "Loading…")
      .replace(/^Repo default \((.*)\)$/, "$1 (default)")
      .replace(/^Use configured default \((.*)\)$/, "$1 (default)");
    // Ambiguity, unavailable choices and explicit overrides must remain in view.
    const accountNeedsChoice = !!accountHint.textContent || accountSelection.picked
      || (accountRows.length > 2 && !accountDefaultFor(accounts, accountAgentFor(programSelect.value, programCatalog)));
    defaults.setSummary([`Program: ${choiceLabel(programSelect)}`, `Backend: ${choiceLabel(backendSelect)}`,
      ...(accountNeedsChoice ? [] : [`Account: ${choiceLabel(accountSelect)}`])]);
    const accountParent = accountNeedsChoice ? accountSlot : defaults.body;
    if (accountBlock.parentElement !== accountParent) {
      const focused = document.activeElement as HTMLElement | null;
      const ownsFocus = !!focused && accountBlock.contains(focused);
      accountParent.append(accountBlock);
      if (ownsFocus) focused.focus();
    }
    accountSlot.hidden = !accountNeedsChoice;
    if (backendHint.textContent || programSelect.value !== PROGRAM_REPO_DEFAULT || backendSelect.value !== REPO_DEFAULT) defaults.el.open = true;
    confirmBtn.disabled = busy
      || projects.length === 0
      || !backendSelectable(choices, backendSelect.value)
      || !accountSelectable(accountRows, accountSelect.value)
      || ((accounts === null || programsPending || !accountAgentFor(programSelect.value, programCatalog)) && accountSelection.namedChoicePending);
  };

  // Route the chrome's setBusy through the same writer. index.ts drives busy around
  // the create call; without this, a catalog response arriving mid-create would
  // re-enable the button underneath it.
  const chromeSetBusy = handle.setBusy.bind(handle);
  handle.setBusy = (b: boolean): void => {
    busy = b;
    chromeSetBusy(b);
    syncSubmitState();
  };

  const renderChoices = (): void => {
    const previous = backendSelect.value;
    backendSelect.replaceChildren();
    for (const choice of choices) {
      backendSelect.append(h("option", { value: choice.value }, choice.label));
    }
    // Re-selecting the prior value keeps a user's pick across a project switch when
    // it is still offered; otherwise fall back to the repo default, which always is.
    backendSelect.value = choices.some((c) => c.value === previous) ? previous : REPO_DEFAULT;
    syncSubmitState();
  };

  backendSelect.addEventListener("change", syncSubmitState);

  /** Rebuilds the account options for the agent the PROGRAM field currently names.
   *
   *  The previous pick is deliberately NOT carried across an agent change, which is
   *  where this differs from the backend and program fields above. Those re-select a
   *  prior value when it is still offered; an account name that survives into another
   *  agent's registry is a DIFFERENT IDENTITY with the same spelling, and silently
   *  keeping it is the exact outcome this feature exists to prevent. Same agent, same
   *  pick; different agent, back to the ambient identity. */
  const renderAccounts = (): void => {
    const agent = accountAgentFor(programSelect.value, programCatalog);
    // Wait for both per-project facts before judging whether a choice exists.
    const knownAccounts = programsPending ? null : accounts;
    accountRows = accountChoices(knownAccounts, agent, accountsFailed);
    const selected = accountSelection.render(knownAccounts, agent, accountsFailed);
    accountSelect.replaceChildren();
    for (const choice of accountRows) {
      accountSelect.append(h("option", { value: choice.value }, choice.label));
    }
    accountSelect.value = selected;
    syncSubmitState();
  };

  accountSelect.addEventListener("change", () => {
    accountSelection.pick(accountSelect.value);
    syncSubmitState();
  });
  programSelect.addEventListener("change", renderAccounts);

  const renderPrograms = (): void => {
    const previous = programSelect.value;
    programSelect.replaceChildren();
    for (const choice of programs) {
      programSelect.append(h("option", { value: choice.value }, choice.label));
    }
    // Same rule as the backend picker: keep the user's pick across a project switch
    // when it is still offered, else fall back to the repo default, which always is.
    programSelect.value = programs.some((c) => c.value === previous) ? previous : PROGRAM_REPO_DEFAULT;
    // The account list is scoped to whichever agent the program now names, so it is
    // rebuilt here rather than only on a user change: a project switch that moves
    // the repo default from claude to codex changes the registry without anyone
    // touching the account field.
    renderAccounts();
  };

  // A per-load token: a slow catalog for the project the user just left must not
  // overwrite the choices for the one they just picked. Shared by both catalogs —
  // they are fetched together and are both per-project facts, so one token orders
  // them consistently.
  let loadSeq = 0;
  const loadCatalogsFor = (repoPath: string): void => {
    const seq = ++loadSeq;
    accounts = null;
    programsPending = true;
    accountsFailed = false;
    renderAccounts();

    // The program enum is global, so this is asked even with no project picked —
    // repoPath only sharpens which agent "repo default" resolves to.
    void callbacks
      .loadPrograms(repoPath)
      .then((catalog) => {
        if (seq !== loadSeq) {
          return;
        }
        programsPending = false;
        programCatalog = catalog;
        programs = programChoices(catalog);
        renderPrograms();
      })
      .catch(() => {
        if (seq !== loadSeq) {
          return;
        }
        // Degrade to "repo default" only — the same contract as the backend field:
        // an unreachable catalog costs the user the choice, never the session, since
        // sending no program is exactly what "repo default" means on the wire.
        programsPending = false;
        programCatalog = null;
        programs = programChoices(null);
        renderPrograms();
      });

    // The registry is the daemon host's, but the DEFAULT it reports is the picked
    // project's (#3386), so this rides the same per-load token as the two catalogs:
    // a slow answer for the project the user just left must not preselect an
    // account for the one they just picked. A failed load keeps creation available
    // but explicitly discloses that the daemon default, if any, will apply.
    void callbacks
      .loadAccounts(repoPath)
      .then((registry) => {
        if (seq !== loadSeq) {
          return;
        }
        accounts = registry;
        renderAccounts();
      })
      .catch(() => {
        if (seq !== loadSeq) {
          return;
        }
        accounts = null;
        accountsFailed = true;
        renderAccounts();
      });

    if (repoPath === "") {
      choices = backendChoices(null);
      renderChoices();
      return;
    }
    void callbacks
      .loadBackends(repoPath)
      .then((catalog) => {
        if (seq !== loadSeq) {
          return;
        }
        choices = backendChoices(catalog);
        renderChoices();
      })
      .catch(() => {
        if (seq !== loadSeq) {
          return;
        }
        // Degrade to "repo default" only. The create path is unchanged by an
        // unknown catalog, so this costs the user the choice, not the session.
        choices = backendChoices(null);
        renderChoices();
      });
  };

  projectSelect.addEventListener("change", () => loadCatalogsFor(projectSelect.value));

  const promptArea = h("textarea", { class: "af-input af-textarea", placeholder: "Initial prompt (optional)", rows: 3 });
  promptArea.setAttribute("aria-label", "Initial prompt");

  defaults.body.append(field("Program", programSelect), field("Backend", backendSelect), backendHint,
    accountBlock);
  body.append(field("Title", titleInput), field("Project", projectSelect), field("Prompt", promptArea), accountSlot, defaults.el);

  renderPrograms();
  renderChoices();
  renderAccounts();
  loadCatalogsFor(projectSelect.value);

  // Ask for the autocreate name once, on open. Repo-agnostic (the daemon avoids
  // every live title), so it needs no re-fetch on a project change. A failure is
  // silent: the field keeps its static placeholder and simply requires a typed
  // title, exactly as before this feature.
  void callbacks
    .suggestName()
    .then((name) => {
      if (name !== "") {
        suggestedName = name;
        titleInput.placeholder = name;
      }
    })
    .catch(() => {
      /* no suggestion — leave the static placeholder and the typed-title requirement */
    });

  const card = handle.el.firstElementChild as HTMLElement;
  asForm(card, () => {
    // #2470: an empty field adopts the suggested name (the shadow text). The
    // placeholder already equals suggestedName, so submitting untouched creates
    // exactly the name the user saw.
    const typed = titleInput.value.trim();
    const title = typed !== "" ? typed : suggestedName;
    if (projectSelect.value === "") {
      handle.setError("A project is required.");
      return;
    }
    if (title === "") {
      handle.setError("A title is required.");
      return;
    }
    handle.setError(null);
    callbacks.onSubmit({
      title,
      repoPath: projectSelect.value,
      program: programSelect.value,
      prompt: promptArea.value,
      // REPO_DEFAULT ("") when the user did not choose — createSession then omits
      // `backend` entirely and the repo's config decides (#1933).
      backend: backendSelect.value,
      // AMBIENT_ACCOUNT ("") when the user did not choose — createSession then omits
      // `account` entirely and the daemon applies its default, if any (#3844).
      account: accountSelect.value,
    });
  });

  queueMicrotask(() => titleInput.focus());
  return handle;
}

/** The handoff modal (#2013): pick the agent to continue the session under — the
 *  web half of the TUI's `F`. It collapses the TUI's pick-then-confirm into one
 *  dialog: a web modal already gates the swap behind an explicit submit, and the
 *  explanatory line IS the confirmation the TUI shows after the picker.
 *
 *  The agent list comes from the daemon (loadPrograms), never a list here, and
 *  excludes the running agent — the daemon's same-agent guard would reject it. If
 *  the catalog can't be reached or offers no other agent, Hand off stays disabled
 *  with an explanation, so the modal degrades to "you can't from here" rather than
 *  to a submit that always errors. */
export function handoffModal(
  sessionTitle: string,
  currentAgent: string,
  callbacks: {
    onSubmit: (target: string, account?: string) => void;
    loadAccounts?: () => Promise<AccountsResponse>;
    currentAccount?: string;
    onCancel: () => void;
    loadPrograms: () => Promise<ProgramCatalog>;
  },
): ModalHandle {
  const { handle, body, confirmBtn } = modalChrome({
    title: `Hand off ${sessionTitle}`,
    confirmLabel: "Hand off",
    confirmClass: "af-primary",
    onCancel: callbacks.onCancel,
  });

  let accounts: AccountsResponse = { entries: [], agents: [] };
  let accountsLoaded = !callbacks.loadAccounts;
  const accountSelect = h("select", { class: "af-input" });
  accountSelect.setAttribute("aria-label", "New account");
  const refreshAccounts = (): void => {
    const agent = agentSelect.value;
    const choices = handoffAccountChoices(accounts, agent, agent === currentAgent ? callbacks.currentAccount : "");
    accountSelect.replaceChildren();
    if (agent !== currentAgent) accountSelect.append(h("option", { value: "" }, "Current account selection"));
    for (const choice of choices) accountSelect.append(h("option", { value: choice.value }, choice.label));
    const fallback = accounts.defaults?.[agent];
    if (fallback && choices.some((choice) => choice.value === fallback)) accountSelect.value = fallback;
    accountSelect.disabled = choices.length === 0;
    confirmBtn.disabled = !accountsLoaded || !agent || (agent === currentAgent && !accountSelect.value);
  };
  const agentSelect = h("select", { class: "af-input" });
  agentSelect.setAttribute("aria-label", "New agent");
  // Nothing to pick until the catalog lands; Hand off is disabled until it does.
  confirmBtn.disabled = true;

  const renderChoices = (choices: ProgramChoice[]): void => {
    agentSelect.replaceChildren();
    for (const choice of choices) {
      agentSelect.append(h("option", { value: choice.value }, choice.label));
    }
    confirmBtn.disabled = choices.length === 0;
  };

  body.append(
    field("New agent", agentSelect),
    field("New account", accountSelect),
    h(
      "p",
      { class: "af-modal-text" },
      "Start a new agent with a summary. Keep the worktree and branch.",
    ),
  );

  void callbacks
    .loadPrograms()
    .then((catalog) => {
      const choices = handoffAgentChoices(catalog, currentAgent);
      if (callbacks.loadAccounts && currentAgent) choices.unshift({value: currentAgent, label: currentAgent + " (another account)"});
      renderChoices(choices);
      refreshAccounts();
      if (choices.length === 0) {
        handle.setError("No other agent is available to hand off to.");
      }
    })
    .catch(() => {
      // A handoff needs a concrete target and the web cannot read the enum itself —
      // that is the whole reason ListPrograms is served — so an unreachable catalog
      // means the picker can't be offered. Say so rather than submit an empty `to`.
      renderChoices([]);
      handle.setError("Could not load the agent list. Try again.");
    });

  agentSelect.addEventListener("change", refreshAccounts);
  if (callbacks.loadAccounts) {
    void callbacks.loadAccounts().then((result) => {
      accounts = result; accountsLoaded = true; refreshAccounts();
    }).catch(() => { confirmBtn.disabled = true; handle.setError("Could not load accounts. Try again."); });
  }
  const card = handle.el.firstElementChild as HTMLElement;
  asForm(card, () => {
    const target = agentSelect.value;
    if (target === "") {
      handle.setError("Pick an agent to hand off to.");
      return;
    }
    handle.setError(null);
    if (!accountsLoaded || (target === currentAgent && !accountSelect.value)) {
      handle.setError("Pick another registered account."); return;
    }
    callbacks.onSubmit(target, accountSelect.value);
  });

  queueMicrotask(() => agentSelect.focus());
  return handle;
}

export interface DeletionWorkspace {
  archived: boolean;
  offBox: boolean;
  externalWorktree: boolean;
  branchCreatedByUs: boolean;
}

/** Deletion consequences, selected from the same workspace ownership facts as teardown. */
export function deletionConfirmationBody(opts: DeletionWorkspace): string {
  if (opts.archived && opts.offBox) {
    return "Permanently deletes the session record. Its branch stays published from the archive. Restore instead to use the session again.";
  }
  if (opts.archived && !opts.externalWorktree) {
    return opts.branchCreatedByUs
      ? "Permanently deletes the session, archived worktree and af-created branch. Uncommitted changes and unpushed commits are lost. Restore instead to keep the session."
      : "Permanently deletes the session and archived worktree. Your branch and its commits stay. Uncommitted changes are lost. Restore instead to keep the session.";
  }
  if (opts.offBox) {
    return "Permanently removes the sandbox. Unpushed commits and uncommitted changes are lost. Archive publishes the branch first.";
  }
  if (opts.externalWorktree) {
    return "Permanently deletes the session record and runtime. Your checkout and branch stay.";
  }
  return opts.branchCreatedByUs
    ? "Permanently deletes the session, its af-owned worktree and af-created branch. Uncommitted changes and unpushed commits are lost. Archive to keep them."
    : "Permanently deletes the session and its worktree. Your branch and its commits stay. Uncommitted changes are lost. Archive to keep them.";
}

/** A session-lifecycle confirm modal (kill, archive, or restore). Kill is
 *  destructive; archive/restore are the reversible pair (#1932). */
export function confirmModal(
  opts: { action: "kill" | "archive" | "restore"; sessionTitle: string; isRoot?: boolean; archived: boolean; offBox: boolean; externalWorktree: boolean; branchCreatedByUs: boolean; onConfirm: () => void; onCancel: () => void },
): ModalHandle {
  // Restore is the reverse of archive (#1932): non-destructive, so it reads as a
  // primary (not danger) confirm, mirroring archive's own class. The web routes it
  // through this same confirm chrome as kill/archive so it inherits their busy +
  // error surface; the copy makes true the "you can restore it later" promise the
  // archive confirm already prints.
  const copy = {
    kill: {
      title: `Delete session ${opts.sessionTitle}?`,
      confirmLabel: "Delete session",
      confirmClass: "af-danger",
      body: deletionConfirmationBody(opts),
    },
    archive: {
      title: `Archive ${opts.sessionTitle}?`,
      confirmLabel: "Archive",
      confirmClass: "af-primary",
      body: "Local: move the worktree to the archive. Sandboxes: publish work, then remove the sandbox. Restore anytime.",
    },
    restore: {
      title: `Restore ${opts.sessionTitle}?`,
      confirmLabel: "Restore",
      confirmClass: "af-primary",
      body: "Restore the worktree and agent. Sandboxes push work before replacement; restore refuses if preservation is uncertain.",
    },
  }[opts.action];

  const { handle, body } = modalChrome({
    title: copy.title,
    confirmLabel: copy.confirmLabel,
    confirmClass: copy.confirmClass,
    onCancel: opts.onCancel,
  });

  body.append(h("p", { class: opts.action === "kill" ? "af-modal-text af-modal-danger" : "af-modal-text" }, copy.body));

  let acknowledgment: HTMLInputElement | undefined;
  if (opts.action === "kill" && opts.isRoot) {
    body.append(h("p", { class: "af-modal-text af-modal-danger" },
      `“${opts.sessionTitle}” is the daemon-managed root agent. Deleting it stops scheduled and watch-task delivery to it until it self-heals (usually about two minutes) or you restart the daemon.`));
    acknowledgment = h("input", { type: "checkbox", class: "af-config-check", required: true });
    body.append(h("label", { class: "af-modal-text" }, acknowledgment, " I understand that deleting this root session interrupts task delivery."));
  }

  const card = handle.el.firstElementChild as HTMLElement;
  asForm(card, () => {
    if (acknowledgment && !acknowledgment.checked) {
      handle.setError("Acknowledge the interruption to root task delivery before deleting this session.");
      return;
    }
    handle.setError(null);
    opts.onConfirm();
  });

  if (acknowledgment) {
    const checkbox = acknowledgment;
    const focusAcknowledgment = () => {
      if (card.isConnected) (checkbox.disabled ? card : checkbox).focus({ preventScroll: true });
    };
    // The shared mount hook focuses the card, including a retained failed modal.
    // Root consent starts at its required field; disabled fields fall back to the
    // card's existing tabindex=-1. Initial construction can precede mounting.
    card.addEventListener("focus", () => { if (!checkbox.disabled) focusAcknowledgment(); });
    queueMicrotask(focusAcknowledgment);
  }
  return handle;
}

/** Delete-project confirmation (#1735): archived sessions remain restorable and
 *  the real git repo is untouched, while the durable project registration is
 *  removed. */
export function confirmDeleteProjectModal(
  opts: { projectLabel: string; sessionCount: number; inPlaceCount: number; onConfirm: () => void; onCancel: () => void },
): ModalHandle {
  const word = opts.sessionCount === 1 ? "session" : "sessions";
  const { handle, body } = modalChrome({
    title: `Delete project ${opts.projectLabel}?`,
    confirmLabel: "Delete project",
    confirmClass: "af-primary",
    onCancel: opts.onCancel,
  });

  let message = opts.sessionCount === 0
    ? "No live sessions to archive. Remove the project; the repo stays and you can add it again."
    : `Archive ${opts.sessionCount} ${word} and remove the project. Keep the repo; restore sessions anytime.`;
  if (opts.inPlaceCount > 0) {
    const inPlaceWord = opts.inPlaceCount === 1 ? "session is" : "sessions are";
    message = `${opts.inPlaceCount} in-place ${inPlaceWord} ended permanently and cannot be restored. Their checkouts and branches are kept.`;
    if (opts.sessionCount > 0) message += ` Archive ${opts.sessionCount} regular ${word}; restore those sessions anytime.`;
    message += " Remove the project; the repo stays.";
  }
  if (opts.sessionCount === 0 && opts.inPlaceCount === 0) {
    message += " Archived sessions and tasks stay. Tasks keep the project in the switcher; otherwise, add it again to see archives.";
  }
  body.append(h("p", { class: "af-modal-text" }, message));

  const card = handle.el.firstElementChild as HTMLElement;
  asForm(card, () => {
    handle.setError(null);
    opts.onConfirm();
  });

  return handle;
}

/** The add-project modal (#2456): registers a git checkout as a durable,
 *  sessionless project through the RegisterProject RPC. The path names a
 *  directory ON THE DAEMON HOST (absolute, or ~-prefixed which the daemon
 *  expands) — a browser has no shared cwd to resolve a relative path against, so
 *  the daemon owns resolution and validation. A non-git or unreadable path comes
 *  back as the daemon's actionable error, shown inline; on success the modal
 *  closes and the repo appears in the switcher immediately (the #2456 union: the
 *  derived project list ∪ the daemon's registry), no session required.
 *
 *  #2788 adds a minimal directory picker above the field, because on a phone (or
 *  any browser away from the daemon host) a typed absolute path is close to
 *  unusable. `loadDirectory` is the daemon read it browses with; passing it in
 *  keeps this module free of the API/token layer, exactly as the create modal's
 *  catalogs are passed in. Omit it and the modal degrades to the plain field it
 *  was — which is also what happens if the read fails.
 *
 *  The FIELD stays the single source of truth for the submit: picking a repo
 *  writes its path there rather than registering behind the user's back, so what
 *  gets sent is always the string on screen, and a path the browser cannot reach
 *  is still typeable.
 *
 *  onSubmit is async in index.ts, which drives setBusy/setError around it. */
export function addProjectModal(callbacks: {
  onSubmit: (path: string) => void;
  onCancel: () => void;
  loadDirectory?: (path: string) => Promise<DirectoryListing>;
  errorText?: (e: unknown) => string;
}): ModalHandle {
  const { handle, body, confirmBtn } = modalChrome({
    title: "Add project",
    confirmLabel: "Add project",
    confirmClass: "af-primary",
    onCancel: callbacks.onCancel,
  });

  const pathInput = h("input", {
    type: "text",
    class: "af-input",
    placeholder: "/path/to/repo  or  ~/repo",
    autocomplete: "off",
  });
  pathInput.setAttribute("aria-label", "Repository path");

  const { loadDirectory, errorText } = callbacks;
  let picker: DirectoryPickerHandle | null = null;
  if (loadDirectory && errorText) {
    picker = directoryPicker({
      load: loadDirectory,
      errorText,
      onSelect: (path) => {
        // Fill the field rather than submit: the user sees and can edit exactly
        // what will be registered. Focus moves to the confirm button so Enter
        // finishes the job in one more keystroke on a phone keyboard.
        pathInput.value = path;
        handle.setError(null);
        confirmBtn.focus();
      },
    });
    // NOT field(): that wraps its control in a <label>, and a label holding a
    // grid of buttons is both semantically wrong and a click-target hazard. Same
    // caption styling, plain container.
    body.append(
      h(
        "div",
        { class: "af-modal-field" },
        h("span", { class: "af-modal-label" }, "Browse host"),
        picker.el,
      ),
    );
  }

  body.append(
    field("Repository path", pathInput),
    h(
      "p",
      { class: "af-modal-hint" },
      "Enter an absolute repo path on the daemon host (~ works).",
    ),
  );

  // Clear a stale validation/daemon error as the user edits, so the inline
  // message always reflects the CURRENT input rather than the last rejected path.
  pathInput.addEventListener("input", () => handle.setError(null));

  const card = handle.el.firstElementChild as HTMLElement;
  asForm(card, () => {
    const path = pathInput.value.trim();
    if (path === "") {
      handle.setError("Enter or choose a repo path.");
      return;
    }
    handle.setError(null);
    callbacks.onSubmit(path);
  });

  // With no picker, focus the sole input so the user can type immediately,
  // matching every other input modal (add-task, create-session, …).
  //
  // WITH a picker, focus the DIALOG instead of the input. Not the input, because
  // this feature exists for the phone and autofocusing a text field there raises
  // the virtual keyboard over the very browser the user opened the modal to use.
  // But focus must still land inside the dialog: the modal is opened from the
  // project menu, so leaving focus where it was parks it on the "+ Add project"
  // button BEHIND the overlay, where Tab walks the page underneath and Enter
  // re-triggers the button that opened this (#2800 review). Escape is unaffected
  // either way — index.ts handles it at the document level.
  //
  // The first load starts here, after the modal is built, so a slow filesystem
  // never delays the modal appearing.
  queueMicrotask(() => {
    if (picker) {
      card.focus();
      picker.start();
      return;
    }
    pathInput.focus();
  });
  return handle;
}

/** A labeled field row: a caption above its control. */


/** A friendly project label: the repo's basename with its parent for context. */
export function projectLabel(root: string): string {
  const parts = root.replace(/\/+$/, "").split("/");
  const base = parts[parts.length - 1] || root;
  const parent = parts.length >= 2 ? parts[parts.length - 2] : "";
  return parent ? `${base}  (${parent}/${base})` : base;
}

/** Target-specific task deletion; failures retain the open confirmation. */
export function removeTaskModal(name: string, onConfirm: () => void, onCancel: () => void): ModalHandle {
  const { handle, body } = modalChrome({ title: `Remove ${name}?`, confirmLabel: "Remove", confirmClass: "af-primary", onCancel });
  body.append(h("p", { class: "af-modal-text af-modal-danger" }, "Delete the task and stop future runs. Keep existing sessions."));
  asForm(handle.el.firstElementChild as HTMLElement, onConfirm);
  return handle;
}
