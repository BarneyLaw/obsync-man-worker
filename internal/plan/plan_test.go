package plan

import (
	"strings"
	"testing"
	"time"

	"github.com/leifsen/obsync/internal/manifest"
	"github.com/leifsen/obsync/internal/policy"
	"github.com/leifsen/obsync/internal/scope"
)

var (
	t0  = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	now = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
)

func rules() *policy.Policy {
	return &policy.Policy{Version: 1, Default: policy.ActionInclude, Rules: []policy.Rule{
		{Name: "no-video", Priority: 10, Action: policy.ActionSkip,
			Match: policy.Match{Ext: []string{"mp4"}}},
	}}
}

func file(path string, id int64, size int64, updated time.Time) File {
	return File{Path: path, CanvasID: id, Size: size, UpdatedAt: updated, ModifiedAt: updated}
}

func stored(path string, id int64, size int64, updated time.Time) manifest.Entry {
	return manifest.Entry{
		Path: path, State: manifest.StateStored, CanvasID: id, Size: size,
		UpdatedAt: updated, ModifiedAt: updated, SHA256: "deadbeef",
	}
}

func prevManifest(rulesHash string, entries ...manifest.Entry) *manifest.Manifest {
	return &manifest.Manifest{
		SchemaVersion: manifest.SchemaVersion, RunID: "prev",
		RulesHash: rulesHash, Entries: entries,
	}
}

func TestFirstRunFetchesEverythingIncluded(t *testing.T) {
	p := Compute(Input{
		Files:  []File{file("a.pdf", 1, 100, t0), file("b.mp4", 2, 999, t0)},
		Policy: rules(), Now: now,
	})
	if len(p.Fetch) != 1 || p.Fetch[0].Path != "a.pdf" {
		t.Fatalf("fetch = %+v", p.Fetch)
	}
	if len(p.Skipped) != 1 || p.Skipped[0].RuleName != "no-video" {
		t.Fatalf("skipped = %+v", p.Skipped)
	}
	// The skipped file must still be catalogued, that is the whole point.
	if p.Skipped[0].Size != 999 {
		t.Fatal("skipped entries must retain metadata so the plugin can offer them")
	}
}

func TestUnchangedFileIsCarriedNotRefetched(t *testing.T) {
	r := rules()
	prev := prevManifest(r.Hash(), stored("a.pdf", 1, 100, t0))
	p := Compute(Input{Prev: prev, Files: []File{file("a.pdf", 1, 100, t0)}, Policy: r, Now: now})
	if len(p.Fetch) != 0 {
		t.Fatalf("should not refetch unchanged file: %+v", p.Fetch)
	}
	if len(p.Carry) != 1 {
		t.Fatalf("carry = %+v", p.Carry)
	}
}

func TestChangedTimestampTriggersFetch(t *testing.T) {
	r := rules()
	prev := prevManifest(r.Hash(), stored("a.pdf", 1, 100, t0))
	p := Compute(Input{Prev: prev, Files: []File{file("a.pdf", 1, 100, now)}, Policy: r, Now: now})
	if len(p.Fetch) != 1 {
		t.Fatalf("timestamp move must trigger a fetch: %+v", p)
	}
}

// The single most likely source of duplicate entries. Lecturers delete and
// re-upload rather than replacing, which mints a new Canvas id for the same
// logical path.
func TestResurrectionIsNotADuplicate(t *testing.T) {
	r := rules()
	prev := prevManifest(r.Hash(), stored("Week 1/slides.pdf", 111, 100, t0))
	p := Compute(Input{
		Prev:   prev,
		Files:  []File{file("Week 1/slides.pdf", 222, 140, now)}, // new id, same path
		Policy: r, Now: now,
	})
	if len(p.Fetch) != 1 {
		t.Fatalf("expected one fetch, got %+v", p.Fetch)
	}
	if len(p.Tombstone) != 0 {
		t.Fatalf("must not tombstone a path that is still present: %+v", p.Tombstone)
	}
	if len(p.Carry) != 0 {
		t.Fatalf("must not carry the stale entry alongside the fetch: %+v", p.Carry)
	}
}

