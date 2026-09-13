import { Plugin } from "obsidian";

/**
 * Local record of what this device wrote. Lives in the plugin's data.json via
 * saveData(), NOT in the vault body.
 *
 * IMPORTANT: data.json sits inside .obsidian/plugins/obsync/ and is synced by
 * whatever else syncs your vault. Never put credentials here. See store.ts for
 * why the recommended transport needs none.
 */
export interface FileRecord {
  sha256: string;
  size: number;
  writtenAt: number;
  /** Byte offset reached by an interrupted chunked download, if any. */
  partialOffset?: number;
}

export interface LocalState {
  version: 1;
  lastRunId: Record<string, string>; // courseId -> run_id
  files: Record<string, FileRecord>; // vault-relative path -> record
}

export const emptyState = (): LocalState => ({ version: 1, lastRunId: {}, files: {} });

export async function loadState(plugin: Plugin): Promise<LocalState> {
  const raw = (await plugin.loadData()) as { state?: LocalState } | null;
  return raw?.state ?? emptyState();
}

export async function saveState(plugin: Plugin, state: LocalState, settings: unknown) {
  await plugin.saveData({ state, settings });
}
