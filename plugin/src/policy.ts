// Behavioural twin of internal/policy/policy.go.
//
// This is NOT duplication. The worker decides what enters the STORE
// (irreversible). This decides what enters THIS VAULT (reversible, per-device).
// Different defaults, different consequences, same rule schema.
//
// Both implementations are tested against schema/policy-golden.json. If you
// change one, change the fixture, and run both suites.

export type Action = "include" | "skip";

export interface Match {
  ext?: string[];
  glob?: string[];
  min_size?: number;
  max_size?: number;
  course_ids?: number[];
}

export interface Rule {
  name: string;
  priority: number;
  match: Match;
  action: Action;
}

export interface Policy {
  version: number;
  default: Action;
  rules: Rule[];
}

export interface Candidate {
  Path: string;
  Size: number;
  MIME?: string;
  CourseID?: number;
}

export interface Decision {
  action: Action;
  rule: string;
  reason: string;
}

export function validate(p: Policy): string | null {
  if (p.version !== 1) return `unsupported policy version ${p.version}`;
  // Widened to string on purpose: this value arrives from a user-edited JSON
  // textarea, so the declared type is a claim, not a guarantee.
  const dflt: string = p.default;
  if (dflt !== "include" && dflt !== "skip") return `bad default ${dflt}`;
  if (!Array.isArray(p.rules)) return "rules must be an array";

  const seen = new Set<string>();
  for (const r of p.rules) {
    if (!r.name) return "a rule has no name";
    if (seen.has(r.name)) return `duplicate rule name ${r.name}`;
    seen.add(r.name);
    const action: string = r.action;
    if (action !== "include" && action !== "skip") {
      return `rule ${r.name} has bad action ${action}`;
    }
    if (typeof r.priority !== "number" || !Number.isFinite(r.priority)) {
      return `rule ${r.name} has a non-numeric priority`;
    }
    if (!r.match || typeof r.match !== "object") return `rule ${r.name} has no match`;
    if (isEmptyMatch(r.match)) {
      return `rule ${r.name} matches everything, which is what default is for`;
    }
    // Go's path.Match returns ErrBadPattern for these; catching it here means
    // the user finds out while editing rules, not silently at preview time.
    for (const g of r.match.glob ?? []) {
      if (compileGlob(g) === null) return `rule ${r.name} has an invalid glob pattern ${g}`;
    }
  }
  return null;
}

export function evaluate(p: Policy, c: Candidate): Decision {
  let best = -1;
  let bestPri = 0;
  for (let i = 0; i < p.rules.length; i++) {
    const r = p.rules[i]!;
    if (!matches(r.match, c)) continue;
    // Highest priority wins; ties break by document order so a rules file
    // reads top to bottom the way a human expects.
    if (best === -1 || r.priority > bestPri) {
      best = i;
      bestPri = r.priority;
    }
  }
  if (best === -1) return { action: p.default, rule: "", reason: "default policy" };
  const r = p.rules[best]!;
  return { action: r.action, rule: r.name, reason: describe(r, c) };
}

function matches(m: Match, c: Candidate): boolean {
  if (m.course_ids?.length && !m.course_ids.includes(c.CourseID ?? 0)) return false;
  if (m.ext?.length && !m.ext.some((e) => e.replace(/^\./, "").toLowerCase() === ext(c.Path))) {
    return false;
  }
  if (m.glob?.length && !m.glob.some((g) => globMatch(g, c.Path))) return false;
  if (m.min_size && c.Size < m.min_size) return false;
  if (m.max_size && c.Size > m.max_size) return false;
  return true;
}

function isEmptyMatch(m: Match): boolean {
  return (
    !m.ext?.length && !m.glob?.length && !m.min_size && !m.max_size && !m.course_ids?.length
  );
}

function describe(r: Rule, c: Candidate): string {
  if (r.match.min_size && !r.match.ext?.length) {
    return `rule "${r.name}": ${humanBytes(c.Size)} is at or above ${humanBytes(r.match.min_size)}`;
  }
  if (r.match.ext?.length) return `rule "${r.name}": extension .${ext(c.Path)}`;
  return `rule "${r.name}"`;
}

/** Lowercase extension without the dot. A dotfile with no other dot has none. */
export function ext(p: string): string {
  const base = p.slice(p.lastIndexOf("/") + 1);
  const i = base.lastIndexOf(".");
  return i <= 0 ? "" : base.slice(i + 1).toLowerCase();
}

