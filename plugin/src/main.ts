import { Plugin, Notice } from "obsidian";
import { ObsyncSettings, DEFAULT_SETTINGS, ObsyncSettingTab } from "./settings";
import { LocalState, loadState, saveState, emptyState } from "./state";
import { RemoteStore } from "./store";
import { Syncer, notifyResult } from "./sync";
import { PreviewModal } from "./ui/PreviewModal";

export default class ObsyncPlugin extends Plugin {
  settings: ObsyncSettings = DEFAULT_SETTINGS;
  state: LocalState = emptyState();
  private statusEl?: HTMLElement;

  async onload() {
    const data = (await this.loadData()) as { settings?: ObsyncSettings } | null;
    this.settings = { ...DEFAULT_SETTINGS, ...(data?.settings ?? {}) };
    this.state = await loadState(this);

    this.addSettingTab(new ObsyncSettingTab(this.app, this));
    this.statusEl = this.addStatusBarItem();
    this.setStatus("idle");

    this.addRibbonIcon("cloud-download", "obsync: preview and pull", () => void this.previewAndPull());
    this.addCommand({ id: "obsync-pull", name: "Pull now", callback: () => void this.pullAll() });
    this.addCommand({
      id: "obsync-preview", name: "Preview what would be pulled",
      callback: () => void this.previewAndPull(),
    });

    // Delay on load so plugin startup is not blocked by network.
    this.app.workspace.onLayoutReady(() => {
      window.setTimeout(() => void this.pullAll(), 10_000);
    });
    // Interval only. NEVER sync on vault file change: a mirror that reacts to
    // the user's own edits is how you get a feedback loop.
    this.registerInterval(
      window.setInterval(() => void this.pullAll(), this.settings.syncIntervalMinutes * 60_000),
    );
  }

  async save() {
    await saveState(this, this.state, this.settings);
  }

  private syncer(): Syncer | null {
    if (!this.settings.baseUrl) {
      new Notice("obsync: set the store URL in settings first");
      return null;
    }
    const store = new RemoteStore({ baseUrl: this.settings.baseUrl, bucket: this.settings.bucket });
    return new Syncer(this, store, this.settings.policy, this.state, this.settings);
  }

  private async previewAndPull() {
    const s = this.syncer();
    if (!s) return;
    const courseId = this.settings.courses[0];
    if (courseId === undefined) {
      new Notice("obsync: no courses configured");
      return;
    }
    const m = await s.fetchManifest(courseId);
    if (!m) {
      new Notice("obsync: no manifest found for that course");
      return;
    }
    new PreviewModal(this.app, s.previewCourse(m), (selected) => {
      void (async () => {
        this.setStatus("syncing…");
        notifyResult(await s.syncCourse(m, selected));
        this.setStatus(`synced ${new Date().toLocaleTimeString()}`);
      })();
    }).open();
  }

  private async pullAll() {
    const s = this.syncer();
    if (!s) return;
    this.setStatus("syncing…");
    try {
      for (const courseId of this.settings.courses) {
        const m = await s.fetchManifest(courseId);
        if (!m) continue;
        // Skip work when the worker has not published since we last looked.
        if (this.state.lastRunId[String(courseId)] === m.run_id) continue;
        notifyResult(await s.syncCourse(m));
      }
      this.setStatus(`synced ${new Date().toLocaleTimeString()}`);
    } catch (e) {
      this.setStatus("sync failed");
      new Notice(`obsync: ${String(e)}`);
    }
  }

  private setStatus(text: string) {
    this.statusEl?.setText(`obsync: ${text}`);
  }
}
