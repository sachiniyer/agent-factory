/** Rebuild live project counts without dropping keyboard focus with the old row. */
export function replaceProjectMenuChildren(menu: HTMLElement, children: HTMLElement[], fallback: HTMLElement): void {
  const active = menu.ownerDocument.activeElement as HTMLElement | null;
  const key = active && menu.contains(active) ? active.dataset.projectFocus : undefined;
  menu.replaceChildren(...children);
  if (key === undefined) return;
  if (menu.hidden) {
    fallback.focus({ preventScroll: true });
    return;
  }
  const replacement = Array.from(menu.querySelectorAll<HTMLElement>("[data-project-focus]"))
    .find(control => control.dataset.projectFocus === key && !(control as HTMLButtonElement).disabled);
  (replacement ?? fallback).focus({ preventScroll: true });
}
