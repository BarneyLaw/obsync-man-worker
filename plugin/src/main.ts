import { Plugin, Notice, WorkspaceLeaf } from "obsidian";
import { ObsyncSettings, DEFAULT_SETTINGS, ObsyncSettingTab } from "./settings";
import { LocalState, loadState, saveState, emptyState } from "./state";
import { RemoteStore } from "./store";
import { Syncer, notifyResult } from "./sync";
import { ObsyncView, VIEW_TYPE_OBSYNC } from "./ui/ObsyncView";

export default class ObsyncPlugin extends Plugin {
  settings: ObsyncSettings = DEFAULT_SETTINGS;
  state: LocalState = emptyState();
  private statusEl?: HTMLElement;
  private intervalId?: number;
  /** One pull at a time: the interval must not stack on a slow sync. */
  private running = false;
  private status = "idle";
  private statusListeners = new Set<(text: string) => void>();

  async onload() {
    const data = (await this.loadData()) as { settings?: ObsyncSettings } | null;
    this.settings = { ...DEFAULT_SETTINGS, ...(data?.settings ?? {}) };
    this.state = await loadState(this);

    this.addSettingTab(new ObsyncSettingTab(this.app, this));

    this.registerView(VIEW_TYPE_OBSYNC, (leaf: WorkspaceLeaf) => new ObsyncView(leaf, this));

    this.statusEl = this.addStatusBarItem();
    this.setStatus("idle");
    this.statusEl?.addEventListener("click", () => void this.activateView());

    // The panel is the plugin's main surface, so the ribbon icon opens it.
    this.addRibbonIcon("cloud-download", "obsync", () => void this.activateView());

    this.addCommand({
      id: "open-panel", name: "Open panel",
      callback: () => void this.activateView(),
    });
    this.addCommand({ id: "pull", name: "Pull now", callback: () => void this.pullAll() });

    // Delay on load so plugin startup is not blocked by network.
    this.app.workspace.onLayoutReady(() => {
      this.registerInterval(window.setTimeout(() => void this.pullAll(), 10_000));
    });
    // Interval only. NEVER sync on vault file change: a mirror that reacts to
    // the user's own edits is how you get a feedback loop.
    this.rescheduleSync();
  }

  /** Reveal the panel, creating it in the right sidebar if it is not open. */
  async activateView() {
    const { workspace } = this.app;
    let leaf: WorkspaceLeaf | null = workspace.getLeavesOfType(VIEW_TYPE_OBSYNC)[0] ?? null;
    if (!leaf) {
      leaf = workspace.getRightLeaf(false);
      await leaf?.setViewState({ type: VIEW_TYPE_OBSYNC, active: true });
    }
    if (leaf) await workspace.revealLeaf(leaf);
  }

  /** Settings changes must take effect without an Obsidian restart. */
  rescheduleSync() {
    if (this.intervalId !== undefined) window.clearInterval(this.intervalId);
    this.intervalId = window.setInterval(
      () => void this.pullAll(),
      this.settings.syncIntervalMinutes * 60_000,
    );
    this.registerInterval(this.intervalId);
  }

  async save() {
    await saveState(this, this.state, this.settings);
  }

  makeSyncer(): Syncer | null {
    if (!this.settings.baseUrl) {
      new Notice("obsync: set the store URL in the panel's Setup section first");
      return null;
    }
    if (this.settings.courses.length === 0) {
      new Notice("obsync: add at least one course ID in the panel's Setup section");
      return null;
    }
    const store = new RemoteStore({ baseUrl: this.settings.baseUrl, bucket: this.settings.bucket });
    return new Syncer(this, store, this.settings.policy, this.state, this.settings);
  }

  async pullAll() {
    if (this.running) return;
    const s = this.makeSyncer();
    if (!s) return;
    this.running = true;
    this.setStatus("syncing...");
    try {
      // One failing course must not abort the others: its previous state stays
      // live, which is the correct degraded outcome.
      const failed: number[] = [];
      for (const courseId of this.settings.courses) {
        try {
          const m = await s.fetchManifest(courseId);
          if (!m) continue;
          // Skip work when the worker has not published since we last looked.
          if (this.state.lastRunId[String(courseId)] === m.run_id) continue;
          notifyResult(await s.syncCourse(m));
        } catch (e) {
          failed.push(courseId);
          console.error(`obsync: course ${courseId} failed`, e);
        }
      }
      if (failed.length > 0) {
        this.setStatus(`sync failed: ${failed.join(", ")}`);
        new Notice(`obsync: ${failed.length} course(s) failed. See the console.`);
      } else {
        this.setStatus(`synced ${new Date().toLocaleTimeString()}`);
      }
    } finally {
      this.running = false;
    }
  }

  currentStatus(): string {
    return this.status;
  }

  /** @returns an unsubscribe function; the panel calls it on close. */
  onStatus(cb: (text: string) => void): () => void {
    this.statusListeners.add(cb);
    return () => this.statusListeners.delete(cb);
  }

  private setStatus(text: string) {
    this.status = text;
    this.statusEl?.setText(`obsync: ${text}`);
    for (const cb of this.statusListeners) cb(text);
  }
}
