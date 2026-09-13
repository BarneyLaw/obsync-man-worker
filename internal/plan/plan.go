// Package plan is the differ: previous manifest plus current Canvas listing
// plus rules produces a plan of what to do this run.
//
// Pure: no I/O, no clock (Now is injected), no logging. Every decision carries
// its reason as data so the runner can log it; that is what makes a run
// auditable without this package knowing about loggers. This is where the bugs
// will be, so it is where the tests are.
package plan

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/leifsen/obsync/internal/manifest"
	"github.com/leifsen/obsync/internal/policy"
	"github.com/leifsen/obsync/internal/portable"
	"github.com/leifsen/obsync/internal/scope"
)

// ScopeRule is the RuleName on entries a scoped manual pull deferred. The
// colon keeps it out of the namespace of user-written rule names.
const ScopeRule = "obsync:pull-scope"

// File is the differ's view of a Canvas file, already path-sanitised.
type File struct {
	Path       string
	CanvasID   int64
	CanvasUUID string
	Size       int64
	MIME       string
	UpdatedAt  time.Time
	ModifiedAt time.Time
	Locked     bool
	UnlockAt   *time.Time
	// Note records anything the path builder had to do that a human should
	// know, e.g. a stand-in name for an unportable filename.
	Note string
}

// Overrides are per-path user requests that beat the rule set. Populated from
// the request bucket (or a ConfigMap) and folded in before evaluation.
type Overrides struct {
	Include map[string]bool
	Exclude map[string]bool
}

func (o Overrides) decide(p string) (policy.Action, bool) {
	if o.Exclude[p] {
		return policy.ActionSkip, true
	}
	if o.Include[p] {
		return policy.ActionInclude, true
	}
	return "", false
}

// Fetch is a file whose bytes must be pulled this run.
type Fetch struct {
	File
	// Why is the audit trail for this download.
	Why string
	// Prev is the previous entry at this path, if any. The runner falls back
	// to it when the download fails, so a transient error never hides a file
	// the consumer already has.
	Prev *manifest.Entry
}

type Plan struct {
	// Fetch needs bytes pulled from Canvas this run.
	Fetch []Fetch
	// Carry are entries copied forward from the previous manifest unchanged.
	Carry []manifest.Entry
	// Skipped were catalogued and deliberately not fetched.
	Skipped []manifest.Entry
	// Locked are visible in Canvas but not yet downloadable.
	Locked []manifest.Entry
	// Tombstone were live before and are absent from Canvas now.
	Tombstone []manifest.Entry
	// OutOfScope would have been fetched, but a scoped manual pull excluded
	// them. Each is either the previous entry kept verbatim or, for a file
	// never fetched before, a skipped entry with RuleName ScopeRule.
	OutOfScope []manifest.Entry
	// Duplicates are Canvas ids that appeared more than once in the listing.
	// Pagination over a changing collection can repeat items.
	Duplicates []int64
}

type Input struct {
	CourseID  int64
	Prev      *manifest.Manifest // nil on first run
	Files     []File
	Policy    *policy.Policy
	Overrides Overrides
	// Scope restricts which files a manual pull downloads. The zero value
	// selects everything. Scope never changes what is catalogued: skips,
	// locks and tombstones come from metadata and are always complete.
	Scope scope.Scope
	Now   time.Time
}