/**
 * Go's path.Match semantics, not full globbing. Matching Go exactly here is the
 * whole point, since the golden fixture is shared with internal/policy.
 *
 * Supported, per Go's grammar:
 *   *          any run of non-separator characters
 *   ?          one non-separator character
 *   [abc]      character class; [^abc] negates (Go uses ^, not !)
 *   [a-z]      range
 *   \x         literal x
 *
 * A malformed pattern (unterminated class, empty class, reversed range,
 * trailing backslash) is Go's ErrBadPattern and matches nothing. validate()
 * surfaces those to the user rather than letting a rule quietly never fire.
 */
export function globMatch(pattern: string, name: string): boolean {
  const rx = compileGlob(pattern);
  return rx !== null && rx.test(name);
}

const globCache = new Map<string, RegExp | null>();

/** @returns null for a pattern Go would reject with ErrBadPattern. */
export function compileGlob(pattern: string): RegExp | null {
  const hit = globCache.get(pattern);
  if (hit !== undefined) return hit;
  const rx = buildGlob(pattern);
  globCache.set(pattern, rx);
  return rx;
}

function buildGlob(pattern: string): RegExp | null {
  let out = "";
  let i = 0;
  while (i < pattern.length) {
    const c = pattern[i]!;
    if (c === "*") {
      // The one rule that makes this path matching rather than string matching.
      out += "[^/]*";
      i++;
    } else if (c === "?") {
      out += "[^/]";
      i++;
    } else if (c === "\\") {
      if (i + 1 >= pattern.length) return null; // trailing backslash
      out += escLiteral(pattern[i + 1]!);
      i += 2;
    } else if (c === "[") {
      const cls = buildClass(pattern, i);
      if (cls === null) return null;
      out += cls.src;
      i = cls.next;
    } else {
      out += escLiteral(c);
      i++;
    }
  }
  try {
    return new RegExp(`^${out}$`, "u");
  } catch {
    return null;
  }
}

function buildClass(p: string, start: number): { src: string; next: number } | null {
  let i = start + 1;
  let neg = false;
  if (p[i] === "^") {
    neg = true;
    i++;
  }
  let body = "";
  let count = 0;
  while (i < p.length && p[i] !== "]") {
    const lo = readClassChar(p, i);
    if (lo === null) return null;
    i = lo.next;
    if (p[i] === "-" && i + 1 < p.length && p[i + 1] !== "]") {
      const hi = readClassChar(p, i + 1);
      if (hi === null) return null;
      if (hi.ch < lo.ch) return null; // Go rejects a reversed range
      body += `${escClass(lo.ch)}-${escClass(hi.ch)}`;
      i = hi.next;
    } else {
      body += escClass(lo.ch);
    }
    count++;
  }
  if (i >= p.length) return null; // unterminated
  if (count === 0) return null;   // Go requires a non-empty class
  return { src: `[${neg ? "^" : ""}${body}]`, next: i + 1 };
}

function readClassChar(p: string, i: number): { ch: string; next: number } | null {
  if (i >= p.length) return null;
  if (p[i] === "\\") {
    if (i + 1 >= p.length) return null;
    return { ch: p[i + 1]!, next: i + 2 };
  }
  return { ch: p[i]!, next: i + 1 };
}

// The `u` flag only permits escaping actual syntax characters, so escape
// exactly those and leave everything else alone.
const SYNTAX = new Set(["^", "$", "\\", ".", "*", "+", "?", "(", ")", "[", "]", "{", "}", "|", "/"]);
const CLASS_SYNTAX = new Set(["\\", "]", "^", "-"]);

function escLiteral(ch: string): string {
  return SYNTAX.has(ch) ? `\\${ch}` : ch;
}

function escClass(ch: string): string {
  return CLASS_SYNTAX.has(ch) ? `\\${ch}` : ch;
}

export function humanBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ["K", "M", "G", "T", "P", "E"];
  let div = 1024;
  let exp = 0;
  for (let m = Math.floor(n / 1024); m >= 1024; m = Math.floor(m / 1024)) {
    div *= 1024;
    exp++;
  }
  return `${(n / div).toFixed(1)} ${units[exp]}B`;
}
