package plan

import (
	"testing"
	"time"

	"github.com/leifsen/obsync/internal/manifest"
	"github.com/leifsen/obsync/internal/policy"
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
