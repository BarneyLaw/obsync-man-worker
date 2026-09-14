// Mirror of internal/manifest/manifest.go. The Go side owns the schema; this
// file follows it.
//
// Contract enforcement lives in schema/ at the repo root. The Go types generate
// the first two files, and src/contract.test.ts reads all three:
//   manifest-golden.json   a manifest exercising every state and field
//   store-contract.json    store keys and constants such as SCOPE_RULE
//   manifest-invalid.json  documents both halves must refuse

export const SCHEMA_VERSION = 1;

export type EntryState =
  | "stored"   // blob is in the store, sha256 valid
  | "skipped"  // catalogued, deliberately not fetched. reason says why
  | "locked"   // Canvas says not downloadable yet
  | "failed"   // fetch attempted, failed
  | "deleted"; // tombstone

export const ENTRY_STATES: readonly EntryState[] = ["stored", "skipped", "locked", "failed", "deleted"];

/**
 * rule_name on a skipped entry that a scoped manual pull deferred
 * (`obsync-worker pull -path ...`). Not a rule anyone wrote: the worker's next
 * full pull fetches the file. Mirrors plan.ScopeRule in Go.
 */
export const SCOPE_RULE = "obsync:pull-scope";

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
  /** e.g. "CS3103". Absent in manifests from workers that predate it. */
  course_code?: string;
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
 *
 * Also refuses everything Go's manifest.Validate refuses, so the plugin never
 * acts on a document the worker could not have written.
 */
export function parseManifest(raw: string): Manifest {
  const doc: unknown = JSON.parse(raw);
  if (typeof doc !== "object" || doc === null || Array.isArray(doc)) {
    throw new Error("obsync: manifest is not a JSON object");
  }
  const m = doc as Manifest;
  if (typeof m.schema_version !== "number") {
    throw new Error("obsync: manifest has no schema_version");
  }
  if (m.schema_version > SCHEMA_VERSION) {
    throw new Error(
      `obsync: manifest schema v${m.schema_version} is newer than this plugin understands (v${SCHEMA_VERSION}). Update the plugin.`,
    );
  }
  if (m.schema_version < SCHEMA_VERSION) {
    throw new Error(
      `obsync: manifest schema v${m.schema_version} is older than this plugin supports (v${SCHEMA_VERSION}). Re-run the worker.`,
    );
  }
  if (typeof m.run_id !== "string" || m.run_id === "") {
    throw new Error("obsync: manifest has an empty run id");
  }
  // An empty course is [], never absent: absent would read as "Canvas has
  // nothing" and tombstone every file in the vault.
  if (!Array.isArray(m.entries)) throw new Error("obsync: manifest has no entries");

  const seen = new Set<string>();
  m.entries.forEach((e, i) => checkEntry(e, i, seen));
  return m;
}

function checkEntry(raw: unknown, i: number, seen: Set<string>) {
  if (typeof raw !== "object" || raw === null) {
    throw new Error(`obsync: manifest entry ${i} is not an object`);
  }
  const e = raw as Entry;
  if (typeof e.path !== "string" || e.path === "") {
    throw new Error(`obsync: manifest entry ${i} has an empty path`);
  }
  if (seen.has(e.path)) throw new Error(`obsync: manifest has duplicate path ${e.path}`);
  seen.add(e.path);
  if (!ENTRY_STATES.includes(e.state)) {
    throw new Error(`obsync: ${e.path} has unknown state ${String(e.state)}`);
  }
  if (e.state === "stored" && !e.sha256) throw new Error(`obsync: ${e.path} is stored but has no hash`);
  if (e.state !== "stored" && e.sha256) throw new Error(`obsync: ${e.path} is ${e.state} but carries a hash`);
  if ((e.state === "skipped" || e.state === "failed") && !e.reason) {
    throw new Error(`obsync: ${e.path} is ${e.state} with no reason`);
  }
}

export const liveEntries = (m: Manifest) => m.entries.filter((e) => e.state !== "deleted");