// Compute is the whole brain of the worker.
func Compute(in Input) Plan {
	var p Plan

	prevByPath := map[string]manifest.Entry{}
	if in.Prev != nil {
		prevByPath = in.Prev.ByPath()
	}

	files, dups := dedupe(in.Files)
	p.Duplicates = dups
	// Resolve path collisions deterministically before anything else, so the
	// same Canvas listing always yields the same paths.
	files = disambiguate(files)

	seen := make(map[string]bool, len(files))

	for _, f := range files {
		seen[f.Path] = true
		prev, hadPrev := prevByPath[f.Path]

		// Locked wins over everything: there are no bytes to fetch yet.
		if f.Locked {
			p.Locked = append(p.Locked, lockedEntry(f))
			continue
		}

		// Always evaluate. Evaluation is cheap and pure, and carrying last
		// run's skip forward is how a file that shrank under the size cap
		// stayed skipped forever.
		var d policy.Decision
		if a, ok := in.Overrides.decide(f.Path); ok {
			d = policy.Decision{Action: a, Reason: "user override"}
		} else {
			d = in.Policy.Evaluate(policy.Candidate{
				Path: f.Path, Size: f.Size, MIME: f.MIME, CourseID: in.CourseID,
			})
		}
		if d.Action == policy.ActionSkip {
			p.Skipped = append(p.Skipped, skippedEntry(f, d.Rule, d.Reason))
			continue
		}

		// Included. Do we already have the bytes?
		//
		// A path that comes back with a DIFFERENT Canvas id is a resurrection,
		// not a new file: lecturers delete and re-upload constantly instead of
		// replacing. Treat it as changed content on the same logical path,
		// never as a second entry.
		if hadPrev && prev.State == manifest.StateStored && unchanged(prev, f) {
			p.Carry = append(p.Carry, prev)
			continue
		}

		if !in.Scope.Contains(f.Path) {
			p.OutOfScope = append(p.OutOfScope, outOfScope(f, prev, hadPrev))
			continue
		}

		fe := Fetch{File: f, Why: why(f, prev, hadPrev)}
		if hadPrev {
			pe := prev
			fe.Prev = &pe
		}
		p.Fetch = append(p.Fetch, fe)
	}

	// Anything live in the previous manifest and absent from Canvas now.
	if in.Prev != nil {
		for _, e := range in.Prev.Entries {
			if seen[e.Path] {
				continue
			}
			if e.State == manifest.StateDeleted {
				// Already tombstoned and still gone. Carry it forward
				// unchanged rather than re-stamping DeletedAt every run.
				p.Carry = append(p.Carry, e)
				continue
			}
			t := e
			t.State = manifest.StateDeleted
			t.SHA256 = "" // Validate refuses a hash on a non-stored entry
			t.RuleName = ""
			at := in.Now
			t.DeletedAt = &at
			t.Reason = fmt.Sprintf("no longer present in Canvas (was %s)", e.State)
			p.Tombstone = append(p.Tombstone, t)
		}
	}

	return p
}

// Paths returns every path the plan catalogues, excluding tombstones. Used to
// check scope patterns against what actually exists.
func (p Plan) Paths() []string {
	var out []string
	for _, f := range p.Fetch {
		out = append(out, f.Path)
	}
	for _, group := range [][]manifest.Entry{p.Carry, p.Skipped, p.Locked, p.OutOfScope} {
		for _, e := range group {
			if e.State != manifest.StateDeleted {
				out = append(out, e.Path)
			}
		}
	}
	return out
}

func unchanged(prev manifest.Entry, f File) bool {
	return prev.CanvasID == f.CanvasID &&
		prev.Size == f.Size &&
		prev.UpdatedAt.Equal(f.UpdatedAt) &&
		prev.ModifiedAt.Equal(f.ModifiedAt)
}

// outOfScope keeps what the consumer already has. A scoped pull must never
// make a file disappear, so a stored (or failed) previous entry is carried
// verbatim, stale metadata included, which guarantees the next full pull still
// sees the change and fetches it.
func outOfScope(f File, prev manifest.Entry, hadPrev bool) manifest.Entry {
	if hadPrev && (prev.State == manifest.StateStored || prev.State == manifest.StateFailed) {
		return prev
	}
	return skippedEntry(f, ScopeRule, "outside the scope of a manual pull; the next full pull fetches it")
}

