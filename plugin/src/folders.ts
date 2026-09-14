import type { Manifest } from "./types";

/**
 * The vault folder for one course, under the target folder: "<code> (<id>)",
 * e.g. "CS3103 (93794)".
 *
 * The Canvas id makes it unique, so two courses never share a folder, not even
 * the same module in two terms; the code makes it recognisable. A manifest
 * from a worker too old to send the code gets the id alone, and
 * Syncer.migrateCourse renames the folder once the code arrives.
 *
 * Pure.
 */
export function courseFolderName(m: Pick<Manifest, "course_id" | "course_code">): string {
  const code = segment(m.course_code ?? "");
  return code ? `${code} (${m.course_id})` : String(m.course_id);
}

/**
 * Make a string safe as a single path segment on every platform Obsidian runs
 * on. Cross-listed codes contain "/", as in "CS2103/CS2103T".
 */
function segment(s: string): string {
  return [...s]
    .filter((ch) => ch.charCodeAt(0) >= 32)
    .join("")
    .replace(/[\\/:*?"<>|]/g, "-")
    .replace(/\s+/g, " ")
    .trim()
    .replace(/[. ]+$/, "");
}
