import { describe, it, expect } from "vitest";
import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";
import type { Plugin } from "obsidian";
import { Syncer, uniquify } from "./sync";
import { RemoteStore } from "./store";
import { Manifest, Entry, blobKey } from "./types";
import { LocalState, emptyState } from "./state";
import { Policy } from "./policy";

const OPEN: Policy = { version: 1, default: "include", rules: [] };

const FOLDERS = {
  targetFolder: "Canvas",
  trashFolder: "Canvas/_trash",
  conflictFolder: "Canvas/_conflicts",
};

const enc = (s: string) => new TextEncoder().encode(s);
const hashOf = (s: string) => bytesToHex(sha256(enc(s)));

/** In-memory DataAdapter: files as bytes, directories as a set. */
class FakeAdapter {
  files = new Map<string, Uint8Array>();
  dirs = new Set<string>();
  mtimes = new Map<string, number>();
  mkdirCalls: string[] = [];

  write(path: string, content: string, mtime = Date.now()) {
    this.files.set(path, enc(content));
    this.mtimes.set(path, mtime);
  }
  read(path: string): string {
    return new TextDecoder().decode(this.files.get(path));
  }

  exists(p: string) { return Promise.resolve(this.files.has(p) || this.dirs.has(p)); }
  stat(p: string) {
    const f = this.files.get(p);
    if (!f) return Promise.resolve(null);
    return Promise.resolve({ type: "file", size: f.byteLength, mtime: this.mtimes.get(p) ?? 0, ctime: 0 });
  }
  readBinary(p: string) {
    const f = this.files.get(p);
    if (!f) throw new Error(`no such file ${p}`);
    return Promise.resolve(f.buffer.slice(f.byteOffset, f.byteOffset + f.byteLength) as ArrayBuffer);
  }
  writeBinary(p: string, data: ArrayBuffer) {
    this.files.set(p, new Uint8Array(data));
    this.mtimes.set(p, Date.now());
    return Promise.resolve();
  }
  appendBinary(p: string, data: ArrayBuffer) {
    const prev = this.files.get(p) ?? new Uint8Array(0);
    const next = new Uint8Array(prev.byteLength + data.byteLength);
    next.set(prev, 0);
    next.set(new Uint8Array(data), prev.byteLength);
    this.files.set(p, next);
    return Promise.resolve();
  }
  remove(p: string) { this.files.delete(p); return Promise.resolve(); }
  rename(from: string, to: string) {
    const f = this.files.get(from);
    if (!f) throw new Error(`no such file ${from}`);
    // The real adapter refuses to clobber; make the fake just as strict so a
    // test cannot pass on behaviour Obsidian would reject.
    if (this.files.has(to)) throw new Error(`rename target exists: ${to}`);
    this.files.delete(from);
    this.files.set(to, f);
    this.mtimes.set(to, Date.now());
    return Promise.resolve();
  }
  mkdir(p: string) {
    this.mkdirCalls.push(p);
    // Mirrors a non-recursive mkdir: the parent must already exist.
    const parent = p.slice(0, p.lastIndexOf("/"));
    if (parent && !this.dirs.has(parent)) throw new Error(`parent missing: ${parent}`);
    this.dirs.add(p);
    return Promise.resolve();
  }
}

class FakePlugin {
  saved: unknown = null;
  manifest = { id: "obsync", dir: ".myconfig/plugins/obsync" };
  constructor(public adapter: FakeAdapter) {}
  get app() { return { vault: { adapter: this.adapter, configDir: ".myconfig" } }; }
  saveData(d: unknown) { this.saved = d; return Promise.resolve(); }
}

/** Serves blob bytes by hash; records what was requested. */
class FakeStore {
  gets: string[] = [];
  constructor(public blobs: Map<string, Uint8Array>) {}
  getBinary(key: string) {
    this.gets.push(key);
    const b = this.blobs.get(key);
    if (!b) throw new Error(`404 ${key}`);
    return Promise.resolve(b.buffer.slice(b.byteOffset, b.byteOffset + b.byteLength));
  }
  getRange(key: string, off: number, n: number) {
    this.gets.push(key);
    const b = this.blobs.get(key);
    if (!b) throw new Error(`404 ${key}`);
    const slice = b.slice(off, off + n);
    return Promise.resolve(slice.buffer.slice(slice.byteOffset, slice.byteOffset + slice.byteLength));
  }
}

function entry(path: string, content: string, over: Partial<Entry> = {}): Entry {
  return {
    path,
    state: "stored",
    size: enc(content).byteLength,
    canvas_id: 1,
    updated_at: "2026-01-01T00:00:00Z",
    modified_at: "2026-01-01T00:00:00Z",
    sha256: hashOf(content),
    ...over,
  };
}

