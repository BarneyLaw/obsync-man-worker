import { ItemView, WorkspaceLeaf, Notice, normalizePath, setIcon } from "obsidian";
import { Manifest } from "../types";
import { Preview, PreviewItem, isPullable } from "../preview";
import { humanBytes } from "../policy";
import { renderSettings, ObsyncPluginLike } from "../settings";
import { Syncer, notifyResult } from "../sync";
import { buildTree, filesUnder, TreeFolder } from "../tree";

export const VIEW_TYPE_OBSYNC = "obsync-panel";

/** Long lists get truncated: a semester is ~2000 entries and the DOM notices. */
const MAX_ROWS = 300;

/**
 * Below the top level, a folder starts expanded only if it holds at most this
 * many files, so a big course opens as an overview rather than a wall of rows.
 */
const AUTO_EXPAND_FILES = 12;

/** What the plugin has to expose for the panel to drive it. */
export type ObsyncHost = ObsyncPluginLike & {
  makeSyncer(opts?: { quiet?: boolean }): Syncer | null;
  pullAll(opts?: { manual?: boolean }): Promise<void>;
  onStatus(cb: (text: string) => void): () => void;
  currentStatus(): string;
};

interface CourseState {
  manifest?: Manifest;
  preview?: Preview;
  error?: string;
  /** Pullable paths currently ticked. Every change is also saved via Syncer.choose. */
  selected: Set<string>;
}

/** A checkbox and the file paths it stands for: one for a file, all beneath it for a folder. */
interface Box {
  box: HTMLInputElement;
  paths: string[];
}

/**
 * The plugin's main surface: a panel in the sidebar, opened by the ribbon icon.
 *
 * Everything lives here -- status, what will be pulled, what was withheld and
 * why, quarantined files, and the setup form -- because this is how the plugin
 * is actually used. The settings tab renders the same form via renderSettings()
 * so the two cannot drift.
 */
export class ObsyncView extends ItemView {
  private courses = new Map<number, CourseState>();
  private syncer: Syncer | null = null;
  private loading = false;
  private unsubscribe?: () => void;
  private statusEl?: HTMLElement;
  // Which sections the user opened or closed, so a reload does not undo it.
  private setupOpen = false;
  private collapsedCourses = new Set<number>();
  private openWithheld = new Set<number>();

  constructor(leaf: WorkspaceLeaf, private plugin: ObsyncHost) {
    super(leaf);
  }

  getViewType() { return VIEW_TYPE_OBSYNC; }
  getDisplayText() { return "obsync"; }
  getIcon() { return "cloud-download"; }

  async onOpen() {
    this.unsubscribe = this.plugin.onStatus((t) => this.statusEl?.setText(t));
    this.render();
    // Only reach for the network if there is somewhere to reach.
    if (this.plugin.settings.baseUrl && this.plugin.settings.courses.length > 0) {
      await this.refresh();
    }
  }

  async onClose() {
    this.unsubscribe?.();
  }

  /** Re-fetch every configured course's manifest, then re-render. */
  async refresh() {
    // Quiet: the panel explains an incomplete setup itself.
    const s = this.plugin.makeSyncer({ quiet: true });
    this.syncer = s;
    if (!s) {
      this.render();
      return;
    }
    this.loading = true;
    this.render();

    this.courses.clear();
    for (const id of this.plugin.settings.courses) {
      const st: CourseState = { selected: new Set() };
      try {
        const m = await s.fetchManifest(id);
        if (!m) {
          st.error = "no manifest published for this course yet";
        } else {
          st.manifest = m;
          // Move files into this course's folder first, then check the vault,
          // not just the record: a file deleted since it was pulled must show
          // up as pullable again.
          await s.migrateCourse(m);
          st.preview = s.previewCourse(m, await s.missingFiles(m));
          st.selected = s.selection(m, st.preview);
        }
      } catch (e) {
        // One unreachable course must not blank the whole panel.
        st.error = String(e);
      }
      this.courses.set(id, st);
    }

    this.loading = false;
    this.render();
  }

  private render() {
    const root = this.containerEl.children[1] as HTMLElement | undefined;
    if (!root) return;
    root.empty();
    root.addClass("obsync-view");

    this.renderHeader(root);
    this.renderBody(root.createDiv());
    this.renderConflicts(root);
    this.renderSetup(root);
  }

