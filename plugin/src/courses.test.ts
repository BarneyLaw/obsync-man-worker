import { describe, it, expect } from "vitest";
import { checkCourses, parseCourseIds, sameIds } from "./courses";
import type { Manifest } from "./types";

describe("parseCourseIds", () => {
  it("accepts commas, spaces or both, and drops duplicates", () => {
    expect(parseCourseIds("93794, 77826,94846  93794")).toEqual({ ids: [93794, 77826, 94846], bad: [] });
  });

  it("reports what is not a course ID without losing the rest", () => {
    expect(parseCourseIds("93794, CS3103, 0, -5, 1.5,,")).toEqual({ ids: [93794], bad: ["CS3103", "0", "-5", "1.5"] });
  });

  it("treats an empty field as no courses", () => {
    expect(parseCourseIds("  ,  ")).toEqual({ ids: [], bad: [] });
  });
});

describe("sameIds", () => {
  it("compares in order", () => {
    expect(sameIds([1, 2], [1, 2])).toBe(true);
    expect(sameIds([1, 2], [2, 1])).toBe(false);
    expect(sameIds([1], [1, 2])).toBe(false);
  });
});

describe("checkCourses", () => {
  const manifest = (id: number): Manifest => ({
    schema_version: 1, course_id: id, course_name: "CS3103 Computer Networks Practice [2610]",
    course_code: "CS3103", run_id: "r", generated_at: "", rules_hash: "", entries: [],
  });

  it("says which IDs the store has, which it does not, and which it could not check", async () => {
    const results = await checkCourses([93794, 12345, 55555], (id) => {
      if (id === 93794) return Promise.resolve(manifest(id));
      if (id === 12345) return Promise.resolve(null);
      return Promise.reject(new Error("obsync: GET manifests/55555/latest -> 500"));
    });

    expect(results).toEqual([
      { id: 93794, status: "found", name: "CS3103 Computer Networks Practice [2610]" },
      { id: 12345, status: "missing" },
      { id: 55555, status: "error", message: "obsync: GET manifests/55555/latest -> 500" },
    ]);
  });

  it("keeps the typed order however the lookups finish", async () => {
    const results = await checkCourses([3, 1, 2], (id) =>
      new Promise((resolve) => setTimeout(() => resolve(id === 1 ? manifest(id) : null), 5 * id)));
    expect(results.map((r) => r.id)).toEqual([3, 1, 2]);
  });
});
