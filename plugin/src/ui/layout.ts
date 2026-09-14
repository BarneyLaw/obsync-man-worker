import { App, Platform } from "obsidian";

/** Obsidian's default sidebar width, in pixels. */
export const DEFAULT_SIDEBAR_WIDTH = 300;

/** The panel opens at 1.5x the default: file trees need the room. */
export const PANEL_WIDTH = Math.round(DEFAULT_SIDEBAR_WIDTH * 1.5);

/** The parts of Obsidian's internal WorkspaceSidedock this relies on. */
interface SizableSplit {
  containerEl?: HTMLElement;
  setSize?: (px: number) => void;
}

/**
 * Widen the right sidebar to PANEL_WIDTH.
 *
 * Obsidian has no public API for sidebar width. WorkspaceSidedock.setSize is
 * internal: it records the size Obsidian saves in the workspace layout and sets
 * the element's width, so the change survives a restart. Being internal, it is
 * feature-detected, and if a future Obsidian drops it this does nothing rather
 * than throwing. It never narrows a sidebar the user already made wider, and
 * does nothing on mobile, where the sidebar is a drawer.
 */
export function widenRightSidebar(app: App): void {
  if (Platform.isMobile) return;
  const split = app.workspace.rightSplit as unknown as SizableSplit;
  if (typeof split.setSize !== "function") return;
  const current = split.containerEl?.getBoundingClientRect().width ?? 0;
  if (current < PANEL_WIDTH) split.setSize(PANEL_WIDTH);
}