function manifest(entries: Entry[], runId = "run-2"): Manifest {
  return {
    schema_version: 1,
    course_id: 42,
    course_name: "Test",
    run_id: runId,
    generated_at: "2026-01-01T00:00:00Z",
    rules_hash: "abc",
    entries,
  };
}

function build(contents: string[]) {
  const adapter = new FakeAdapter();
  adapter.dirs.add("Canvas");
  adapter.dirs.add(".myconfig");
  adapter.dirs.add(".myconfig/plugins");
  adapter.dirs.add(".myconfig/plugins/obsync");
  const blobs = new Map<string, Uint8Array>();
  for (const c of contents) blobs.set(blobKey(hashOf(c)), enc(c));
  const plugin = new FakePlugin(adapter);
  const store = new FakeStore(blobs);
  const state = emptyState();
  const make = (s: LocalState = state) =>
    new Syncer(plugin as unknown as Plugin, store as unknown as RemoteStore, OPEN, s, FOLDERS);
  return { adapter, plugin, store, state, make };
}

describe("writeEntry: downloading", () => {
  it("writes a new file and records its hash", async () => {
    const { adapter, state, make } = build(["hello"]);
    const res = await make().syncCourse(manifest([entry("a.pdf", "hello")]));

    expect(res.added).toBe(1);
    expect(res.conflicts).toEqual([]);
    expect(adapter.read("Canvas/a.pdf")).toBe("hello");
    expect(state.files["a.pdf"]?.sha256).toBe(hashOf("hello"));
  });

  it("creates nested parent directories one level at a time", async () => {
    const { adapter, make } = build(["slides"]);
    await make().syncCourse(manifest([entry("Week 1/Lecture 2/s.pdf", "slides")]));

    // A single non-recursive mkdir of the full path would have thrown.
    expect(adapter.read("Canvas/Week 1/Lecture 2/s.pdf")).toBe("slides");
    expect(adapter.mkdirCalls).toContain("Canvas/Week 1");
    expect(adapter.mkdirCalls).toContain("Canvas/Week 1/Lecture 2");
  });

  it("refuses bytes whose hash does not match the manifest", async () => {
    const { adapter, store, make } = build([]);
    const e = entry("a.pdf", "hello");
    // Store serves the wrong content under the expected key.
    store.blobs.set(blobKey(e.sha256!), enc("tampered"));

    const res = await make().syncCourse(manifest([e]));

    expect(res.added).toBe(0);
    expect(res.errors[0]).toMatch(/hash mismatch/);
    expect(adapter.files.has("Canvas/a.pdf")).toBe(false);
    // And no part file is left behind.
    expect([...adapter.files.keys()].filter((k) => k.includes(".parts"))).toEqual([]);
  });
});

describe("writeEntry: not destroying local data", () => {
  it("quarantines a file present on disk that we have NO record of", async () => {
    // The second-device case: the vault synced over, data.json did not.
    const { adapter, state, make } = build(["new content"]);
    adapter.write("Canvas/a.pdf", "the user's own work");

    const res = await make().syncCourse(manifest([entry("a.pdf", "new content")]));

    expect(res.conflicts).toEqual(["a.pdf"]);
    expect(adapter.read("Canvas/_conflicts/a.pdf")).toBe("the user's own work");
    // The user's bytes survived; nothing was overwritten in place.
    expect(adapter.files.has("Canvas/a.pdf")).toBe(false);
    expect(state.files["a.pdf"]).toBeUndefined();
  });

  it("quarantines a file edited since we wrote it", async () => {
    const { adapter, state, make } = build(["v2"]);
    adapter.write("Canvas/a.pdf", "edited by hand", Date.now());
    state.files["a.pdf"] = { sha256: hashOf("v1"), size: 2, writtenAt: 0 };

    const res = await make(state).syncCourse(manifest([entry("a.pdf", "v2")]));

    expect(res.conflicts).toEqual(["a.pdf"]);
    expect(adapter.read("Canvas/_conflicts/a.pdf")).toBe("edited by hand");
  });

  it("adopts a file already containing the right bytes instead of re-downloading", async () => {
    const { adapter, store, state, make } = build(["same"]);
    adapter.write("Canvas/a.pdf", "same");

    const res = await make().syncCourse(manifest([entry("a.pdf", "same")]));

    expect(res.adopted).toBe(1);
    expect(res.conflicts).toEqual([]);
    expect(store.gets).toEqual([]); // no download at all
    expect(state.files["a.pdf"]?.sha256).toBe(hashOf("same"));
  });

  it("overwrites cleanly when the file still matches what we wrote", async () => {
    const { adapter, state, make } = build(["v2"]);
    const written = Date.now();
    adapter.write("Canvas/a.pdf", "v1", written);
    state.files["a.pdf"] = { sha256: hashOf("v1"), size: 2, writtenAt: written };

    const res = await make(state).syncCourse(manifest([entry("a.pdf", "v2")]));

    expect(res.conflicts).toEqual([]);
    expect(res.updated).toBe(1);
    expect(adapter.read("Canvas/a.pdf")).toBe("v2");
  });

  it("does not collide when a second conflict arrives for the same path", async () => {
    const { adapter, make } = build(["v3"]);
    adapter.write("Canvas/a.pdf", "mine");
    adapter.write("Canvas/_conflicts/a.pdf", "an earlier conflict");

    const res = await make().syncCourse(manifest([entry("a.pdf", "v3")]));

    expect(res.conflicts).toEqual(["a.pdf"]);
    expect(adapter.read("Canvas/_conflicts/a.pdf")).toBe("an earlier conflict");
    const extra = [...adapter.files.keys()].filter((k) => /_conflicts\/a \(\d+\)\.pdf/.test(k));
    expect(extra).toHaveLength(1);
  });
});

