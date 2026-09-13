import { App, PluginSettingTab, Setting, Plugin } from "obsidian";
import { Policy } from "./policy";

export interface ObsyncSettings {
  baseUrl: string;
  bucket: string;
  courses: number[];
  targetFolder: string;
  trashFolder: string;
  conflictFolder: string;
  syncIntervalMinutes: number;
  /** CONSUMER rules: reversible, per-device, so they can be aggressive. */
  policy: Policy;
}

export const DEFAULT_SETTINGS: ObsyncSettings = {
  baseUrl: "",
  bucket: "obsync",
  courses: [],
  targetFolder: "Canvas",
  trashFolder: "Canvas/_trash",
  conflictFolder: "Canvas/_conflicts",
  syncIntervalMinutes: 60,
  policy: {
    version: 1,
    default: "include",
    rules: [
      // Mobile-friendly default. Reversible with a checkbox, which is exactly
      // why aggressive filtering belongs here and not in the worker.
      { name: "no-huge", priority: 10, action: "skip", match: { min_size: 50 * 1024 * 1024 } },
    ],
  },
};

export class ObsyncSettingTab extends PluginSettingTab {
  constructor(app: App, private plugin: Plugin & { settings: ObsyncSettings; save(): Promise<void> }) {
    super(app, plugin);
  }

  display(): void {
    const { containerEl } = this;
    containerEl.empty();

    new Setting(containerEl)
      .setName("Store URL")
      .setDesc(
        "Read-only endpoint for the obsync bucket. Prefer an endpoint already " +
        "protected at the network layer (Tailscale, auth proxy) so no credentials " +
        "are stored in the vault.",
      )
      .addText((t) =>
        t.setValue(this.plugin.settings.baseUrl).onChange(async (v) => {
          this.plugin.settings.baseUrl = v.trim();
          await this.plugin.save();
        }),
      );

    new Setting(containerEl)
      .setName("Target folder")
      .setDesc(
        "Keep the mirror in its own top-level folder. Hundreds of PDFs will " +
        "trigger an Obsidian reindex, so add this folder to Excluded Files in " +
        "Obsidian's own settings if search gets noisy.",
      )
      .addText((t) =>
        t.setValue(this.plugin.settings.targetFolder).onChange(async (v) => {
          this.plugin.settings.targetFolder = v.trim();
          await this.plugin.save();
        }),
      );

    new Setting(containerEl)
      .setName("Sync interval (minutes)")
      .addText((t) =>
        t.setValue(String(this.plugin.settings.syncIntervalMinutes)).onChange(async (v) => {
          this.plugin.settings.syncIntervalMinutes = Math.max(5, Number(v) || 60);
          await this.plugin.save();
        }),
      );

    // TODO: rule editor UI. Until then, JSON textarea. The engine is already
    // correct and shared with Go, so this is presentation only.
    new Setting(containerEl)
      .setName("Exclusion rules (JSON)")
      .setDesc("Applied to the manifest locally. Reversible: changing these never needs a refetch.")
      .addTextArea((t) =>
        t.setValue(JSON.stringify(this.plugin.settings.policy, null, 2)).onChange(async (v) => {
          try {
            this.plugin.settings.policy = JSON.parse(v) as Policy;
            await this.plugin.save();
          } catch {
            /* leave the previous value in place until the JSON parses */
          }
        }),
      );
  }
}
