import { App, PluginSettingTab, Setting, Plugin, setIcon } from "obsidian";
import { Policy, validate } from "./policy";
import { CourseCheck, parseCourseIds, sameIds } from "./courses";

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
  /** Look each course ID up in the store. */
  checkCourses(ids: number[]): Promise<CourseCheck[]>;
  /** The last lookup, so its report survives the panel re-rendering. */
  courseCheck?: { ids: number[]; results: CourseCheck[] };
};

const COURSES_DESC =
  "Comma-separated numeric Canvas course IDs, e.g. 93794. The store is keyed by ID, " +
  "not course code: run obsync ls or obsync-worker courses to list them.";
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
      "are stored in the vault. For local development, run obsync serve and " +
      "use http://127.0.0.1:8765.",
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
    .setDesc(
      "Path segment between the URL and the store keys. Leave empty if the URL " +
      "already points at the bucket root; obsync serve accepts either.",
    )
    .addText((t) =>
      t.setValue(plugin.settings.bucket).onChange(async (v) => {
        plugin.settings.bucket = v.trim();
        await plugin.save();
      }),
    );

  // Without this the plugin has nothing to pull and no way to be told what to
  // pull. It is the one setting that cannot be defaulted.
  //
  // Typing only checks the format, quietly: nothing is saved, nothing reloads,
  // no notices. Saving on every keystroke reloaded the whole panel, which
  // closed this form and raised "add a course ID" while the field was empty.
  // Enter, or leaving the field, saves the list and looks each ID up.
  const courses = new Setting(containerEl).setName("Course IDs").setDesc(COURSES_DESC);
  const report = containerEl.createDiv({ cls: "obsync-course-report" });
  const cached = plugin.courseCheck;
  if (cached && sameIds(cached.ids, plugin.settings.courses)) {
    renderCourseReport(report, cached.results, []);
  }

  courses.addText((t) => {
    t.setPlaceholder("93794, 77826").setValue(plugin.settings.courses.join(", "));
    t.onChange((v) => {
      const { bad } = parseCourseIds(v);
      courses.setDesc(
        bad.length > 0 ? `Not a course ID: ${bad.join(", ")}. Use numbers separated by commas.` : COURSES_DESC,
      );
      courses.descEl.toggleClass("obsync-error", bad.length > 0);
    });
    t.inputEl.addEventListener("change", () => {
      void (async () => {
        const { ids, bad } = parseCourseIds(t.getValue());
        const changed = !sameIds(ids, plugin.settings.courses);
        if (changed) {
          plugin.settings.courses = ids;
          await plugin.save();
        }
        report.empty();
        if (ids.length === 0) {
          plugin.courseCheck = undefined;
          renderCourseReport(report, [], bad);
        } else {
          report.createDiv({ cls: "obsync-muted", text: "Checking the store..." });
          const results = await plugin.checkCourses(ids);
          plugin.courseCheck = { ids, results };
          renderCourseReport(report, results, bad);
        }
        // Reload the panel once, and only if the list really changed.
        if (changed) onCoursesChanged?.();
      })();
    });
  });

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
  // Valid rules are saved as you type, but the panel reloads only once you
  // leave the field, for the same reason as the course IDs.
  let rulesChanged = false;
  rules.addTextArea((t) => {
    t.inputEl.addEventListener("change", () => {
      if (!rulesChanged) return;
      rulesChanged = false;
      onCoursesChanged?.();
    });
    return t.setValue(JSON.stringify(plugin.settings.policy, null, 2)).onChange(async (v) => {
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
      rulesChanged = true;
    });
  });
}

/** One line per course ID: found with its name, not found, or not checkable. */
function renderCourseReport(el: HTMLElement, results: CourseCheck[], bad: string[]) {
  el.empty();
  const line = (icon: string, cls: string, text: string) => {
    const row = el.createDiv({ cls: `obsync-course-report-row ${cls}` });
    setIcon(row.createSpan({ cls: "obsync-course-report-icon" }), icon);
    row.createSpan({ text });
  };
  for (const r of results) {
    switch (r.status) {
      case "found":
        line("check", "obsync-ok", `${r.id}: ${r.name}`);
        break;
      case "missing":
        line("x", "obsync-error",
          `${r.id}: not in the store. Check the ID, or run the worker for this course first.`);
        break;
      case "error":
        line("alert-triangle", "obsync-warning", `${r.id}: could not check (${r.message})`);
        break;
    }
  }
  for (const b of bad) line("x", "obsync-error", `"${b}" is not a course ID`);
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
