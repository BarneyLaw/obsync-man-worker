import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { evaluate, validate, ext, globMatch, Policy, Candidate, Action } from "./policy";

// THE contract with internal/policy in Go. Both suites read this file and must
// agree on every case. If they diverge, the plugin will show the user a preview
// that does not match what the worker actually did.
const golden = JSON.parse(readFileSync("../schema/policy-golden.json", "utf8")) as {
  policy: Policy;
  cases: { candidate: Candidate; want: Action; want_rule: string }[];
};

describe("policy golden fixture", () => {
  it("validates", () => {
    expect(validate(golden.policy)).toBeNull();
  });

  for (const [i, c] of golden.cases.entries()) {
    it(`case ${i}: ${c.candidate.Path}`, () => {
      const d = evaluate(golden.policy, c.candidate);
      expect(d.action).toBe(c.want);
      expect(d.rule).toBe(c.want_rule);
      expect(d.reason).not.toBe("");
    });
  }
});

describe("ext", () => {
  it("lowercases and strips the dot", () => expect(ext("a/B.PDF")).toBe("pdf"));
  it("returns empty for no extension", () => expect(ext("LICENSE")).toBe(""));
  it("treats a dotfile as having no extension", () => expect(ext(".gitignore")).toBe(""));
});

describe("globMatch matches Go's path.Match", () => {
  it("star does not cross a slash", () => {
    expect(globMatch("*/solutions/*", "cs/solutions/a.pdf")).toBe(true);
    expect(globMatch("*/solutions/*", "cs/x/solutions/a.pdf")).toBe(false);
  });
});

describe("priority", () => {
  it("breaks ties by document order", () => {
    const p: Policy = {
      version: 1, default: "include",
      rules: [
        { name: "first", priority: 5, action: "skip", match: { ext: ["pdf"] } },
        { name: "second", priority: 5, action: "include", match: { ext: ["pdf"] } },
      ],
    };
    expect(evaluate(p, { Path: "a.pdf", Size: 1 }).rule).toBe("first");
  });
});
