import { App, Modal, Setting } from "obsidian";
import { Preview } from "../preview";
import { humanBytes } from "../policy";

/**
 * "What will be pulled" before pulling, with per-file opt-out.
 *
 * This exists as a pure local computation because the manifest catalogues
 * SKIPPED files too, not just fetched ones. That one schema decision is what
 * turns this screen from an API design problem into a rendering problem.
 */
export class PreviewModal extends Modal {
  private excluded = new Set<string>();

  constructor(app: App, private p: Preview, private onConfirm: (selected: Set<string>) => void) {
    super(app);
  }

  onOpen() {
    const { contentEl } = this;
    contentEl.createEl("h3", { text: "Pull preview" });
    contentEl.createEl("p", {
      text:
        `${this.p.toDownload} files (${humanBytes(this.p.bytesToDownload)}) to download, ` +
        `${this.p.alreadyHave} already current, ${this.p.skippedLocal} excluded by your rules, ` +
        `${this.p.skippedWorker} not in the store, ${this.p.locked} locked in Canvas.`,
    });

    const list = contentEl.createDiv({ cls: "obsync-preview-list" });
    for (const item of this.p.items) {
      if (item.action !== "download" && item.action !== "update") continue;
      new Setting(list)
        .setName(item.entry.path)
        .setDesc(item.reason)
        .addToggle((t) =>
          t.setValue(true).onChange((on) => {
            if (on) this.excluded.delete(item.entry.path);
            else this.excluded.add(item.entry.path);
          }),
        );
    }

    // Users need to know why a file they can see in Canvas is not here.
    // "It silently did not appear" is the worst possible answer.
    const withheld = this.p.items.filter(
      (i) => i.action === "skip-worker" || i.action === "skip-local" || i.action === "locked",
    );
    if (withheld.length) {
      contentEl.createEl("h4", { text: "Not included" });
      const ul = contentEl.createEl("ul");
      for (const w of withheld.slice(0, 50)) {
        ul.createEl("li", { text: `${w.entry.path} — ${w.reason}` });
      }
      if (withheld.length > 50) {
        ul.createEl("li", { text: `…and ${withheld.length - 50} more` });
      }
    }

    new Setting(contentEl).addButton((b) =>
      b.setButtonText("Pull").setCta().onClick(() => {
        const selected = new Set<string>();
        for (const i of this.p.items) {
          if ((i.action === "download" || i.action === "update") && !this.excluded.has(i.entry.path)) {
            selected.add(i.entry.path);
          }
        }
        this.close();
        this.onConfirm(selected);
      }),
    );
  }

  onClose() {
    this.contentEl.empty();
  }
}
