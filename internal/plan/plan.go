// Package plan is the differ: previous manifest plus current Canvas listing
// plus rules produces a plan of what to do this run.
//
// Pure: no I/O, no clock (Now is injected). This is where the bugs will be, so
// it is where the tests are.
package plan

import (
	"time"

	"github.com/leifsen/obsync/internal/manifest"
	"github.com/leifsen/obsync/internal/policy"
	"github.com/leifsen/obsync/internal/portable"
)

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

type Plan struct {
	// Fetch needs bytes pulled from Canvas this run.
	Fetch []File
	// Carry are entries copied forward from the previous manifest unchanged.
	Carry []manifest.Entry
	// Skipped were catalogued and deliberately not fetched.
	Skipped []manifest.Entry
	// Locked are visible in Canvas but not yet downloadable.
	Locked []manifest.Entry
	// Tombstone were live before and are absent from Canvas now.
	Tombstone []manifest.Entry
}

type Input struct {
	Prev      *manifest.Manifest // nil on first run
	Files     []File
	Policy    *policy.Policy
	Overrides Overrides
	Now       time.Time
}

// Compute is the whole brain of the worker.
func Compute(in Input) Plan {
	var p Plan

	prevByPath := map[string]manifest.Entry{}
	rulesChanged := true
	if in.Prev != nil {
		prevByPath = in.Prev.ByPath()
		rulesChanged = in.Prev.RulesHash != in.Policy.Hash()
	}

	// Resolve path collisions deterministically before anything else, so the
	// same Canvas listing always yields the same paths.
	files := disambiguate(in.Files)

	seen := make(map[string]bool, len(files))

	for _, f := range files {
		seen[f.Path] = true
		prev, hadPrev := prevByPath[f.Path]

		// Locked wins over everything: there are no bytes to fetch yet.
		if f.Locked {
			p.Locked = append(p.Locked, lockedEntry(f))
			continue
		}

		action := policy.ActionInclude
		rule, reason := "", ""
		if a, ok := in.Overrides.decide(f.Path); ok {
			action, rule, reason = a, "", "user override"
		} else {
			d := in.Policy.Evaluate(policy.Candidate{
				Path: f.Path, Size: f.Size, MIME: f.MIME,
			})
			action, rule, reason = d.Action, d.Rule, d.Reason
		}

		if action == policy.ActionSkip {
			p.Skipped = append(p.Skipped, skippedEntry(f, rule, reason))
			continue
		}

		// Included. Do we already have the bytes?
		//
		// A path that comes back with a DIFFERENT Canvas id is a resurrection,
		// not a new file: lecturers delete and re-upload constantly instead of
		// replacing. Treat it as changed content on the same logical path,
		// never as a second entry.
		if hadPrev &&
			prev.State == manifest.StateStored &&
			prev.CanvasID == f.CanvasID &&
			prev.Size == f.Size &&
			prev.UpdatedAt.Equal(f.UpdatedAt) &&
			prev.ModifiedAt.Equal(f.ModifiedAt) {
			p.Carry = append(p.Carry, prev)
			continue
		}

		// Previously skipped and the rules have not changed and no override
		// touched it: leave the decision alone rather than silently rewriting
		// history on every run.
		if hadPrev && prev.State == manifest.StateSkipped && !rulesChanged {
			if _, overridden := in.Overrides.decide(f.Path); !overridden {
				p.Carry = append(p.Carry, prev)
				continue
			}
		}

		p.Fetch = append(p.Fetch, f)
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
			at := in.Now
			t.DeletedAt = &at
			t.Reason = "no longer present in Canvas"
			p.Tombstone = append(p.Tombstone, t)
		}
	}

	return p
}

// disambiguate resolves paths that collide after sanitisation. The suffix comes
// from the Canvas file id, never from iteration order, so the result is stable
// across runs.
func disambiguate(files []File) []File {
	counts := map[string]int{}
	for _, f := range files {
		counts[f.Path]++
	}
	out := make([]File, 0, len(files))
	for _, f := range files {
		if counts[f.Path] > 1 {
			f.Path = portable.Disambiguate(f.Path, f.CanvasID)
		}
		out = append(out, f)
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