  private renderHeader(root: HTMLElement) {
    const head = root.createDiv({ cls: "obsync-head" });
    this.statusEl = head.createDiv({ cls: "obsync-status", text: this.plugin.currentStatus() });

    const bar = head.createDiv({ cls: "obsync-actions" });
    const pull = bar.createEl("button", { text: "Pull now", cls: "mod-cta" });
    pull.setAttr("aria-label", "Pull everything ticked, in every course");
    pull.addEventListener("click", () => {
      void (async () => {
        await this.plugin.pullAll({ manual: true });
        await this.refresh();
      })();
    });
    const refresh = bar.createEl("button", { text: "Refresh" });
    refresh.addEventListener("click", () => void this.refresh());
  }

  private renderBody(body: HTMLElement) {
    if (!this.plugin.settings.baseUrl || this.plugin.settings.courses.length === 0) {
      body.createEl("p", {
        cls: "obsync-muted",
        text: "Set a store URL and at least one course ID in Setup below to get started.",
      });
      return;
    }
    if (this.loading) {
      body.createEl("p", { cls: "obsync-muted", text: "Loading manifests..." });
      return;
    }
    if (this.courses.size === 0) {
      body.createEl("p", { cls: "obsync-muted", text: "Nothing loaded. Press Refresh." });
      return;
    }
    for (const [id, st] of this.courses) this.renderCourse(body, id, st);
  }

  private renderCourse(parent: HTMLElement, id: number, st: CourseState) {
    const details = parent.createEl("details", { cls: "obsync-course" });
    details.open = !this.collapsedCourses.has(id);
    details.addEventListener("toggle", () => {
      if (details.open) this.collapsedCourses.delete(id);
      else this.collapsedCourses.add(id);
    });
    const name = st.manifest?.course_name ?? `Course ${id}`;
    details.createEl("summary", { text: name });

    if (st.error) {
      details.createEl("p", { cls: "obsync-error", text: st.error });
      return;
    }
    const p = st.preview;
    const m = st.manifest;
    if (!p || !m) return;

    const items = p.items.filter(isPullable);
    const counts = { new: 0, changed: 0, missing: 0 };
    for (const i of items) {
      if (i.action === "download") counts.new++;
      else if (i.action === "update") counts.changed++;
      else counts.missing++;
    }
    const parts = [`${counts.new} new`, `${counts.changed} changed`];
    if (counts.missing > 0) parts.push(`${counts.missing} missing from the vault`);
    details.createEl("p", {
      cls: "obsync-muted",
      text: `${parts.join(" · ")} · ${p.alreadyHave} up to date`,
    });

    if (items.length === 0) {
      details.createEl("p", { cls: "obsync-muted", text: "Everything here is current." });
    } else {
      details.createEl("p", {
        cls: "obsync-muted",
        text:
          "Only ticked files are pulled, and only when you press a pull button. " +
          "Automatic pulls just refresh files already in your vault.",
      });
      this.renderTree(details, items, { id, st, manifest: m });

      const foot = details.createDiv({ cls: "obsync-actions" });
      const count = foot.createSpan({ cls: "obsync-muted obsync-count" });
      count.dataset.course = String(id);
      const go = foot.createEl("button", { text: "Pull selected", cls: "mod-cta" });
      go.addEventListener("click", () => void this.pullCourse(st));
      this.updateCount(id);
    }

    this.renderWithheld(details, id, p);
  }

  private renderWithheld(parent: HTMLElement, id: number, p: Preview) {
    const withheld = p.items.filter((i) => !isPullable(i) && i.action !== "have");
    if (withheld.length === 0) return;

    const details = parent.createEl("details", { cls: "obsync-withheld" });
    details.open = this.openWithheld.has(id);
    details.addEventListener("toggle", () => {
      if (details.open) this.openWithheld.add(id);
      else this.openWithheld.delete(id);
    });
    details.createEl("summary", { text: `Not included (${withheld.length})` });
    details.createEl("p", {
      cls: "obsync-muted",
      text:
        "Canvas has these and they were deliberately not pulled. Local rules are " +
        "reversible in Setup; worker rules need a change on the server. Pending " +
        "files were left for later by a scoped pull on the worker and arrive with " +
        "its next full pull.",
    });
    this.renderTree(details, withheld);
  }

