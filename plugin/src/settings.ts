import { App, PluginSettingTab, Setting, Plugin } from "obsidian";
import { Policy, validate } from "./policy";

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

export type ObsyncPluginLike = Plugin & {
  settings: ObsyncSettings;
  save(): Promise<void>;
  rescheduleSync(): void;
};

const COURSES_DESC = "Comma-separated Canvas course IDs, e.g. 12345, 67890.";
const RULES_DESC =
  "Applied to the manifest locally. Reversible: changing these never needs a refetch.";

/**
 * The configuration form, rendered into whatever container it is given.
 *
 * Both the settings tab and the sidebar panel's Setup section call this, so the
 * two surfaces cannot drift apart. `Setting` only needs an HTMLElement, so
 * nothing here is tab-specific.
 *
 * @param onCoursesChanged - lets the panel re-fetch when the course list moves.
 */
export function renderSettings(
  containerEl: HTMLElement,
  plugin: ObsyncPluginLike,
  onCoursesChanged?: () => void,
) {
  new Setting(containerEl).setName("Store").setHeading();

  new Setting(containerEl)
    .setName("Store URL")
    .setDesc(
      "Read-only endpoint for the obsync bucket. Prefer an endpoint already " +
      "protected at the network layer (Tailscale, auth proxy) so no credentials " +
      "are stored in the vault.",
    )
    .addText((t) =>
      t
        .setPlaceholder("https://obsync.tailnet.ts.net")
        .setValue(plugin.settings.baseUrl)
        .onChange(async (v) => {
          plugin.settings.baseUrl = v.trim();
          await plugin.save();
        }),
    );

  new Setting(containerEl)
    .setName("Bucket")
    .setDesc("Leave empty if the URL already points at the bucket root.")
    .addText((t) =>
      t.setValue(plugin.settings.bucket).onChange(async (v) => {
        plugin.settings.bucket = v.trim();
        await plugin.save();
      }),
    );

  // Without this the plugin has nothing to pull and no way to be told what to
  // pull. It is the one setting that cannot be defaulted.
  const courses = new Setting(containerEl).setName("Course IDs").setDesc(COURSES_DESC);
  courses.addText((t) =>
    t
      .setPlaceholder("12345, 67890")
      .setValue(plugin.settings.courses.join(", "))
      .onChange(async (v) => {
        const { ids, bad } = parseCourseIds(v);
        courses.setDesc(
          bad.length > 0
            ? `Not a course ID: ${bad.join(", ")}. Use comma-separated numbers.`
            : COURSES_DESC,
        );
        courses.descEl.toggleClass("obsync-error", bad.length > 0);
        plugin.settings.courses = ids;
        await plugin.save();
        onCoursesChanged?.();
      }),
  );

  new Setting(containerEl).setName("Folders").setHeading();

  new Setting(containerEl)
    .setName("Target folder")
    .setDesc(
      "Keep the mirror in its own top-level folder. Hundreds of PDFs will " +
      "trigger an Obsidian reindex, so add this folder to Excluded Files in " +
      "Obsidian's own settings if search gets noisy.",
    )
    .addText((t) =>
      t.setValue(plugin.settings.targetFolder).onChange(async (v) => {
        plugin.settings.targetFolder = v.trim();
        await plugin.save();
      }),
    );

  new Setting(containerEl)
    .setName("Trash folder")
    .setDesc("Where files go when Canvas removes them. Never hard deleted.")
    .addText((t) =>
      t.setValue(plugin.settings.trashFolder).onChange(async (v) => {
        plugin.settings.trashFolder = v.trim();
        await plugin.save();
      }),
    );

  new Setting(containerEl)
    .setName("Conflict folder")
    .setDesc(
      "Where a locally-edited file goes instead of being overwritten. " +
      "One-way sync is not a licence to destroy local data.",
    )
    .addText((t) =>
      t.setValue(plugin.settings.conflictFolder).onChange(async (v) => {
        plugin.settings.conflictFolder = v.trim();
        await plugin.save();
      }),
    );

  new Setting(containerEl).setName("Sync").setHeading();

  new Setting(containerEl)
    .setName("Sync interval (minutes)")
    .setDesc("Minimum 5. Takes effect immediately.")
    .addText((t) =>
      t.setValue(String(plugin.settings.syncIntervalMinutes)).onChange(async (v) => {
        plugin.settings.syncIntervalMinutes = Math.max(5, Number(v) || 60);
        await plugin.save();
        // Otherwise the old interval keeps firing until Obsidian restarts.
        plugin.rescheduleSync();
      }),
    );

  // TODO: rule editor UI. Until then, JSON textarea. The engine is already
  // correct and shared with Go, so this is presentation only.
  const rules = new Setting(containerEl)
    .setName("Exclusion rules (JSON)")
    .setDesc(RULES_DESC);
  rules.addTextArea((t) =>
    t.setValue(JSON.stringify(plugin.settings.policy, null, 2)).onChange(async (v) => {
      let parsed: Policy;
      try {
        parsed = JSON.parse(v) as Policy;
      } catch {
        rules.setDesc("Invalid JSON. The previous rules are still in effect.");
        rules.descEl.addClass("obsync-error");
        return;
      }
      // Parsing is not enough: an empty match silently swallows everything,
      // which is what `default` is for. Reject it here rather than at preview.
      const err = validate(parsed);
      if (err) {
        rules.setDesc(`Invalid rules: ${err}. The previous rules are still in effect.`);
        rules.descEl.addClass("obsync-error");
        return;
      }
      rules.setDesc(RULES_DESC);
      rules.descEl.removeClass("obsync-error");
      plugin.settings.policy = parsed;
      await plugin.save();
      onCoursesChanged?.();
    }),
  );
}

export class ObsyncSettingTab extends PluginSettingTab {
  constructor(app: App, private plugin: ObsyncPluginLike) {
    super(app, plugin);
  }

  display(): void {
    this.containerEl.empty();
    renderSettings(this.containerEl, this.plugin);
  }
}

/** Split on commas/whitespace, keep the positive integers, report the rest. */
export function parseCourseIds(raw: string): { ids: number[]; bad: string[] } {
  const ids: number[] = [];
  const bad: string[] = [];
  for (const tok of raw.split(/[,\s]+/).filter((s) => s.length > 0)) {
    const n = Number(tok);
    if (Number.isInteger(n) && n > 0) {
      if (!ids.includes(n)) ids.push(n);
    } else {
      bad.push(tok);
    }
  }
  return { ids, bad };
}
