import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { evaluate, validate, ext, globMatch, compileGlob, Policy, Candidate, Action } from "./policy";

// THE contract with internal/policy in Go. Both suites read the one file at the
// repo root and must agree on every case. If they diverge, the plugin will show
// the user a preview that does not match what the worker actually did.
//
// Resolved relative to THIS FILE, not to the working directory, so the suite
// cannot quietly collect zero cases when run from somewhere else.
const goldenPath = fileURLToPath(new URL("../../schema/policy-golden.json", import.meta.url));
const golden = JSON.parse(readFileSync(goldenPath, "utf8")) as {
  policy: Policy;
  cases: { _why?: string; candidate: Candidate; want: Action; want_rule: string }[];
  invalid: { _why: string; policy: unknown }[];
};

describe("policy golden fixture", () => {
  it("validates", () => {
    expect(validate(golden.policy)).toBeNull();
  });

  it("is not empty", () => {
    expect(golden.cases.length).toBeGreaterThan(0);
    expect(golden.invalid.length).toBeGreaterThan(0);
  });

  for (const [i, c] of golden.cases.entries()) {
    it(`case ${i}: ${c.candidate.Path} (${c._why ?? ""})`, () => {
      const d = evaluate(golden.policy, c.candidate);
      expect(d.action).toBe(c.want);
      expect(d.rule).toBe(c.want_rule);
      expect(d.reason).not.toBe("");
    });
  }

  // Go's policy.Parse rejects every one of these too.
  for (const [i, c] of golden.invalid.entries()) {
    it(`rejects invalid ${i}: ${c._why}`, () => {
      expect(validate(c.policy as Policy)).not.toBeNull();
    });
  }
});

describe("ext", () => {
  it("lowercases and strips the dot", () => expect(ext("a/B.PDF")).toBe("pdf"));
  it("returns empty for no extension", () => expect(ext("LICENSE")).toBe(""));
  it("treats a dotfile as having no extension", () => expect(ext(".gitignore")).toBe(""));
  it("ignores dots in parent directories", () => expect(ext("v1.2/README")).toBe(""));
});

describe("globMatch matches Go's path.Match", () => {
  it("star does not cross a slash", () => {
    expect(globMatch("*/solutions/*", "cs/solutions/a.pdf")).toBe(true);
    expect(globMatch("*/solutions/*", "cs/x/solutions/a.pdf")).toBe(false);
  });

  it("question mark is exactly one non-separator character", () => {
    expect(globMatch("a?c", "abc")).toBe(true);
    expect(globMatch("a?c", "ac")).toBe(false);
    expect(globMatch("a?c", "a/c")).toBe(false);
  });

  it("supports character classes", () => {
    expect(globMatch("draft[0-9].pdf", "draft3.pdf")).toBe(true);
    expect(globMatch("draft[0-9].pdf", "draftX.pdf")).toBe(false);
    expect(globMatch("[abc]x", "bx")).toBe(true);
    expect(globMatch("[abc]x", "dx")).toBe(false);
  });

  it("negates with ^, the way Go does, not with !", () => {
    expect(globMatch("[^a]x", "bx")).toBe(true);
    expect(globMatch("[^a]x", "ax")).toBe(false);
  });

  it("treats a backslash as an escape", () => {
    expect(globMatch("a\\*b", "a*b")).toBe(true);
    expect(globMatch("a\\*b", "azzb")).toBe(false);
  });

  it("does not let a dot behave as a wildcard", () => {
    expect(globMatch("a.pdf", "axpdf")).toBe(false);
    expect(globMatch("a.pdf", "a.pdf")).toBe(true);
  });

  it("rejects patterns Go calls ErrBadPattern", () => {
    expect(compileGlob("[abc")).toBeNull();   // unterminated
    expect(compileGlob("[]")).toBeNull();     // empty class
    expect(compileGlob("[z-a]")).toBeNull();  // reversed range
    expect(compileGlob("abc\\")).toBeNull();  // trailing backslash
    // A bad pattern matches nothing rather than throwing at preview time.
    expect(globMatch("[abc", "a")).toBe(false);
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

  it("lets a higher priority override a lower one regardless of order", () => {
    const p: Policy = {
      version: 1, default: "include",
      rules: [
        { name: "low", priority: 1, action: "skip", match: { ext: ["pdf"] } },
        { name: "high", priority: 9, action: "include", match: { ext: ["pdf"] } },
      ],
    };
    expect(evaluate(p, { Path: "a.pdf", Size: 1 }).rule).toBe("high");
  });

  it("handles negative priorities", () => {
    const p: Policy = {
      version: 1, default: "include",
      rules: [
        { name: "lower", priority: -5, action: "skip", match: { ext: ["pdf"] } },
        { name: "higher", priority: -1, action: "include", match: { ext: ["pdf"] } },
      ],
    };
    expect(evaluate(p, { Path: "a.pdf", Size: 1 }).rule).toBe("higher");
  });
});

describe("validate", () => {
  const base = (rules: Policy["rules"]): Policy => ({ version: 1, default: "include", rules });

  it("rejects an unsupported version", () => {
    expect(validate({ version: 2, default: "include", rules: [] })).toMatch(/version/);
  });

  it("rejects a bad default", () => {
    expect(validate({ version: 1, default: "nope" as Action, rules: [] })).toMatch(/default/);
  });

  it("rejects an empty match, which is what default is for", () => {
    expect(validate(base([{ name: "all", priority: 1, action: "skip", match: {} }])))
      .toMatch(/matches everything/);
  });

  it("rejects duplicate rule names", () => {
    expect(validate(base([
      { name: "dup", priority: 1, action: "skip", match: { ext: ["pdf"] } },
      { name: "dup", priority: 2, action: "skip", match: { ext: ["mp4"] } },
    ]))).toMatch(/duplicate/);
  });

  it("rejects an unnamed rule", () => {
    expect(validate(base([{ name: "", priority: 1, action: "skip", match: { ext: ["pdf"] } }])))
      .toMatch(/no name/);
  });

  it("rejects a bad action", () => {
    expect(validate(base([
      { name: "r", priority: 1, action: "delete" as Action, match: { ext: ["pdf"] } },
    ]))).toMatch(/action/);
  });

  it("rejects an invalid glob so a rule cannot silently never fire", () => {
    expect(validate(base([{ name: "r", priority: 1, action: "skip", match: { glob: ["[abc"] } }])))
      .toMatch(/invalid glob/);
  });
});