  /**
   * Items as a collapsible folder tree.
   *
   * With `selection`, every file and folder gets a checkbox, plus one for the
   * whole course. Ticking or unticking a folder applies to everything beneath
   * it, and every box, files included, is redrawn from the selection so a
   * parent and its children can never disagree. A folder shows a partial state
   * when only some of it is ticked. Without `selection`, each file shows why it
   * is withheld.
   */
  private renderTree(
    parent: HTMLElement,
    items: PreviewItem[],
    selection?: { id: number; st: CourseState; manifest: Manifest },
  ) {
    const list = parent.createDiv({ cls: "obsync-list obsync-tree" });
    const boxes: Box[] = [];

    const redraw = () => {
      if (!selection) return;
      for (const { box, paths } of boxes) {
        const n = paths.filter((path) => selection.st.selected.has(path)).length;
        box.checked = n > 0 && n === paths.length;
        box.indeterminate = n > 0 && n < paths.length;
      }
      this.updateCount(selection.id);
    };
    const select = (paths: string[], on: boolean) => {
      if (!selection) return;
      for (const path of paths) {
        if (on) selection.st.selected.add(path);
        else selection.st.selected.delete(path);
      }
      redraw();
      // Remember the choice, so an untick holds through restarts and every
      // automatic pull.
      void this.syncer?.choose(selection.manifest, paths, on);
    };
    const checkbox = (row: HTMLElement, paths: string[]) => {
      const box = row.createEl("input", { type: "checkbox" });
      boxes.push({ box, paths });
      box.addEventListener("change", () => select(paths, box.checked));
    };

    if (selection) {
      const all = list.createDiv({ cls: "obsync-tree-row obsync-tree-all" });
      all.createSpan({ cls: "obsync-tree-caret" });
      checkbox(all, items.map((i) => i.entry.path));
      all.createSpan({ cls: "obsync-tree-name", text: "All files" });
    }

    let rendered = 0;
    const renderFolder = (el: HTMLElement, folder: TreeFolder<PreviewItem>, depth: number) => {
      for (const sub of folder.folders) {
        if (rendered >= MAX_ROWS) return;
        const files = filesUnder(sub);
        const node = el.createDiv({ cls: "obsync-tree-folder" });
        const row = node.createDiv({ cls: "obsync-tree-row" });
        const caret = row.createSpan({ cls: "obsync-tree-caret" });

        if (selection) checkbox(row, files.map((f) => f.path));
        setIcon(row.createSpan({ cls: "obsync-tree-icon" }), "folder");
        const label = row.createDiv({ cls: "obsync-label obsync-tree-toggle" });
        label.createSpan({ cls: "obsync-path obsync-tree-name", text: sub.name });
        const bytes = files.reduce((n, f) => n + f.value.entry.size, 0);
        label.createSpan({
          cls: "obsync-muted",
          text: ` ${files.length} ${files.length === 1 ? "file" : "files"}, ${humanBytes(bytes)}`,
        });

        const children = node.createDiv({ cls: "obsync-tree-children" });
        let open = depth === 0 || files.length <= AUTO_EXPAND_FILES;
        const apply = () => {
          setIcon(caret, open ? "chevron-down" : "chevron-right");
          children.toggle(open);
          row.setAttr("aria-expanded", String(open));
        };
        const flip = () => {
          open = !open;
          apply();
        };
        caret.addEventListener("click", flip);
        label.addEventListener("click", flip);
        apply();

        renderFolder(children, sub, depth + 1);
      }

      for (const file of folder.files) {
        if (rendered >= MAX_ROWS) return;
        rendered++;
        const item = file.value;
        const row = el.createDiv({ cls: "obsync-tree-row obsync-tree-file" });
        // An empty caret keeps files aligned with their sibling folders.
        row.createSpan({ cls: "obsync-tree-caret" });
        if (selection) checkbox(row, [file.path]);
        setIcon(row.createSpan({ cls: "obsync-tree-icon" }), "file");
        const label = row.createDiv({ cls: "obsync-label" });
        label.createSpan({ cls: "obsync-path", text: file.name });
        label.createSpan({ cls: "obsync-muted", text: ` ${detailFor(item, selection !== undefined)}` });
        const tag = tagFor(item);
        if (tag) {
          row.createSpan({ cls: `obsync-tag obsync-tag-${tag.kind}`, text: tag.text })
            .setAttr("aria-label", tag.hint);
        }
      }
    };

    renderFolder(list, buildTree(items, (i) => i.entry.path), 0);
    redraw();
    overflow(parent, items.length);
  }

  private updateCount(id: number) {
    const st = this.courses.get(id);
    if (!st?.preview) return;
    const bytes = st.preview.items
      .filter((i) => isPullable(i) && st.selected.has(i.entry.path))
      .reduce((n, i) => n + i.entry.size, 0);
    const el = this.containerEl.querySelector<HTMLElement>(
      `.obsync-count[data-course="${id}"]`,
    );
    el?.setText(`${st.selected.size} ticked, ${humanBytes(bytes)}`);
  }