func TestVanishedFileIsTombstoned(t *testing.T) {
	r := rules()
	prev := prevManifest(r.Hash(), stored("gone.pdf", 1, 100, t0))
	p := Compute(Input{Prev: prev, Files: nil, Policy: r, Now: now})
	if len(p.Tombstone) != 1 {
		t.Fatalf("tombstone = %+v", p.Tombstone)
	}
	tb := p.Tombstone[0]
	if tb.State != manifest.StateDeleted || tb.DeletedAt == nil {
		t.Fatalf("bad tombstone %+v", tb)
	}
	if tb.SHA256 != "" {
		t.Fatal("a tombstone must not carry a hash, Validate rejects it")
	}
}

// Re-stamping DeletedAt every run would make every manifest differ from the
// last forever, which destroys the value of diffing them.
func TestAlreadyTombstonedIsStable(t *testing.T) {
	r := rules()
	at := t0
	prev := prevManifest(r.Hash(), manifest.Entry{
		Path: "gone.pdf", State: manifest.StateDeleted, DeletedAt: &at, Reason: "gone",
	})
	p := Compute(Input{Prev: prev, Files: nil, Policy: r, Now: now})
	if len(p.Tombstone) != 0 {
		t.Fatal("should not re-tombstone")
	}
	if len(p.Carry) != 1 || !p.Carry[0].DeletedAt.Equal(t0) {
		t.Fatalf("DeletedAt must not be re-stamped: %+v", p.Carry)
	}
}

func TestSkippedIsCarriedWhenRulesUnchanged(t *testing.T) {
	r := rules()
	prev := prevManifest(r.Hash(), manifest.Entry{
		Path: "a.mp4", State: manifest.StateSkipped, CanvasID: 1, Size: 999,
		UpdatedAt: t0, ModifiedAt: t0, RuleName: "no-video", Reason: "rule",
	})
	p := Compute(Input{Prev: prev, Files: []File{file("a.mp4", 1, 999, t0)}, Policy: r, Now: now})
	if len(p.Fetch) != 0 {
		t.Fatalf("skipped file re-evaluated for no reason: %+v", p.Fetch)
	}
}

func TestOverrideBeatsRule(t *testing.T) {
	r := rules()
	p := Compute(Input{
		Files:     []File{file("a.mp4", 1, 999, t0)},
		Policy:    r,
		Overrides: Overrides{Include: map[string]bool{"a.mp4": true}},
		Now:       now,
	})
	if len(p.Fetch) != 1 {
		t.Fatalf("user override must beat the rule set: %+v", p)
	}
}

func TestLockedIsNotAFailure(t *testing.T) {
	r := rules()
	unlock := now.Add(48 * time.Hour)
	f := file("a.pdf", 1, 100, t0)
	f.Locked, f.UnlockAt = true, &unlock
	p := Compute(Input{Files: []File{f}, Policy: r, Now: now})
	if len(p.Locked) != 1 || len(p.Fetch) != 0 {
		t.Fatalf("locked file must not be fetched or treated as failed: %+v", p)
	}
}

func TestCollidingPathsAreDisambiguatedDeterministically(t *testing.T) {
	r := rules()
	in := Input{
		Files: []File{
			file("Q1- what.pdf", 100, 10, t0),
			file("Q1- what.pdf", 200, 20, t0),
		},
		Policy: r, Now: now,
	}
	a := Compute(in)
	// Reverse the listing order: Canvas pagination order is not guaranteed.
	in.Files[0], in.Files[1] = in.Files[1], in.Files[0]
	b := Compute(in)

	pathsOf := func(p Plan) map[string]bool {
		m := map[string]bool{}
		for _, f := range p.Fetch {
			m[f.Path] = true
		}
		return m
	}
	pa, pb := pathsOf(a), pathsOf(b)
	if len(pa) != 2 {
		t.Fatalf("collision not resolved: %v", pa)
	}
	for k := range pa {
		if !pb[k] {
			t.Fatalf("path set depends on listing order: %v vs %v", pa, pb)
		}
	}
}

func sizeCapRules() *policy.Policy {
	return &policy.Policy{Version: 1, Default: policy.ActionInclude, Rules: []policy.Rule{
		{Name: "cap", Priority: 10, Action: policy.ActionSkip, Match: policy.Match{MinSize: 1000}},
	}}
}

