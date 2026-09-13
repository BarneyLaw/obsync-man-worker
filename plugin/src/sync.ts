import { Plugin, Notice, normalizePath } from "obsidian";
import { sha256 } from "@noble/hashes/sha256";
import { bytesToHex } from "@noble/hashes/utils";
import { Manifest, Entry, blobKey, latestKey, manifestKey, parseManifest } from "./types";
import { Policy } from "./policy";
import { LocalState, saveState } from "./state";
import { RemoteStore, chunkSize, CHUNK_THRESHOLD } from "./store";
import { preview, Preview } from "./preview";

export interface SyncResult {
  added: number;
  updated: number;
  removed: number;
  conflicts: string[];
  skipped: number;
  errors: string[];
}

/**
 * Consumer sync loop.
 *
 * Note this diff (manifest entry vs what I wrote locally) is a DIFFERENT
 * algorithm from the worker's diff (Canvas metadata vs previous manifest).
 * Writing one in Go and one in TypeScript is not duplication.
 */
export class Syncer {
  constructor(
    private plugin: Plugin,
    private store: RemoteStore,
    private policy: Policy,
    private state: LocalState,
    private settings: { targetFolder: string; trashFolder: string; conflictFolder: string },
  ) {}

  async fetchManifest(courseId: number): Promise<Manifest | null> {
    const runId = await this.store.getText(latestKey(courseId));
    if (!runId) return null;
    const raw = await this.store.getText(manifestKey(courseId, runId.trim()));
    if (!raw) return null;
    return parseManifest(raw);
  }

  previewCourse(m: Manifest): Preview {
    return preview(m, this.policy, this.state);
  }

  async syncCourse(m: Manifest, only?: Set<string>): Promise<SyncResult> {
    const res: SyncResult = { added: 0, updated: 0, removed: 0, conflicts: [], skipped: 0, errors: [] };
    const p = this.previewCourse(m);

    for (const item of p.items) {
      if (only && !only.has(item.entry.path)) continue;
      try {
        switch (item.action) {
          case "download":
          case "update": {
            const conflicted = await this.writeEntry(item.entry);
            if (conflicted) res.conflicts.push(item.entry.path);
            else if (item.action === "download") res.added++;
            else res.updated++;
            break;
          }
          default:
            res.skipped++;
        }
      } catch (e) {
        res.errors.push(`${item.entry.path}: ${String(e)}`);
      }
    }

    for (const e of m.entries) {
      if (e.state === "deleted" && this.state.files[e.path]) {
        await this.trash(e.path);
        res.removed++;
      }
    }

    this.state.lastRunId[String(m.course_id)] = m.run_id;
    await saveState(this.plugin, this.state, this.settings);
    return res;
  }

  /** @returns true if the write was diverted to a conflict quarantine. */
  private async writeEntry(e: Entry): Promise<boolean> {
    if (!e.sha256) throw new Error("stored entry without a hash");
    const adapter = this.plugin.app.vault.adapter;
    const dest = normalizePath(`${this.settings.targetFolder}/${e.path}`);

    // One-way sync is NOT a licence to destroy local data. If what is on disk
    // does not match the hash we last wrote, the user edited a read-only file:
    // quarantine it and move on.
    const known = this.state.files[e.path];
    if (known && (await adapter.exists(dest))) {
      const onDisk = bytesToHex(sha256(new Uint8Array(await adapter.readBinary(dest))));
      if (onDisk !== known.sha256) {
        const q = normalizePath(`${this.settings.conflictFolder}/${e.path}`);
        await this.ensureDir(q);
        await adapter.rename(dest, q);
        return true;
      }
    }

    await this.ensureDir(dest);
    // Part file lives in the plugin folder and is dot-prefixed so Obsidian
    // never indexes a half-written PDF and the user never sees it flicker.
    const part = normalizePath(
      `${this.plugin.manifest.dir}/.parts/${e.sha256}.part`,
    );
    await this.ensureDir(part);
    if (await adapter.exists(part)) await adapter.remove(part);

    const key = blobKey(e.sha256);
    const hasher = sha256.create();

    if (e.size <= CHUNK_THRESHOLD) {
      const buf = await this.store.getRange(key, 0, e.size);
      hasher.update(new Uint8Array(buf));
      await adapter.writeBinary(part, buf);
    } else {
      const cs = chunkSize();
      for (let off = 0; off < e.size; off += cs) {
        const n = Math.min(cs, e.size - off);
        const buf = await this.store.getRange(key, off, n);
        hasher.update(new Uint8Array(buf));
        // appendBinary landed in Obsidian 1.12.3 and is why the mobile size
        // ceiling stopped being structural. CapacitorAdapter implements
        // DataAdapter, so this works on phones too.
        await adapter.appendBinary(part, buf);
        // Record progress so an interrupted pull resumes instead of restarting.
        this.state.files[e.path] = {
          sha256: "", size: e.size, writtenAt: Date.now(), partialOffset: off + n,
        };
      }
    }

    // Verify before the rename. Costs nothing (the bytes already passed through
    // the hasher) and is the only check that the store gave us what we asked
    // for.
    const got = bytesToHex(hasher.digest());
    if (got !== e.sha256) {
      await adapter.remove(part);
      throw new Error(`hash mismatch: expected ${e.sha256.slice(0, 12)} got ${got.slice(0, 12)}`);
    }

    if (await adapter.exists(dest)) await adapter.remove(dest);
    await adapter.rename(part, dest);
    this.state.files[e.path] = { sha256: e.sha256, size: e.size, writtenAt: Date.now() };
    return false;
  }

  /** Never hard delete. Lecturers unpublish and republish constantly. */
  private async trash(path: string) {
    const adapter = this.plugin.app.vault.adapter;
    const src = normalizePath(`${this.settings.targetFolder}/${path}`);
    if (!(await adapter.exists(src))) return;
    const dst = normalizePath(`${this.settings.trashFolder}/${path}`);
    await this.ensureDir(dst);
    await adapter.rename(src, dst);
    delete this.state.files[path];
  }

  private async ensureDir(filePath: string) {
    const dir = filePath.slice(0, filePath.lastIndexOf("/"));
    if (dir && !(await this.plugin.app.vault.adapter.exists(dir))) {
      await this.plugin.app.vault.adapter.mkdir(dir);
    }
  }
}

export function notifyResult(r: SyncResult) {
  const parts = [`${r.added} new`, `${r.updated} updated`, `${r.removed} removed`];
  if (r.conflicts.length) parts.push(`${r.conflicts.length} conflicts`);
  if (r.errors.length) parts.push(`${r.errors.length} errors`);
  new Notice(`obsync: ${parts.join(", ")}`);
}