describe("partial vs full pulls", () => {
  const two = () => manifest([entry("a.pdf", "aaa"), entry("b.pdf", "bbb")]);

  it("a full pull claims the run", async () => {
    const { state, make } = build(["aaa", "bbb"]);
    await make().syncCourse(two());
    expect(state.lastRunId["42"]).toBe("run-2");
  });

  it("a partial pull does NOT claim the run", async () => {
    // Otherwise pullAll's "nothing new since last look" guard skips the course
    // forever and the unselected files are stranded.
    const { adapter, state, make } = build(["aaa", "bbb"]);
    const res = await make().syncCourse(two(), new Set(["a.pdf"]));

    expect(res.added).toBe(1);
    expect(adapter.files.has("Canvas/a.pdf")).toBe(true);
    expect(adapter.files.has("Canvas/b.pdf")).toBe(false);
    expect(state.lastRunId["42"]).toBeUndefined();
  });

  it("a partial pull does not act on tombstones", async () => {
    const { adapter, state, make } = build(["aaa"]);
    adapter.write("Canvas/gone.pdf", "old");
    state.files["gone.pdf"] = { sha256: hashOf("old"), size: 3, writtenAt: Date.now() };

    const m = manifest([entry("a.pdf", "aaa"), entry("gone.pdf", "old", { state: "deleted" })]);
    const res = await make(state).syncCourse(m, new Set(["a.pdf"]));

    expect(res.removed).toBe(0);
    expect(adapter.files.has("Canvas/gone.pdf")).toBe(true);
  });
});

describe("tombstones", () => {
  it("moves a removed file to trash rather than deleting it", async () => {
    const { adapter, state, make } = build([]);
    adapter.write("Canvas/gone.pdf", "lecture");
    state.files["gone.pdf"] = { sha256: hashOf("lecture"), size: 7, writtenAt: Date.now() };

    const res = await make(state).syncCourse(
      manifest([entry("gone.pdf", "lecture", { state: "deleted" })]),
    );

    expect(res.removed).toBe(1);
    expect(adapter.read("Canvas/_trash/gone.pdf")).toBe("lecture");
    expect(adapter.files.has("Canvas/gone.pdf")).toBe(false);
    expect(state.files["gone.pdf"]).toBeUndefined();
  });

  it("survives a file the user already removed", async () => {
    const { state, make } = build([]);
    state.files["gone.pdf"] = { sha256: hashOf("x"), size: 1, writtenAt: Date.now() };

    const res = await make(state).syncCourse(
      manifest([entry("gone.pdf", "x", { state: "deleted" })]),
    );

    expect(res.errors).toEqual([]);
    expect(state.files["gone.pdf"]).toBeUndefined();
  });
});

describe("uniquify", () => {
  it("keeps the extension at the end", () => {
    expect(uniquify("a/b.pdf", 7)).toBe("a/b (7).pdf");
  });
  it("appends when there is no extension", () => {
    expect(uniquify("a/README", 7)).toBe("a/README (7)");
  });
  it("does not treat a dotfile's leading dot as an extension", () => {
    expect(uniquify("a/.env", 7)).toBe("a/.env (7)");
  });
});
