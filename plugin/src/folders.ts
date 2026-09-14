import type { Manifest } from "./types";

/**
 * The vault folder for one course, under the target folder:
 * "<code> [<term>] (<id>)", e.g. "CS3103 [2610] (93794)".
 *
 * The Canvas id makes it unique, so two courses never share a folder; the code
 * and term make it recognisable. The term is the tag Canvas course names end
 * with; a course without one gets "<code> (<id>)". A manifest from a worker too
 * old to send the code gets the id alone. Whenever the name changes,
 * Syncer.migrateCourse renames the folder rather than pulling it again.
 *
 * Pure.
 */
export function courseFolderName(m: Pick<Manifest, "course_id" | "course_code" | "course_name">): string {
  const code = segment(m.course_code ?? "");
  if (!code) return String(m.course_id);
  const term = segment(termOf(m.course_name));
  return term ? `${code} [${term}] (${m.course_id})` : `${code} (${m.course_id})`;
}

/**
 * The term tag a Canvas course name ends with: "2610" in
 * "CS3103 Computer Networks Practice [2610]". Empty when there is none.
 */
export function termOf(courseName: string): string {
  return /\[([^[\]]+)\]\s*$/.exec(courseName)?.[1]?.trim() ?? "";
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