  private async pullCourse(st: CourseState) {
    const s = this.syncer ?? this.plugin.makeSyncer();
    if (!s || !st.manifest) return;
    // The ticks are already saved with Syncer.choose, so a manual pull writes
    // exactly what the panel shows as ticked.
    try {
      notifyResult(await s.syncCourse(st.manifest, { mode: "manual" }));
    } catch (e) {
      new Notice(`obsync: ${String(e)}`);
    }
    await this.refresh();
  }

  /** Files quarantined because they were edited locally. */
  private renderConflicts(root: HTMLElement) {
    const details = root.createEl("details", { cls: "obsync-conflicts" });
    const summary = details.createEl("summary", { text: "Conflicts" });
    const list = details.createDiv({ cls: "obsync-list" });

    void (async () => {
      const dir = normalizePath(this.plugin.settings.conflictFolder);
      const found = await walk(this.plugin, dir);
      summary.setText(`Conflicts (${found.length})`);
      list.empty();
      if (found.length === 0) {
        list.createEl("p", { cls: "obsync-muted", text: "None. Your edits are safe." });
        return;
      }
      details.setAttr("open", "");
      for (const path of found.slice(0, MAX_ROWS)) {
        const row = list.createDiv({ cls: "obsync-row" });
        const label = row.createDiv({ cls: "obsync-label" });
        label.createSpan({ cls: "obsync-path", text: path.slice(dir.length + 1) });
        const open = row.createEl("button", { text: "Open" });
        open.addEventListener("click", () => {
          void this.app.workspace.openLinkText(path, "", true);
        });
        const discard = row.createEl("button", { text: "Discard" });
        discard.addEventListener("click", () => {
          void (async () => {
            await this.app.vault.adapter.remove(path);
            new Notice("obsync: discarded your copy; the mirrored version stands");
            this.render();
          })();
        });
      }
      overflow(list, found.length);
    })();
  }

  private renderSetup(root: HTMLElement) {
    const details = root.createEl("details", { cls: "obsync-setup" });
    details.open = this.setupOpen;
    details.addEventListener("toggle", () => {
      this.setupOpen = details.open;
    });
    details.createEl("summary", { text: "Setup" });
    const box = details.createDiv();
    // The same form the settings tab renders, so the two cannot drift. It is
    // the complete form, which is why there is no "open full settings" escape
    // hatch here -- that would only buy a dependency on a private API.
    renderSettings(box, this.plugin, () => void this.refresh());
  }
}

function overflow(parent: HTMLElement, total: number) {
  if (total > MAX_ROWS) {
    parent.createEl("p", { cls: "obsync-muted", text: `... and ${total - MAX_ROWS} more.` });
  }
}

/**
 * The muted text beside a file. Pullable rows show size plus any worker note,
 * since the tag already says new, changed or missing; withheld rows show why.
 */
function detailFor(item: PreviewItem, pullable: boolean): string {
  if (!pullable) return item.reason;
  const note = item.entry.reason ? ` · ${item.entry.reason}` : "";
  return `${humanBytes(item.entry.size)}${note}`;
}

function tagFor(i: PreviewItem): { kind: string; text: string; hint: string } | null {
  switch (i.action) {
    case "download": return { kind: "new", text: "new", hint: "Not in this vault yet" };
    case "update": return { kind: "changed", text: "changed", hint: "Canvas has a newer version than the copy in this vault" };
    case "restore": return { kind: "missing", text: "missing", hint: "Pulled before, then deleted from this vault. Tick it to put it back" };
    case "skip-local": return { kind: "withheld", text: "your rules", hint: "Excluded by your local rules in Setup" };
    case "skip-worker": return { kind: "withheld", text: "worker", hint: "Excluded by the worker's rules" };
    case "deferred": return { kind: "withheld", text: "pending", hint: "Arrives with the worker's next full pull" };
    case "locked": return { kind: "withheld", text: "locked", hint: "Not released in Canvas yet" };
    case "unavailable": return { kind: "withheld", text: "failed", hint: "The worker could not fetch this file" };
    default: return null;
  }
}

/** Every file under a folder, recursively. Returns [] if the folder is absent. */
async function walk(plugin: ObsyncHost, dir: string): Promise<string[]> {
  const adapter = plugin.app.vault.adapter;
  if (!dir || !(await adapter.exists(dir))) return [];
  const out: string[] = [];
  const queue = [dir];
  while (queue.length > 0) {
    const cur = queue.pop()!;
    try {
      const listed = await adapter.list(cur);
      out.push(...listed.files);
      queue.push(...listed.folders);
    } catch {
      // A folder that vanished mid-walk is not worth failing the panel over.
    }
  }
  return out.sort();
}
