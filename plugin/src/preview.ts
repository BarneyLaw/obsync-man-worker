import { Manifest, Entry, liveEntries } from "./types";
import { Policy, evaluate, humanBytes } from "./policy";
import { LocalState } from "./state";

/**
 * The payoff of cataloguing skipped files instead of dropping them: this is a
 * PURE FUNCTION over data the plugin already has. No round trip to the worker,
 * instant, works offline, and the user can see exactly what is being withheld
 * and why.
 */
export interface PreviewItem {
  entry: Entry;
  action: "download" | "update" | "have" | "skip-local" | "skip-worker" | "locked" | "unavailable";
  reason: string;
}

export interface Preview {
  items: PreviewItem[];
  toDownload: number;
  bytesToDownload: number;
  alreadyHave: number;
  skippedLocal: number;
  skippedWorker: number;
  locked: number;
}

export function preview(m: Manifest, p: Policy, state: LocalState): Preview {
  const out: Preview = {
    items: [], toDownload: 0, bytesToDownload: 0,
    alreadyHave: 0, skippedLocal: 0, skippedWorker: 0, locked: 0,
  };

  for (const e of liveEntries(m)) {
    const item = classify(e, m.course_id, p, state);
    out.items.push(item);
    switch (item.action) {
      case "download":
      case "update":
        out.toDownload++;
        out.bytesToDownload += e.size;
        break;
      case "have": out.alreadyHave++; break;
      case "skip-local": out.skippedLocal++; break;
      case "skip-worker": out.skippedWorker++; break;
      case "locked": out.locked++; break;
    }
  }
  return out;
}

function classify(e: Entry, courseId: number, p: Policy, state: LocalState): PreviewItem {
  if (e.state === "locked") {
    const when = e.unlock_at ? new Date(e.unlock_at).toLocaleString() : "unknown";
    return { entry: e, action: "locked", reason: `unlocks ${when}` };
  }
  if (e.state === "skipped") {
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

  const known = state.files[e.path];
  if (known && known.sha256 === e.sha256) {
    return { entry: e, action: "have", reason: "up to date" };
  }
  if (known) {
    return { entry: e, action: "update", reason: `changed, ${humanBytes(e.size)}` };
  }
  return { entry: e, action: "download", reason: `new, ${humanBytes(e.size)}` };
}