func why(f File, prev manifest.Entry, hadPrev bool) string {
	if !hadPrev {
		return "new file"
	}
	switch prev.State {
	case manifest.StateStored:
		if prev.CanvasID != f.CanvasID {
			return fmt.Sprintf("re-uploaded in Canvas (canvas id %d -> %d)", prev.CanvasID, f.CanvasID)
		}
		var changed []string
		if prev.Size != f.Size {
			changed = append(changed, fmt.Sprintf("size %d -> %d", prev.Size, f.Size))
		}
		if !prev.UpdatedAt.Equal(f.UpdatedAt) {
			changed = append(changed, fmt.Sprintf("updated_at %s -> %s",
				prev.UpdatedAt.Format(time.RFC3339), f.UpdatedAt.Format(time.RFC3339)))
		}
		if !prev.ModifiedAt.Equal(f.ModifiedAt) {
			changed = append(changed, fmt.Sprintf("modified_at %s -> %s",
				prev.ModifiedAt.Format(time.RFC3339), f.ModifiedAt.Format(time.RFC3339)))
		}
		return "changed in Canvas: " + strings.Join(changed, ", ")
	case manifest.StateFailed:
		return "retrying previous failure: " + prev.Reason
	case manifest.StateSkipped:
		if prev.RuleName == ScopeRule {
			return "deferred by an earlier scoped pull"
		}
		return "previously skipped, now included"
	case manifest.StateLocked:
		return "unlocked in Canvas"
	case manifest.StateDeleted:
		return "reappeared in Canvas after deletion"
	}
	return "previous state " + string(prev.State)
}

// dedupe drops repeated Canvas ids. Of two copies, the most recently updated
// wins, then the larger, so the choice does not depend on listing order.
func dedupe(files []File) ([]File, []int64) {
	idx := make(map[int64]int, len(files))
	out := make([]File, 0, len(files))
	var dups []int64
	for _, f := range files {
		i, ok := idx[f.CanvasID]
		if !ok {
			idx[f.CanvasID] = len(out)
			out = append(out, f)
			continue
		}
		dups = append(dups, f.CanvasID)
		cur := out[i]
		if f.UpdatedAt.After(cur.UpdatedAt) || (f.UpdatedAt.Equal(cur.UpdatedAt) && f.Size > cur.Size) {
			out[i] = f
		}
	}
	sort.Slice(dups, func(a, b int) bool { return dups[a] < dups[b] })
	return out, dups
}

// disambiguate resolves paths that collide after sanitisation, including
// case-only collisions, which are one file on Windows and macOS. The suffix
// comes from the Canvas file id, never from iteration order, so the result is
// stable across runs. It repeats in case a suffixed path lands on a real one.
func disambiguate(files []File) []File {
	out := append([]File(nil), files...)
	for round := 0; round < 4; round++ {
		counts := map[string]int{}
		for _, f := range out {
			counts[portable.CollisionKey(f.Path)]++
		}
		collided := false
		for i := range out {
			if counts[portable.CollisionKey(out[i].Path)] > 1 {
				out[i].Path = portable.Disambiguate(out[i].Path, out[i].CanvasID)
				collided = true
			}
		}
		if !collided {
			break
		}
	}
	return out
}

func skippedEntry(f File, rule, reason string) manifest.Entry {
	return manifest.Entry{
		Path: f.Path, State: manifest.StateSkipped,
		Size: f.Size, MIME: f.MIME,
		CanvasID: f.CanvasID, CanvasUUID: f.CanvasUUID,
		UpdatedAt: f.UpdatedAt, ModifiedAt: f.ModifiedAt,
		RuleName: rule, Reason: reason,
	}
}

func lockedEntry(f File) manifest.Entry {
	return manifest.Entry{
		Path: f.Path, State: manifest.StateLocked,
		Size: f.Size, MIME: f.MIME,
		CanvasID: f.CanvasID, CanvasUUID: f.CanvasUUID,
		UpdatedAt: f.UpdatedAt, ModifiedAt: f.ModifiedAt,
		UnlockAt: f.UnlockAt,
		Reason:   "locked in Canvas",
	}
}
