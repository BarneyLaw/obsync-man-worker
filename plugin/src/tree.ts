/**
 * Folder tree over manifest paths, for the panel's file lists.
 *
 * Pure: no Obsidian, no DOM. Folders sort before files, and names sort the way
 * a person reads them ("Lecture 2" before "Lecture 10").
 */

export interface TreeFile<T> {
  name: string;
  path: string;
  value: T;
}

export interface TreeFolder<T> {
  /** "" for the root. */
  name: string;
  path: string;
  folders: TreeFolder<T>[];
  files: TreeFile<T>[];
}

const collator = new Intl.Collator(undefined, { numeric: true, sensitivity: "base" });

export function buildTree<T>(items: readonly T[], pathOf: (item: T) => string): TreeFolder<T> {
  const root: TreeFolder<T> = { name: "", path: "", folders: [], files: [] };
  const folders = new Map<string, TreeFolder<T>>([["", root]]);

  for (const value of items) {
    const path = pathOf(value);
    const parts = path.split("/").filter((p) => p.length > 0);
    const name = parts.pop();
    if (name === undefined) continue;

    let parent = root;
    let prefix = "";
    for (const part of parts) {
      prefix = prefix ? `${prefix}/${part}` : part;
      let next = folders.get(prefix);
      if (!next) {
        next = { name: part, path: prefix, folders: [], files: [] };
        folders.set(prefix, next);
        parent.folders.push(next);
      }
      parent = next;
    }
    parent.files.push({ name, path, value });
  }

  sortTree(root);
  return root;
}

function sortTree<T>(folder: TreeFolder<T>) {
  folder.folders.sort((a, b) => collator.compare(a.name, b.name));
  folder.files.sort((a, b) => collator.compare(a.name, b.name));
  for (const sub of folder.folders) sortTree(sub);
}

/** Every file beneath a folder, at any depth, in display order. */
export function filesUnder<T>(folder: TreeFolder<T>): TreeFile<T>[] {
  const out: TreeFile<T>[] = [];
  for (const sub of folder.folders) out.push(...filesUnder(sub));
  out.push(...folder.files);
  return out;
}
