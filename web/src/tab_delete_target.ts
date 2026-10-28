import { tabIdentity, tabRealId } from "./ui.js";

type TabTarget = { id?: string; name: string; kind: number };

/** Retain consent across ordinal changes while the deletion dialog is open. */
export function captureTabDeleteTarget(target: TabTarget): (tabs: TabTarget[]) => number {
  const identity = tabIdentity(target);
  const legacy = tabRealId(target) === "";
  // Names can be reused. Without a daemon ID, only the original projection
  // object carries consent; a newer snapshot must ask again.
  return tabs => tabs.findIndex(tab => tabIdentity(tab) === identity && (!legacy || tab === target));
}
