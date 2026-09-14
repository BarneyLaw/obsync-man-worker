import { Plugin, Notice, normalizePath } from "obsidian";
import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";
import { Manifest, Entry, blobKey, latestKey, manifestKey, parseManifest, liveEntries } from "./types";
import { Policy } from "./policy";
import { FileRecord, LocalState, saveState } from "./state";
import { RemoteStore, chunkSize, CHUNK_THRESHOLD } from "./store";
import { preview, Preview } from "./preview";

export interface SyncResult {
  added: number;
  updated: number;
  removed: number;
  conflicts: string[];
  skipped: number;
  errors: string[];
  /** Files already on disk with the right bytes; adopted without a download. */
  adopted: number;
}

/** Outcome of a single entry write. */
type WriteOutcome = "written" | "conflict" | "adopted";

export interface SyncFolders {
  targetFolder: string;
  trashFolder: string;
  conflictFolder: string;
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
    private settings: SyncFolders,
  ) {}

  async fetchManifest(courseId: number): Promise<Manifest | null> {
    const latest = await this.store.getText(latestKey(courseId));
    if (!latest) return null;
    const runId = latest.trim();
    const key = manifestKey(courseId, runId);
    const raw = await this.store.getText(key);
    if (!raw) return null;
    const m = parseManifest(raw);
    // The worker's own reader checks this too. A manifest naming another
    // course or run is a mis-served or hand-copied object, and syncing it
    // would tombstone the wrong course's files.
    if (m.course_id !== courseId || m.run_id !== runId) {
      throw new Error(`obsync: ${key} claims course ${m.course_id} run ${m.run_id}`);
    }
    return m;
  }

  /**
   * Stored entries this device recorded as written whose file is no longer in
   * the vault.
   *
   * The record alone is what makes preview call a file up to date. Without this
   * check, a file the user deleted stays "up to date" forever and is never
   * pulled again, and pullAll skips the course because the run has not changed.
   */
  async missingFiles(m: Manifest): Promise<Set<string>> {
    const adapter = this.plugin.app.vault.adapter;
    const recorded = liveEntries(m).filter((e) => e.state === "stored" && this.state.files[e.path]);
    const present = await Promise.all(recorded.map((e) => adapter.exists(this.dest(e.path))));
    return new Set(recorded.filter((_, i) => !present[i]).map((e) => e.path));
  }

  /**
   * Whether a background pull has work: the worker published a run this device
   * has not finished, or files this device wrote have gone missing.
   */
  async needsSync(m: Manifest): Promise<boolean> {
    if (this.state.lastRunId[String(m.course_id)] !== m.run_id) return true;
    return (await this.missingFiles(m)).size > 0;
  }

  previewCourse(m: Manifest, missing: ReadonlySet<string> = new Set()): Preview {
    return preview(m, this.policy, this.state, missing);
  }

  /**
   * @param only - when present, restricts the pull to these paths. A partial
   *   pull deliberately does NOT advance lastRunId and does NOT process
   *   tombstones: the run is not finished, and claiming otherwise would make
   *   pullAll skip the course and strand every unselected file.
   */
  async syncCourse(m: Manifest, only?: Set<string>): Promise<SyncResult> {
    const res: SyncResult = {
      added: 0, updated: 0, removed: 0, conflicts: [], skipped: 0, errors: [], adopted: 0,
    };
    const p = this.previewCourse(m, await this.missingFiles(m));
    const partial = only !== undefined;

    for (const item of p.items) {
      if (only && !only.has(item.entry.path)) continue;
      try {
        switch (item.action) {
          case "download":
          case "update": {
            const outcome = await this.writeEntry(item.entry);
            if (outcome === "conflict") res.conflicts.push(item.entry.path);
            else if (outcome === "adopted") res.adopted++;
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

    if (!partial) {
      for (const e of m.entries) {
        if (e.state === "deleted" && this.state.files[e.path]) {
          try {
            await this.trash(e.path);
            res.removed++;
          } catch (err) {
            res.errors.push(`${e.path}: ${String(err)}`);
          }
        }
      }
      // Only a complete pass may claim the run. Marking a partial pull as done
      // would trip the "nothing new since last look" guard in pullAll.
      this.state.lastRunId[String(m.course_id)] = m.run_id;
    }

    await saveState(this.plugin, this.state, this.settings);
    return res;
  }

  private async writeEntry(e: Entry): Promise<WriteOutcome> {
    if (!e.sha256) throw new Error("stored entry without a hash");
    const adapter = this.plugin.app.vault.adapter;
    const dest = this.dest(e.path);
    const known = this.state.files[e.path];

    // One-way sync is NOT a licence to destroy local data.
    //
    // The check is on the FILE, not on whether we have a record of it. A vault
    // synced to a second device arrives with files on disk and an empty
    // data.json, so gating this on `known` would let the first sync on a new
    // device silently overwrite the user's edits.
    if (await adapter.exists(dest)) {
      const onDisk = await this.hashFile(dest, known);
      if (onDisk === e.sha256) {
        // Right bytes already there (second device, or a restored backup).
        // Adopt it instead of re-downloading.
        this.state.files[e.path] = { sha256: e.sha256, size: e.size, writtenAt: Date.now() };
        return "adopted";
      }
      if (onDisk !== known?.sha256) {
        // Either we have no record of this file, or it no longer matches what
        // we last wrote. Both mean the bytes are not ours to overwrite.
        await this.quarantine(dest, e.path);
        return "conflict";
      }
    }

    // Part file lives in the plugin folder and is dot-prefixed so Obsidian
    // never indexes a half-written PDF and the user never sees it flicker.
    const part = normalizePath(`${this.partsDir()}/${e.sha256}.part`);
    await this.ensureDir(part);
    if (await adapter.exists(part)) await adapter.remove(part);

    const key = blobKey(e.sha256);
    const hasher = sha256.create();

    try {
      if (e.size <= CHUNK_THRESHOLD) {
        // Plain GET: no Range arithmetic, and it is the only path that copes
        // with a zero-byte object.
        const buf = await this.store.getBinary(key);
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
        }
      }

      // Verify before the rename. Costs nothing (the bytes already passed
      // through the hasher) and is the only check that the store gave us what
      // we asked for.
      const got = bytesToHex(hasher.digest());
      if (got !== e.sha256) {
        throw new Error(`hash mismatch: expected ${e.sha256.slice(0, 12)} got ${got.slice(0, 12)}`);
      }
    } catch (err) {
      // Never leave a partial file behind: it is keyed by the expected hash, so
      // a stale one would otherwise sit in .parts forever.
      if (await adapter.exists(part)) await adapter.remove(part);
      throw err;
    }

    await this.ensureDir(dest);
    if (await adapter.exists(dest)) await adapter.remove(dest);
    await adapter.rename(part, dest);
    this.state.files[e.path] = { sha256: e.sha256, size: e.size, writtenAt: Date.now() };
    return "written";
  }

  /**
   * Hash of the file on disk, with a stat fast path.
   *
   * Reading a file back costs one whole-file buffer, which is exactly what the
   * chunked download path exists to avoid. So when size and mtime still match
   * what we recorded at write time, take that as untouched rather than pulling
   * a 300 MB recording into memory on a phone every sync.
   */
  private async hashFile(path: string, known?: FileRecord): Promise<string> {
    const adapter = this.plugin.app.vault.adapter;
    if (known) {
      const st = await adapter.stat(path);
      // 5s of slack: mtime is stamped by the adapter just before writtenAt.
      if (st && st.size === known.size && st.mtime <= known.writtenAt + 5000) {
        return known.sha256;
      }
    }
    return bytesToHex(sha256(new Uint8Array(await adapter.readBinary(path))));
  }

  /** Move a locally-modified file aside rather than clobbering it. */
  private async quarantine(src: string, relPath: string) {
    const adapter = this.plugin.app.vault.adapter;
    let q = normalizePath(`${this.settings.conflictFolder}/${relPath}`);
    if (await adapter.exists(q)) q = uniquify(q, Date.now());
    await this.ensureDir(q);
    await adapter.rename(src, q);
  }

  /** Never hard delete. Lecturers unpublish and republish constantly. */
  private async trash(path: string) {
    const adapter = this.plugin.app.vault.adapter;
    const src = normalizePath(`${this.settings.targetFolder}/${path}`);
    if (!(await adapter.exists(src))) {
      delete this.state.files[path];
      return;
    }
    let dst = normalizePath(`${this.settings.trashFolder}/${path}`);
    if (await adapter.exists(dst)) dst = uniquify(dst, Date.now());
    await this.ensureDir(dst);
    await adapter.rename(src, dst);
    delete this.state.files[path];
  }

  /** Vault path of a manifest entry. */
  private dest(path: string): string {
    return normalizePath(`${this.settings.targetFolder}/${path}`);
  }

  /** `.obsidian/plugins/<id>/.parts`, with a fallback: manifest.dir is optional. */
  private partsDir(): string {
    const dir = this.plugin.manifest.dir
      ?? `${this.plugin.app.vault.configDir}/plugins/${this.plugin.manifest.id}`;
    return `${dir}/.parts`;
  }

  /**
   * Create every missing parent of a file path.
   *
   * DataAdapter.mkdir creates ONE directory; it is not documented as recursive
   * and CapacitorAdapter wraps a call that needs an explicit recursive flag.
   * Canvas paths are nested ("Week 1/Lecture 2/slides.pdf"), so walk it.
   */
  private async ensureDir(filePath: string) {
    const adapter = this.plugin.app.vault.adapter;
    const dir = filePath.slice(0, filePath.lastIndexOf("/"));
    if (!dir) return;
    const parts = dir.split("/").filter((s) => s.length > 0);
    let cur = "";
    for (const seg of parts) {
      cur = cur ? `${cur}/${seg}` : seg;
      if (!(await adapter.exists(cur))) {
        try {
          await adapter.mkdir(cur);
        } catch {
          // Racing another mkdir of the same path is fine; a real failure will
          // surface on the write that follows.
        }
      }
    }
  }
}

/** "a/b.pdf" + 123 -> "a/b (123).pdf". Keeps the extension where users expect it. */
export function uniquify(path: string, n: number): string {
  const slash = path.lastIndexOf("/");
  const dot = path.lastIndexOf(".");
  if (dot > slash + 1) return `${path.slice(0, dot)} (${n})${path.slice(dot)}`;
  return `${path} (${n})`;
}

export function notifyResult(r: SyncResult) {
  const parts = [`${r.added} new`, `${r.updated} updated`, `${r.removed} removed`];
  if (r.adopted) parts.push(`${r.adopted} already present`);
  if (r.conflicts.length) parts.push(`${r.conflicts.length} conflicts`);
  if (r.errors.length) parts.push(`${r.errors.length} errors`);
  new Notice(`obsync: ${parts.join(", ")}`);
}
