import type { Manifest } from "./types";

/** What the store says about one course ID typed into Setup. */
export type CourseCheck =
  | { id: number; status: "found"; name: string }
  | { id: number; status: "missing" }
  | { id: number; status: "error"; message: string };

/** Split on commas/whitespace, keep the positive integers, report the rest. */
export function parseCourseIds(raw: string): { ids: number[]; bad: string[] } {
  const ids: number[] = [];
  const bad: string[] = [];
  for (const tok of raw.split(/[,\s]+/).filter((s) => s.length > 0)) {
    const n = Number(tok);
    if (Number.isInteger(n) && n > 0) {
      if (!ids.includes(n)) ids.push(n);
    } else {
      bad.push(tok);
    }
  }
  return { ids, bad };
}

export function sameIds(a: readonly number[], b: readonly number[]): boolean {
  return a.length === b.length && a.every((id, i) => id === b[i]);
}

/**
 * Look every ID up at once, keeping the typed order.
 *
 * "Found" means the worker has published a manifest for the course; the plugin
 * cannot see Canvas itself. One unreachable lookup is reported for that ID and
 * does not hide the others.
 */
export async function checkCourses(
  ids: readonly number[],
  fetchManifest: (id: number) => Promise<Manifest | null>,
): Promise<CourseCheck[]> {
  return Promise.all(
    ids.map(async (id): Promise<CourseCheck> => {
      try {
        const m = await fetchManifest(id);
        if (!m) return { id, status: "missing" };
        return { id, status: "found", name: m.course_name || m.course_code || `Course ${id}` };
      } catch (e) {
        return { id, status: "error", message: e instanceof Error ? e.message : String(e) };
      }
    }),
  );
}
