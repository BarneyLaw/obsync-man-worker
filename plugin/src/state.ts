import { Plugin } from "obsidian";

/**
 * Local record of what this device wrote. Lives in the plugin's data.json via
 * saveData(), NOT in the vault body.
 *
 * IMPORTANT: data.json sits inside .obsidian/plugins/obsync/ and is synced by
 * whatever else syncs your vault. Never put credentials here. See store.ts for
 * why the recommended transport needs none.
 *
 * It is also why sync.ts checks the FILE on disk rather than trusting the
 * absence of a record here: a second device gets the vault without the
 * data.json that describes it.
 */
export interface FileRecord {
  sha256: string;
  size: number;
  writtenAt: number;
}

export interface LocalState {
  version: 1;
  lastRunId: Record<string, string>; // courseId -> run_id
  files: Record<string, FileRecord>; // vault-relative path -> record
}

export const emptyState = (): LocalState => ({ version: 1, lastRunId: {}, files: {} });

export async function loadState(plugin: Plugin): Promise<LocalState> {
  const raw = (await plugin.loadData()) as { state?: LocalState } | null;
  const s = raw?.state;
  if (!s || s.version !== 1) return emptyState();
  // Tolerate a half-written data.json rather than throwing during onload.
  return {
    version: 1,
    lastRunId: s.lastRunId ?? {},
    files: s.files ?? {},
  };
}

export async function saveState(plugin: Plugin, state: LocalState, settings: unknown) {
  await plugin.saveData({ state, settings });
}
