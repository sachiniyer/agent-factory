/** Retain row nodes across snapshots; only changed values reach the renderer. */
export class KeyedRows<T, N> {
  private rows = new Map<string, { signature: string; node: N }>();

  reconcile(
    values: readonly T[],
    key: (value: T) => string,
    signature: (value: T) => string,
    render: (value: T, previous?: N) => N,
    dispose: (key: string) => void,
  ): N[] {
    const keep = new Set<string>();
    const nodes = values.map(value => {
      const id = key(value);
      keep.add(id);
      const sig = signature(value);
      const previous = this.rows.get(id);
      if (previous?.signature === sig) return previous.node;
      const node = render(value, previous?.node);
      this.rows.set(id, { signature: sig, node });
      return node;
    });
    for (const id of this.rows.keys()) {
      if (!keep.has(id)) {
        dispose(id);
        this.rows.delete(id);
      }
    }
    return nodes;
  }
}

/** Insert/move only where order differs, without detaching unchanged children. */
export function orderChildren(parent: HTMLElement, children: readonly HTMLElement[]): void {
  const wanted = new Set(children);
  let cursor = parent.firstElementChild;
  while (cursor) {
    const next = cursor.nextElementSibling;
    if (!wanted.has(cursor as HTMLElement)) cursor.remove();
    cursor = next;
  }
  cursor = parent.firstElementChild;
  for (const child of children) {
    if (child === cursor) cursor = cursor.nextElementSibling;
    else parent.insertBefore(child, cursor);
  }
  while (cursor) {
    const next = cursor.nextElementSibling;
    cursor.remove();
    cursor = next;
  }
}
