// Mirror of internal/manifest/manifest.go. The Go side owns the schema; this
// file follows it.
//
// Contract enforcement: schema/manifest.v1.json in the repo root is a golden
// fixture that both sides decode in their test suites. Ten lines of test per
// side, and it catches every drift.

export const SCHEMA_VERSION = 1;

export type EntryState =
  | "stored"   // blob is in the store, sha256 valid
  | "skipped"  // catalogued, deliberately not fetched. reason says why
  | "locked"   // Canvas says not downloadable yet
  | "failed"   // fetch attempted, failed
  | "deleted"; // tombstone

export interface Entry {
  path: string;
  state: EntryState;
  size: number;
  mime?: string;
  canvas_id: number;
  canvas_uuid?: string;
  updated_at: string;
  modified_at: string;
  sha256?: string;
  reason?: string;
  rule_name?: string;
  unlock_at?: string;
  deleted_at?: string;
}

export interface Manifest {
  schema_version: number;
  course_id: number;
  course_name: string;
  run_id: string;
  prev_run_id?: string;
  generated_at: string;
  rules_hash: string;
  entries: Entry[];
}

export function blobKey(sha256: string): string {
  return `blobs/sha256/${sha256.slice(0, 2)}/${sha256.slice(2, 4)}/${sha256}`;
}

export const latestKey = (courseId: number) => `manifests/${courseId}/latest`;
export const manifestKey = (courseId: number, runId: string) =>
  `manifests/${courseId}/${runId}.json`;

/**
 * Refuse unknown schema versions rather than ignoring fields we do not
 * understand. A consumer that silently drops fields will corrupt a vault the
 * first time the schema grows.
 */
export function parseManifest(raw: string): Manifest {
  const m = JSON.parse(raw) as Manifest;
  if (m.schema_version !== SCHEMA_VERSION) {
    throw new Error(
      `obsync: manifest schema v${m.schema_version} is newer than this plugin understands (v${SCHEMA_VERSION}). Update the plugin.`,
    );
  }
  if (!Array.isArray(m.entries)) throw new Error("obsync: manifest has no entries");
  return m;
}

export const liveEntries = (m: Manifest) => m.entries.filter((e) => e.state !== "deleted");
