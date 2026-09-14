import { describe, it, expect } from "vitest";
import { buildTree, filesUnder, TreeFolder } from "./tree";

const paths = [
  "Lecture Notes/Lecture 10/L10.pdf",
  "Lecture Notes/Lecture 2/L2.pdf",
  "Lecture Notes/Lecture 2/L2-reference.pdf",
  "Labs/labs-intro.pdf",
  "syllabus.pdf",
  "Labs/2026-lab1.pdf",
  "Sample Codes/Python Samples/tcpclient.py",
];

const tree = () => buildTree(paths, (p) => p);
const names = (f: TreeFolder<string>) => ({
  folders: f.folders.map((x) => x.name),
  files: f.files.map((x) => x.name),
});

describe("buildTree", () => {
  it("nests files under their folders, folders before files", () => {
    expect(names(tree())).toEqual({
      folders: ["Labs", "Lecture Notes", "Sample Codes"],
      files: ["syllabus.pdf"],
    });
  });

  it("sorts numbers the way people read them", () => {
    const lectures = tree().folders.find((f) => f.name === "Lecture Notes")!;
    expect(lectures.folders.map((f) => f.name)).toEqual(["Lecture 2", "Lecture 10"]);
  });

  it("keeps full paths on folders and files", () => {
    const py = tree().folders.find((f) => f.name === "Sample Codes")!.folders[0]!;
    expect(py.path).toBe("Sample Codes/Python Samples");
    expect(py.files[0]).toMatchObject({ name: "tcpclient.py", path: "Sample Codes/Python Samples/tcpclient.py" });
  });

  it("creates each folder once however many files it holds", () => {
    const labs = tree().folders.filter((f) => f.name === "Labs");
    expect(labs).toHaveLength(1);
    expect(labs[0]!.files.map((f) => f.name)).toEqual(["2026-lab1.pdf", "labs-intro.pdf"]);
  });

  it("carries the original value", () => {
    const t = buildTree([{ path: "a/b.pdf", size: 7 }], (x) => x.path);
    expect(t.folders[0]!.files[0]!.value.size).toBe(7);
  });

  it("handles an empty list", () => {
    expect(names(buildTree([], (p: string) => p))).toEqual({ folders: [], files: [] });
  });
});

describe("filesUnder", () => {
  it("returns every file at any depth", () => {
    expect(filesUnder(tree()).map((f) => f.path).sort()).toEqual([...paths].sort());
    const lectures = tree().folders.find((f) => f.name === "Lecture Notes")!;
    expect(filesUnder(lectures)).toHaveLength(3);
  });
});
