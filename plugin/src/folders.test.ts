import { describe, it, expect } from "vitest";
import { courseFolderName, termOf } from "./folders";

type Course = Parameters<typeof courseFolderName>[0];

// Real course names and codes from NUS Canvas, plus edge cases.
const cases: [Course, string][] = [
  [{ course_id: 93794, course_code: "CS3103", course_name: "CS3103 Computer Networks Practice [2610]" }, "CS3103 [2610] (93794)"],
  [{ course_id: 77826, course_code: "CS2103/CS2103T", course_name: "CS2103/CS2103T Software Engineering [2510]" }, "CS2103-CS2103T [2510] (77826)"],
  [{ course_id: 94846, course_code: "LAG1201", course_name: "LAG1201 German 1 [2610]" }, "LAG1201 [2610] (94846)"],
  [{ course_id: 81917, course_code: "CP2106", course_name: "Orbital 25" }, "CP2106 (81917)"],
  [{ course_id: 41924, course_code: "SOCT101", course_name: "SOCT101 SoC Teaching Workshop" }, "SOCT101 (41924)"],
  [{ course_id: 51188, course_code: "TPC", course_name: "Travel Preparedness Course" }, "TPC (51188)"],
  [{ course_id: 1, course_code: "X1", course_name: "Name [ 2610 ] " }, "X1 [2610] (1)"],
  [{ course_id: 1, course_code: "X1", course_name: "Name [AY26/27 S1]" }, "X1 [AY26-27 S1] (1)"],
  [{ course_id: 1, course_code: "X1", course_name: "Name [2610] continued" }, "X1 (1)"],
  [{ course_id: 1, course_code: "  A:B*C  ", course_name: "" }, "A-B-C (1)"],
  [{ course_id: 5, course_code: "", course_name: "CS3103 Networks [2610]" }, "5"],
  [{ course_id: 5, course_name: "CS3103 Networks [2610]" }, "5"],
  [{ course_id: 5, course_code: "...", course_name: "" }, "5"],
];

describe("courseFolderName", () => {
  for (const [input, want] of cases) {
    it(`${JSON.stringify(input.course_code)} / ${JSON.stringify(input.course_name)} -> ${want}`, () => {
      expect(courseFolderName(input)).toBe(want);
    });
  }

  it("never produces the same folder for two course ids", () => {
    const name = "CS3103 Computer Networks Practice [2610]";
    expect(courseFolderName({ course_id: 1, course_code: "CS3103", course_name: name }))
      .not.toBe(courseFolderName({ course_id: 2, course_code: "CS3103", course_name: name }));
  });
});

describe("termOf", () => {
  it("reads only a trailing bracket tag", () => {
    expect(termOf("CS3103 Computer Networks Practice [2610]")).toBe("2610");
    expect(termOf("[draft] notes")).toBe("");
    expect(termOf("Orbital 25")).toBe("");
  });
});