// The old differ carried a skip forward whenever the rules hash was unchanged,
// so a file re-uploaded under the size cap stayed skipped forever.
func TestSkippedFileIsReevaluatedWhenItShrinks(t *testing.T) {
	r := sizeCapRules()
	prev := prevManifest(r.Hash(), manifest.Entry{
		Path: "data.zip", State: manifest.StateSkipped, CanvasID: 1, Size: 5000,
		UpdatedAt: t0, ModifiedAt: t0, RuleName: "cap", Reason: "too big",
	})
	p := Compute(Input{Prev: prev, Files: []File{file("data.zip", 1, 50, now)}, Policy: r, Now: now})
	if len(p.Fetch) != 1 {
		t.Fatalf("shrunken file must be fetched: %+v", p)
	}
	if p.Fetch[0].Why != "previously skipped, now included" {
		t.Fatalf("why = %q", p.Fetch[0].Why)
	}
}

// course_ids rules never matched in the worker because CourseID was not passed.
func TestCourseScopedRuleApplies(t *testing.T) {
	r := &policy.Policy{Version: 1, Default: policy.ActionInclude, Rules: []policy.Rule{
		{Name: "only-101", Priority: 1, Action: policy.ActionSkip,
			Match: policy.Match{Ext: []string{"pdf"}, CourseIDs: []int64{101}}},
	}}
	in := Input{CourseID: 101, Files: []File{file("a.pdf", 1, 10, t0)}, Policy: r, Now: now}
	if p := Compute(in); len(p.Skipped) != 1 {
		t.Fatalf("rule scoped to course 101 did not apply in course 101: %+v", p)
	}
	in.CourseID = 202
	if p := Compute(in); len(p.Fetch) != 1 {
		t.Fatalf("rule scoped to course 101 applied in course 202: %+v", p)
	}
}

// "Slides.pdf" and "slides.pdf" are one file on Windows and macOS.
func TestCaseOnlyCollisionIsDisambiguated(t *testing.T) {
	p := Compute(Input{
		Files:  []File{file("Week 1/Slides.pdf", 1, 10, t0), file("week 1/slides.pdf", 2, 10, t0)},
		Policy: rules(), Now: now,
	})
	if len(p.Fetch) != 2 {
		t.Fatalf("fetch = %+v", p.Fetch)
	}
	a, b := p.Fetch[0].Path, p.Fetch[1].Path
	if strings.EqualFold(a, b) {
		t.Fatalf("case-only collision survived: %q vs %q", a, b)
	}
}

// A suffixed path must not land on a real file of the same name.
func TestDisambiguationAvoidsSecondaryCollision(t *testing.T) {
	p := Compute(Input{
		Files: []File{
			file("a.pdf", 5, 10, t0),
			file("a.pdf", 7, 10, t0),
			file("a (5).pdf", 9, 10, t0),
		},
		Policy: rules(), Now: now,
	})
	seen := map[string]bool{}
	for _, f := range p.Fetch {
		k := strings.ToLower(f.Path)
		if seen[k] {
			t.Fatalf("duplicate path %q in %+v", f.Path, p.Fetch)
		}
		seen[k] = true
	}
}

// Pagination over a changing collection can return the same file twice.
func TestDuplicateCanvasIDIsDeduped(t *testing.T) {
	p := Compute(Input{
		Files:  []File{file("a.pdf", 1, 10, t0), file("a.pdf", 1, 10, now)},
		Policy: rules(), Now: now,
	})
	if len(p.Fetch) != 1 || len(p.Duplicates) != 1 {
		t.Fatalf("fetch=%+v dups=%v", p.Fetch, p.Duplicates)
	}
	if !p.Fetch[0].UpdatedAt.Equal(now) {
		t.Fatal("dedupe must keep the most recently updated copy")
	}
}

func TestFetchRecordsWhyAndPrev(t *testing.T) {
	r := rules()
	prev := prevManifest(r.Hash(), stored("a.pdf", 1, 100, t0))
	p := Compute(Input{Prev: prev, Files: []File{file("a.pdf", 1, 120, t0)}, Policy: r, Now: now})
	if len(p.Fetch) != 1 {
		t.Fatalf("fetch = %+v", p.Fetch)
	}
	f := p.Fetch[0]
	if f.Prev == nil || f.Prev.SHA256 != "deadbeef" {
		t.Fatal("fetch must carry the previous entry so a failed download can fall back to it")
	}
	if !strings.Contains(f.Why, "size 100 -> 120") {
		t.Fatalf("why = %q", f.Why)
	}
}

