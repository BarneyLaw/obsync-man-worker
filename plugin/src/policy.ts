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
  if (p.default !== "include" && p.default !== "skip") return `bad default ${p.default}`;
  const seen = new Set<string>();
  for (const r of p.rules) {
    if (!r.name) return "a rule has no name";
    if (seen.has(r.name)) return `duplicate rule name ${r.name}`;
    seen.add(r.name);
    if (isEmptyMatch(r.match)) {
      return `rule ${r.name} matches everything, which is what default is for`;
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
 * Go's path.Match semantics, not full globbing: `*` does not cross `/`.
 * Matching Go exactly here is the whole point, since the golden fixture is
 * shared.
 */
export function globMatch(pattern: string, name: string): boolean {
  const rx = pattern
    .replace(/[.+^${}()|[\]\\]/g, "\\$&")
    .replace(/\*/g, "[^/]*")
    .replace(/\?/g, "[^/]");
  return new RegExp(`^${rx}$`).test(name);
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
