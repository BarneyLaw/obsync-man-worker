import { Manifest, Entry, liveEntries, SCOPE_RULE } from "./types";
import { Policy, evaluate, humanBytes } from "./policy";
import { FileRecord } from "./state";

/**
 * The payoff of cataloguing skipped files instead of dropping them: this is a
 * PURE FUNCTION over data the plugin already has. No round trip to the worker,
 * instant, works offline, and the user can see exactly what is being withheld
 * and why.
 */
export interface PreviewItem {
  entry: Entry;
  action:
    | "download"
    | "update"
    /** Written by this device, since deleted from the vault. Only a user-started pull puts it back. */
    | "restore"
    | "have"
    | "skip-local"
    | "skip-worker"
    | "deferred"
    | "locked"
    | "unavailable";
  reason: string;
}

/** Items a pull can write: new, changed, or deleted from the vault. */
export function isPullable(item: PreviewItem): boolean {
  return item.action === "download" || item.action === "update" || item.action === "restore";
}

export interface Preview {
  items: PreviewItem[];
  /** Everything a manual pull would write: downloads, updates and restores. */
  toDownload: number;
  bytesToDownload: number;
  alreadyHave: number;
  skippedLocal: number;
  skippedWorker: number;
  /** Deferred by a scoped manual pull; the worker's next full pull fetches them. */
  deferred: number;
  locked: number;
}

/**
 * @param files - what this device wrote for this manifest's course, by path.
 * @param missing - paths this device recorded as written that are no longer in
 *   the vault (Syncer.missingFiles). The only non-pure input, passed in so this
 *   stays a function of its arguments.
 */
export function preview(
  m: Manifest,
  p: Policy,
  files: Readonly<Record<string, FileRecord>>,
  missing: ReadonlySet<string> = new Set(),
): Preview {
  const out: Preview = {
    items: [], toDownload: 0, bytesToDownload: 0,
    alreadyHave: 0, skippedLocal: 0, skippedWorker: 0, deferred: 0, locked: 0,
  };

  for (const e of liveEntries(m)) {
    const item = classify(e, m.course_id, p, files, missing);
    out.items.push(item);
    switch (item.action) {
      case "download":
      case "update":
      case "restore":
        out.toDownload++;
        out.bytesToDownload += e.size;
        break;
      case "have": out.alreadyHave++; break;
      case "skip-local": out.skippedLocal++; break;
      case "skip-worker": out.skippedWorker++; break;
      case "deferred": out.deferred++; break;
      case "locked": out.locked++; break;
    }
  }
  return out;
}

function classify(
  e: Entry,
  courseId: number,
  p: Policy,
  files: Readonly<Record<string, FileRecord>>,
  missing: ReadonlySet<string>,
): PreviewItem {
  if (e.state === "locked") {
    const when = e.unlock_at ? new Date(e.unlock_at).toLocaleString() : "unknown";
    return { entry: e, action: "locked", reason: `unlocks ${when}` };
  }
  if (e.state === "skipped") {
    if (e.rule_name === SCOPE_RULE) {
      // Not a rule anyone wrote: a scoped manual pull on the worker left this
      // for later. Nothing for the user to change; it arrives on its own.
      return { entry: e, action: "deferred", reason: "waiting for the worker's next full pull" };
    }
    // Surfaced, not hidden. The user can request it, and phase 1 answers that
    // by writing to the request bucket (or by relaxing worker rules).
    return { entry: e, action: "skip-worker", reason: e.reason ?? "excluded by worker rules" };
  }
  if (e.state === "failed") {
    return { entry: e, action: "unavailable", reason: e.reason ?? "fetch failed" };
  }

  // state === "stored": the bytes exist. Does THIS vault want them?
  const d = evaluate(p, { Path: e.path, Size: e.size, MIME: e.mime, CourseID: courseId });
  if (d.action === "skip") {
    return { entry: e, action: "skip-local", reason: d.reason };
  }

  // The worker only annotates a stored entry when a human should know
  // something: a stand-in name for an unportable Canvas filename, or a failed
  // refresh that means an older version is being served.
  const note = e.reason ? ` (${e.reason})` : "";
  const known = files[e.path];
  if (known && missing.has(e.path)) {
    // Recorded as written, but gone from the vault: usually the user deleted
    // it. Offered in the panel, but automatic pulls leave it alone so a
    // deletion is not undone behind the user's back.
    return { entry: e, action: "restore", reason: `missing from the vault, ${humanBytes(e.size)}${note}` };
  }
  if (known && known.sha256 === e.sha256) {
    return { entry: e, action: "have", reason: `up to date${note}` };
  }
  if (known) {
    return { entry: e, action: "update", reason: `changed, ${humanBytes(e.size)}${note}` };
  }
  return { entry: e, action: "download", reason: `new, ${humanBytes(e.size)}${note}` };
}
