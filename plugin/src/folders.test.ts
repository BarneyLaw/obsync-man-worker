import { describe, it, expect } from "vitest";
import { courseFolderName } from "./folders";

describe("courseFolderName", () => {
  const cases: [Parameters<typeof courseFolderName>[0], string][] = [
    [{ course_id: 93794, course_code: "CS3103" }, "CS3103 (93794)"],
    [{ course_id: 77826, course_code: "CS2103/CS2103T" }, "CS2103-CS2103T (77826)"],
    [{ course_id: 40630, course_code: "THE1001/RC1000A" }, "THE1001-RC1000A (40630)"],
    [{ course_id: 51188, course_code: "TPC" }, "TPC (51188)"],
    [{ course_id: 1, course_code: "  A:B*C  " }, "A-B-C (1)"],
    [{ course_id: 5, course_code: "" }, "5"],
    [{ course_id: 5 }, "5"],
    [{ course_id: 5, course_code: "..." }, "5"],
  ];

  for (const [input, want] of cases) {
    it(`${JSON.stringify(input.course_code)} -> ${want}`, () => {
      expect(courseFolderName(input)).toBe(want);
    });
  }

  it("never produces the same folder for two course ids", () => {
    expect(courseFolderName({ course_id: 1, course_code: "CS3103" }))
      .not.toBe(courseFolderName({ course_id: 2, course_code: "CS3103" }));
  });
});