func mustScope(t *testing.T, patterns ...string) scope.Scope {
	t.Helper()
	s, err := scope.Parse(patterns)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestScopeFetchesOnlySelected(t *testing.T) {
	p := Compute(Input{
		Files:  []File{file("Week 1/a.pdf", 1, 10, t0), file("Week 2/b.pdf", 2, 10, t0)},
		Policy: rules(), Scope: mustScope(t, "Week 1"), Now: now,
	})
	if len(p.Fetch) != 1 || p.Fetch[0].Path != "Week 1/a.pdf" {
		t.Fatalf("fetch = %+v", p.Fetch)
	}
	if len(p.OutOfScope) != 1 {
		t.Fatalf("out of scope = %+v", p.OutOfScope)
	}
	e := p.OutOfScope[0]
	if e.State != manifest.StateSkipped || e.RuleName != ScopeRule || e.Reason == "" {
		t.Fatalf("a never-fetched out-of-scope file must be catalogued as a scope skip: %+v", e)
	}
}

// A scoped pull must never make a file the consumer already has disappear,
// even when Canvas has a newer version of it.
func TestScopeKeepsPreviousVersionOutsideScope(t *testing.T) {
	r := rules()
	prev := prevManifest(r.Hash(), stored("Week 2/b.pdf", 2, 10, t0))
	p := Compute(Input{
		Prev: prev, Files: []File{file("Week 2/b.pdf", 2, 99, now)},
		Policy: r, Scope: mustScope(t, "Week 1"), Now: now,
	})
	if len(p.Fetch) != 0 || len(p.OutOfScope) != 1 {
		t.Fatalf("plan = %+v", p)
	}
	kept := p.OutOfScope[0]
	if kept.State != manifest.StateStored || kept.SHA256 != "deadbeef" || kept.Size != 10 {
		t.Fatalf("previous entry must be kept verbatim so the next full pull still sees the change: %+v", kept)
	}
}

// Scope limits downloads, not the catalogue: skips and tombstones stay complete.
func TestScopeDoesNotHideSkipsOrTombstones(t *testing.T) {
	r := rules()
	prev := prevManifest(r.Hash(), stored("Week 2/gone.pdf", 3, 10, t0))
	p := Compute(Input{
		Prev: prev, Files: []File{file("Week 2/v.mp4", 4, 10, t0)},
		Policy: r, Scope: mustScope(t, "Week 1"), Now: now,
	})
	if len(p.Skipped) != 1 || p.Skipped[0].RuleName != "no-video" {
		t.Fatalf("skipped = %+v", p.Skipped)
	}
	if len(p.Tombstone) != 1 {
		t.Fatalf("tombstone = %+v", p.Tombstone)
	}
}

func TestScopeDeferredFileIsFetchedByNextFullPull(t *testing.T) {
	r := rules()
	prev := prevManifest(r.Hash(), manifest.Entry{
		Path: "Week 2/b.pdf", State: manifest.StateSkipped, CanvasID: 2, Size: 10,
		UpdatedAt: t0, ModifiedAt: t0, RuleName: ScopeRule, Reason: "deferred",
	})
	p := Compute(Input{Prev: prev, Files: []File{file("Week 2/b.pdf", 2, 10, t0)}, Policy: r, Now: now})
	if len(p.Fetch) != 1 || p.Fetch[0].Why != "deferred by an earlier scoped pull" {
		t.Fatalf("plan = %+v", p)
	}
}

func TestPlanPathsExcludesTombstones(t *testing.T) {
	r := rules()
	prev := prevManifest(r.Hash(), stored("gone.pdf", 1, 10, t0))
	p := Compute(Input{Prev: prev, Files: []File{file("a.pdf", 2, 10, t0)}, Policy: r, Now: now})
	got := p.Paths()
	if len(got) != 1 || got[0] != "a.pdf" {
		t.Fatalf("paths = %v", got)
	}
}
